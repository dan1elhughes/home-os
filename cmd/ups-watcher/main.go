// Command ups-watcher guards a swarm node against a UPS battery event.
//
//	watch    poll the NUT server and run the decision machine; execute
//	         drain/un-drain/shutdown decisions (daemon, never exits)
//	undrain  boot-time release: wait until NUT reports OL, then set the
//	         swarm node back to availability=active and clear the flag
//
// All configuration is environment-driven (see internal/ups.LoadConfig).
// Only decisions the machine emits are executed here; the machine itself
// is pure logic in internal/ups.
package main

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/dan1elhughes/home-os/internal/ups"
)

const (
	pollInterval  = 10 * time.Second
	drainDeadline = 600 * time.Second
	drainPoll     = 15 * time.Second
	dialTimeout   = 3 * time.Second

	defaultFlagDir = "/var/lib/ups-watcher"
	defaultLogPath = "/var/log/ups-watcher.log"
)

// flagDir is UPS_FLAG_DIR or the default; overridable so the daemon can
// run anywhere (smoke tests, containers) without recompiling.
func flagDir() string {
	if d := os.Getenv("UPS_FLAG_DIR"); d != "" {
		return d
	}
	return defaultFlagDir
}

func flagFile() string { return flagDir() + "/drained" }

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "watch":
		runWatch()
	case "undrain":
		runUndrain()
	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: ups-watcher <watch|undrain>")
}

func nodeID() string {
	h, err := os.Hostname()
	if err != nil {
		return "unknown"
	}
	return h
}

func newLogger() *ups.Logger {
	path := os.Getenv("UPS_LOG")
	if path == "" {
		path = "/var/log/ups-watcher.log"
	}
	l, err := ups.OpenLogger(path, nodeID())
	if err != nil {
		// The daemon must keep working without its file log: fall back
		// to a stdout-only logger (journald still records the events).
		l = ups.OpenLoggerStdout(nodeID())
	}
	return l
}

func dial(cfg ups.DaemonConfig) (*ups.Client, error) {
	return ups.Dial(cfg.UPSHost, cfg.UPSUser, cfg.UPSPass, cfg.UPSName, dialTimeout)
}

// probe fetches one NUT status. TEST_OB=1 feeds the machine a synthetic
// on-battery status instead (Phase-2 rehearsal); production never sets
// it.
func probe(cfg ups.DaemonConfig) (string, bool) {
	c, err := dial(cfg)
	if err != nil {
		return "", false
	}
	defer c.Close()
	st, err := c.Status()
	if err != nil {
		return "", false
	}
	if os.Getenv("UPS_TEST_OB") == "1" {
		return "OB DISCHRG", true
	}
	return st, true
}

// feedStatus runs one already-fetched probe through the machine, logging
// any machine event (one line) it fires.
func feedStatus(m *ups.Machine, log *ups.Logger, status string, ok bool, suffix string) ups.Decision {
	d, ev := m.OnProbe(ok, status, time.Now().Unix())
	if ev != ups.EventNone {
		log.Log(ups.LogName(ev), detailFor(ev, status)+suffix)
	}
	return d
}

// feed fetches one NUT probe and runs it through the machine, logging
// any machine event (one line) it fires.
func feed(m *ups.Machine, cfg ups.DaemonConfig, log *ups.Logger, suffix string) ups.Decision {
	status, ok := probe(cfg)
	return feedStatus(m, log, status, ok, suffix)
}

func detailFor(ev ups.Event, status string) string {
	switch ev {
	case ups.EvOnBattery:
		return "ups.status=" + status
	case ups.EvBackOnMains:
		return "ups.status=" + status
	case ups.EvWatchdog:
		return "NUT endpoint silent past watchdog window"
	case ups.EvShutdown:
		return "UPService reached low battery; status=" + status
	}
	return ""
}

func runWatch() {
	cfg, err := ups.LoadConfig(ups.EnvOS())
	if err != nil {
		fmt.Fprintln(os.Stderr, "config:", err)
		os.Exit(1)
	}
	log := newLogger()
	testOB := os.Getenv("UPS_TEST_OB") == "1"
	log.Log("START", fmt.Sprintf("role=%s ups=%s confirm=%d watchdog=%ds testob=%v",
		cfg.Role, cfg.UPSHost, cfg.ConfirmOB, cfg.SilentSecs, testOB))

	m := ups.NewMachine(cfg.Config)
	// Test mode: TEST_OB simulates an on-battery status through the
	// whole flow (drain executes for real), and poweroff is suppressed
	// — the node un-drains instead. Production never sets it.
	suppressPoweroff := testOB

	// Startup status: the runbook greps journalctl for an OL confirmation.
	if status, ok := probe(cfg); ok {
		log.Log("STATUS", "ups.status="+status)
	}
	for {
		d := feed(m, cfg, log, "")
		switch d.Action {
		case ups.ActionDrain:
			runDrain(m, cfg, log, suppressPoweroff)
		case ups.ActionUndrain:
			undrainNow(log)
		case ups.ActionShutdown:
			shutdownNow(m, cfg, log, d)
		}
		time.Sleep(pollInterval)
	}
}

// runDrain is the synchronous wait after BeginDrain: set the flag, drain
// the node in swarm, then poll until the survivor runs every service
// that was local (or the hard deadline, or mains is back) and only then
// power off. Local container teardown alone is NOT sufficient: Swarm
// stops the local tasks before the replacements reach Running, and a
// power-off in that gap removes raft quorum mid-reschedule, stranding
// the workload.
func runDrain(m *ups.Machine, cfg ups.DaemonConfig, log *ups.Logger, suppressPoweroff bool) {
	log.Log("OB_CONFIRMED", fmt.Sprintf("%d consecutive OB probes; draining", cfg.ConfirmOB))

	// Snapshot BEFORE the drain: this is the work that must land
	// somewhere running before the node may power off.
	survivor := survivorNode()
	services := localTaskSnapshot(log)

	log.Log("DRAIN_START", fmt.Sprintf("docker node update --availability drain %s; local=%s",
		nodeID(), strings.Join(services, ",")))
	if err := os.MkdirAll(flagDir(), 0o755); err != nil {
		log.Log("DRAIN_ERROR", "mkdir flag dir: "+err.Error())
	} else if err := os.WriteFile(flagFile(), []byte(time.Now().UTC().Format(time.RFC3339)), 0o644); err != nil {
		log.Log("DRAIN_ERROR", "flag file: "+err.Error())
	}
	docker(log, "docker", "node", "update", "--availability", "drain", nodeID())

	deadline := time.Now().Add(drainDeadline)
	emptyLogged := false
	lastMissing := ""
	for {
		time.Sleep(drainPoll)

		// Machine decisions outrank the task count: mains coming back
		// aborts the drain; low battery skips straight to shutdown.
		d := feed(m, cfg, log, " (draining)")
		switch d.Action {
		case ups.ActionUndrain:
			log.Log("ABORT_OL", "powers are back on before shutdown; undraining")
			undrainNow(log)
			return
		case ups.ActionShutdown:
			log.Log("SHUTDOWN", "low battery while draining; poweroff now")
			poweroff(log)
			return
		}

		count, err := containerCount()
		if err != nil {
			log.Log("DRAIN_ERROR", "docker ps: "+err.Error())
		} else if count == 0 {
			if !emptyLogged {
				log.Log("DRAIN_EMPTY", "no running containers; waiting for the survivor to hold the work")
				emptyLogged = true
			}
			missing := survivorMissing(log, survivor, services)
			if len(missing) == 0 {
				log.Log("DRAIN_READY", fmt.Sprintf("survivor %s runs all expected work", survivor))
				if suppressPoweroff {
					log.Log("TEST_DONE", "poweroff suppressed; undraining instead")
					undrainNow(log)
					return
				}
				log.Log("SHUTDOWN", "drained; work converged; powering off cleanly")
				poweroff(log)
				return
			}
			if key := strings.Join(missing, ","); key != lastMissing {
				log.Log("DRAIN_WAIT", fmt.Sprintf("survivor %s lacks work; missing=%s", survivor, key))
				lastMissing = key
			}
		}

		if time.Now().After(deadline) {
			log.Log("DRAIN_DEADLINE", fmt.Sprintf("%s elapsed; powering off anyway", drainDeadline))
			if suppressPoweroff {
				log.Log("TEST_DONE", "poweroff suppressed; undraining instead")
				undrainNow(log)
				return
			}
			poweroff(log)
			return
		}
	}
}

// undrainNow sets the node active again and clears the boot flag.
func undrainNow(log *ups.Logger) {
	log.Log("UNDRAIN_START", "docker node update --availability active "+nodeID())
	docker(log, "docker", "node", "update", "--availability", "active", nodeID())
	if err := os.Remove(flagFile()); err != nil && !os.IsNotExist(err) {
		log.Log("UNDRAIN_ERROR", "flag file: "+err.Error())
	}
	log.Log("UNDRAIN_DONE", "node back to active; flag cleared")
}

// shutdownNow handles ActionShutdown from the idle loop. A peer must be
// drained first (watchdog/LB shutdown with the drain still pending); a
// survivor powers off directly. Its drain wait is bounded and gated on
// the same survivor-convergence check as runDrain.
func shutdownNow(m *ups.Machine, cfg ups.DaemonConfig, log *ups.Logger, d ups.Decision) {
	if d.ShutdownDrain && !drainedAlready() {
		log.Log("DRAIN_START", "watchdog shutdown with pending drain: draining first")
		survivor := survivorNode()
		services := localTaskSnapshot(log)
		_ = os.MkdirAll(flagDir(), 0o755)
		_ = os.WriteFile(flagFile(), []byte(time.Now().UTC().Format(time.RFC3339)), 0o644)
		docker(log, "docker", "node", "update", "--availability", "drain", nodeID())
		// Bounded convergence wait; LB/OL handling is over: the machine
		// is done.
		deadline := time.Now().Add(drainDeadline)
		lastMissing := ""
		for {
			time.Sleep(drainPoll)
			count, err := containerCount()
			if err != nil {
				log.Log("DRAIN_ERROR", "docker ps: "+err.Error())
			} else if count == 0 {
				missing := survivorMissing(log, survivor, services)
				if len(missing) == 0 {
					log.Log("DRAIN_READY", fmt.Sprintf("survivor %s runs all expected work", survivor))
					break
				}
				if key := strings.Join(missing, ","); key != lastMissing {
					log.Log("DRAIN_WAIT", fmt.Sprintf("survivor %s lacks work; missing=%s", survivor, key))
					lastMissing = key
				}
			}
			if time.Now().After(deadline) {
				log.Log("DRAIN_DEADLINE", fmt.Sprintf("%s elapsed; powering off anyway", drainDeadline))
				break
			}
		}
	}
	log.Log("SHUTDOWN", "poweroff now")
	poweroff(log)
}

func drainedAlready() bool {
	_, err := os.Stat(flagFile())
	return err == nil
}

// survivorNode is the swarm node peers converge their drained work onto
// before powering off (UPS_SURVIVOR_NODE, default cl01 — the spec's
// survivor role).
func survivorNode() string {
	if n := os.Getenv("UPS_SURVIVOR_NODE"); n != "" {
		return n
	}
	return "cl01"
}

// localTaskSnapshot lists the distinct Swarm services running on this
// node just before the drain is issued. On snapshot failure it returns
// nil: the convergence gate then degrades to the historical
// local-emptiness check instead of blocking the shutdown entirely.
func localTaskSnapshot(log *ups.Logger) []string {
	out, err := exec.Command("docker", "ps",
		"--filter", "label=com.docker.swarm.service.id",
		"--format", `{{.Label "com.docker.swarm.service.name"}}`).Output()
	if err != nil {
		log.Log("DRAIN_ERROR", "docker ps snapshot: "+err.Error())
		return nil
	}
	return ups.SnapshotLocalServices(string(out))
}

// survivorMissing returns the snapshotted services that still have no
// Running task on the survivor. A node-ps failure is treated as
// "nothing confirmed yet" (the full missing list) so a transient or
// quorum-lost hiccup can never look converged; the drain deadline
// remains the escape hatch.
func survivorMissing(log *ups.Logger, survivor string, services []string) []string {
	if len(services) == 0 {
		return nil
	}
	out, err := exec.Command("docker", "node", "ps", survivor,
		"--filter", "desired-state=running",
		"--format", "{{.Name}}\t{{.CurrentState}}").Output()
	if err != nil {
		log.Log("DRAIN_ERROR", "docker node ps "+survivor+": "+err.Error())
		return append([]string(nil), services...)
	}
	return ups.MissingServices(services, string(out))
}

func containerCount() (int, error) {
	out, err := exec.Command("docker", "ps", "-q").Output()
	if err != nil {
		return 0, err
	}
	n := 0
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if strings.TrimSpace(line) != "" {
			n++
		}
	}
	return n, nil
}

// poweroff powers the node down outside our own stop path (--no-block:
// return immediately and let systemd sequence the shutdown target).
func poweroff(log *ups.Logger) {
	docker(log, "/usr/bin/systemctl", "poweroff")
}

// docker runs a command and logs its failure; docker prune-style
// failures must not wedge the loop.
func docker(log *ups.Logger, name string, args ...string) {
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		log.Log("EXEC_ERROR", fmt.Sprintf("%s: %v: %s", args[0], err, strings.TrimSpace(string(out))))
	}
}

// runUndrain is the boot-time release path (ups-undrain.service).
func runUndrain() {
	cfg, err := ups.LoadConfig(ups.EnvOS())
	if err != nil {
		fmt.Fprintln(os.Stderr, "config:", err)
		os.Exit(1)
	}
	log := newLogger()

	if !drainedAlready() {
		log.Log("UNDRAIN_SKIPPED_NO_FLAG", "ordinary reboot; nothing to release")
		return
	}
	log.Log("UNDRAIN_START", "flag file present; waiting for NUT to report OL")
	for i := 0; ; i++ {
		c, err := dial(cfg)
		if err == nil {
			st, err := c.Status()
			c.Close()
			if err == nil && ups.IsOL(st) {
				undrainNow(log)
				return
			}
			log.Log("STATUS", "ups.status="+st)
		} else if i%30 == 0 {
			log.Log("STATUS", "NUT not reachable: "+err.Error())
		}
		time.Sleep(pollInterval)
	}
}
