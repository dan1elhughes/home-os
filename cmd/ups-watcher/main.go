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
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/dan1elhughes/home-os/internal/ups"
)

const (
	pollInterval   = 10 * time.Second
	drainDeadline  = 600 * time.Second
	drainPoll      = 15 * time.Second
	dialTimeout    = 3 * time.Second
	commandTimeout = 45 * time.Second
	lockTimeout    = 10 * time.Second
	urgentTimeout  = 15 * time.Second

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
	st, ok := realProbe(cfg)
	if ok && os.Getenv("UPS_TEST_OB") == "1" {
		return "OB DISCHRG", true
	}
	return st, ok
}

func realProbe(cfg ups.DaemonConfig) (string, bool) {
	c, err := dial(cfg)
	if err != nil {
		return "", false
	}
	defer c.Close()
	st, err := c.Status()
	if err != nil {
		return "", false
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
			if runDrain(m, cfg, log, suppressPoweroff) {
				waitForPoweroff(log)
			}
		case ups.ActionUndrain:
			// Recovery is attempted below with a fresh status.
		case ups.ActionShutdown:
			shutdownNow(m, cfg, log, d)
			waitForPoweroff(log)
		}
		recoverDrain(cfg, log)
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
func runDrain(m *ups.Machine, cfg ups.DaemonConfig, log *ups.Logger, suppressPoweroff bool) bool {
	log.Log("OB_CONFIRMED", fmt.Sprintf("%d consecutive OB probes; draining", cfg.ConfirmOB))

	survivor := survivorNode()
	deadline := time.Now().Add(drainDeadline)
	var services []string
	snapshotted := false
	drained := false
	emptyLogged := false
	lastMissing := ""
	for {
		if !drained {
			var action ups.Action
			drained, action = monitoredDrain(
				func(ctx context.Context) bool { return drainAttempt(ctx, log, &services, &snapshotted) },
				func() ups.Decision { return feed(m, cfg, log, " (draining)") }, pollInterval)
			switch action {
			case ups.ActionUndrain:
				log.Log("ABORT_OL", "powers are back on before shutdown; undraining")
				recoverDrain(cfg, log)
				return false
			case ups.ActionShutdown:
				log.Log("SHUTDOWN", "low battery while draining; poweroff now")
				poweroff(log)
				return true
			}
		}
		time.Sleep(drainPoll)

		// Machine decisions outrank the task count: mains coming back
		// aborts the drain; low battery skips straight to shutdown.
		d := feed(m, cfg, log, " (draining)")
		switch d.Action {
		case ups.ActionUndrain:
			log.Log("ABORT_OL", "powers are back on before shutdown; undraining")
			recoverDrain(cfg, log)
			return false
		case ups.ActionShutdown:
			log.Log("SHUTDOWN", "low battery while draining; poweroff now")
			poweroff(log)
			return true
		}

		count, err := containerCount()
		if err != nil {
			log.Log("DRAIN_ERROR", "docker ps: "+err.Error())
		} else if drained && count == 0 {
			if !emptyLogged {
				log.Log("DRAIN_EMPTY", "no running containers; waiting for the survivor to hold the work")
				emptyLogged = true
			}
			missing := survivorMissing(log, survivor, services)
			if len(missing) == 0 {
				log.Log("DRAIN_READY", fmt.Sprintf("survivor %s runs all expected work", survivor))
				if suppressPoweroff {
					log.Log("TEST_DONE", "poweroff suppressed; undraining instead")
					recoverDrain(cfg, log)
					return false
				}
				log.Log("SHUTDOWN", "drained; work converged; powering off cleanly")
				poweroff(log)
				return true
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
				recoverDrain(cfg, log)
				return false
			}
			poweroff(log)
			return true
		}
	}
}

func monitoredDrain(attempt func(context.Context) bool, probe func() ups.Decision, interval time.Duration) (bool, ups.Action) {
	if d := probe(); d.Action == ups.ActionShutdown || d.Action == ups.ActionUndrain {
		return false, d.Action
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan bool, 1)
	go func() { result <- attempt(ctx) }()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case ok := <-result:
			return ok, ups.ActionNone
		case <-ticker.C:
			d := probe()
			if d.Action == ups.ActionShutdown || d.Action == ups.ActionUndrain {
				cancel()
				<-result
				return false, d.Action
			}
		}
	}
}

func drainAttempt(ctx context.Context, log *ups.Logger, services *[]string, snapshotted *bool) bool {
	lockCtx, cancel := context.WithTimeout(ctx, lockTimeout)
	defer cancel()
	unlock, err := lockDrain(lockCtx)
	if err != nil {
		log.Log("DRAIN_ERROR", "lock: "+err.Error())
		return false
	}
	defer unlock()
	if ctx.Err() != nil {
		return false
	}
	if !*snapshotted {
		var err error
		*services, err = localTaskSnapshot(ctx, log)
		if err != nil {
			return false
		}
		*snapshotted = true
	}
	if ctx.Err() != nil {
		return false
	}
	log.Log("DRAIN_START", fmt.Sprintf("docker node update --availability drain %s; local=%s", nodeID(), strings.Join(*services, ",")))
	if err := os.WriteFile(flagFile(), []byte(time.Now().UTC().Format(time.RFC3339)), 0644); err != nil {
		log.Log("DRAIN_ERROR", "flag file: "+err.Error())
	}
	return dockerContext(ctx, log, "docker", "node", "update", "--availability", "drain", nodeID())
}

// undrainNow sets the node active again and clears the boot flag.
func undrainNow(log *ups.Logger) bool {
	log.Log("UNDRAIN_START", "docker node update --availability active "+nodeID())
	if !docker(log, "docker", "node", "update", "--availability", "active", nodeID()) {
		return false
	}
	if err := os.Remove(flagFile()); err != nil && !os.IsNotExist(err) {
		log.Log("UNDRAIN_ERROR", "flag file: "+err.Error())
		return false
	}
	log.Log("UNDRAIN_DONE", "node back to active; flag cleared")
	return true
}

func lockDrain(ctx context.Context) (func(), error) {
	if err := os.MkdirAll(flagDir(), 0755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(flagDir()+"/lock", os.O_CREATE|os.O_RDWR, 0644)
	if err != nil {
		return nil, err
	}
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) && !errors.Is(err, syscall.EINTR) {
			f.Close()
			return nil, err
		}
		select {
		case <-ctx.Done():
			f.Close()
			return nil, ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
	return func() { syscall.Flock(int(f.Fd()), syscall.LOCK_UN); f.Close() }, nil
}

func recoverDrain(cfg ups.DaemonConfig, log *ups.Logger) bool {
	ctx, cancel := context.WithTimeout(context.Background(), lockTimeout)
	defer cancel()
	unlock, err := lockDrain(ctx)
	if err != nil {
		log.Log("UNDRAIN_ERROR", "lock: "+err.Error())
		return false
	}
	defer unlock()
	if !drainedAlready() {
		return true
	}
	status, ok := realProbe(cfg)
	if !ok || !safeForUndrain(status) {
		logRecoveryStatus(log, status, ok)
		return false
	}
	return undrainNow(log)
}

var recoveryStatus struct {
	sync.Mutex
	last string
	at   time.Time
}

func logRecoveryStatus(log *ups.Logger, status string, ok bool) {
	detail := "NUT unavailable; recovery waiting"
	if ok {
		detail = "ups.status=" + status + "; recovery waiting"
	}
	recoveryStatus.Lock()
	defer recoveryStatus.Unlock()
	if detail != recoveryStatus.last || time.Since(recoveryStatus.at) >= 5*time.Minute {
		log.Log("STATUS", detail)
		recoveryStatus.last = detail
		recoveryStatus.at = time.Now()
	}
}

func safeForUndrain(status string) bool {
	if !ups.IsOL(status) {
		return false
	}
	for _, word := range strings.Fields(status) {
		if word == "OB" || word == "LB" {
			return false
		}
	}
	return true
}

// shutdownNow makes one bounded drain attempt before an urgent peer shutdown.
func shutdownNow(m *ups.Machine, cfg ups.DaemonConfig, log *ups.Logger, d ups.Decision) {
	if d.ShutdownDrain {
		log.Log("DRAIN_START", "urgent shutdown: attempting drain")
		ctx, cancel := context.WithTimeout(context.Background(), urgentTimeout)
		prepareUrgentDrain(ctx, log)
		cancel()
	}
	log.Log("SHUTDOWN", "poweroff now")
	poweroff(log)
}

func prepareUrgentDrain(ctx context.Context, log *ups.Logger) bool {
	unlock, err := lockDrain(ctx)
	if err != nil {
		log.Log("DRAIN_ERROR", "lock: "+err.Error())
		return false
	}
	defer unlock()
	if ctx.Err() != nil {
		return false
	}
	if drainedAlready() {
		out, err := dockerOutputContext(ctx, "node", "inspect", "--format", "{{.Spec.Availability}}", nodeID())
		if err == nil && strings.TrimSpace(string(out)) == "drain" {
			return true
		}
	}
	if ctx.Err() != nil {
		return false
	}
	if err := os.WriteFile(flagFile(), []byte(time.Now().UTC().Format(time.RFC3339)), 0644); err != nil {
		log.Log("DRAIN_ERROR", "flag file: "+err.Error())
	}
	return dockerContext(ctx, log, "docker", "node", "update", "--availability", "drain", nodeID())
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
// node just before the drain is issued.
func localTaskSnapshot(ctx context.Context, log *ups.Logger) ([]string, error) {
	out, err := dockerOutputContext(ctx, "ps",
		"--filter", "label=com.docker.swarm.service.id",
		"--format", `{{.Label "com.docker.swarm.service.name"}}`)
	if err != nil {
		log.Log("DRAIN_ERROR", "docker ps snapshot: "+err.Error())
		return nil, err
	}
	return ups.SnapshotLocalServices(string(out)), nil
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
	out, err := dockerOutput("node", "ps", survivor,
		"--filter", "desired-state=running",
		"--format", "{{.Name}}\t{{.CurrentState}}")
	if err != nil {
		log.Log("DRAIN_ERROR", "docker node ps "+survivor+": "+err.Error())
		return append([]string(nil), services...)
	}
	return ups.MissingServices(services, string(out))
}

func containerCount() (int, error) {
	out, err := dockerOutput("ps", "-q")
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

// poweroff requests systemd to power down the node without waiting for shutdown.
func poweroff(log *ups.Logger) {
	requestPoweroff(func() bool {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return dockerContext(ctx, log, "/usr/bin/systemctl", "--no-block", "poweroff")
	}, func() { time.Sleep(pollInterval) })
}

func requestPoweroff(request func() bool, wait func()) {
	for !request() {
		wait()
	}
}

func waitForPoweroff(log *ups.Logger) {
	for {
		time.Sleep(time.Minute)
		poweroff(log)
	}
}

// docker runs a command and logs its failure; docker prune-style
// failures must not wedge the loop.
func docker(log *ups.Logger, name string, args ...string) bool {
	return dockerContext(context.Background(), log, name, args...)
}

func dockerContext(parent context.Context, log *ups.Logger, name string, args ...string) bool {
	ctx, cancel := context.WithTimeout(parent, commandTimeout)
	defer cancel()
	out, err := command(ctx, name, args...)
	if err != nil {
		log.Log("EXEC_ERROR", fmt.Sprintf("%s: %v: %s", args[0], err, strings.TrimSpace(string(out))))
		return false
	}
	return true
}

func command(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.WaitDelay = time.Second
	return cmd.CombinedOutput()
}

func dockerOutput(args ...string) ([]byte, error) {
	return dockerOutputContext(context.Background(), args...)
}

func dockerOutputContext(parent context.Context, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(parent, 10*time.Second)
	defer cancel()
	return command(ctx, "docker", args...)
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
	for {
		if recoverDrain(cfg, log) {
			return
		}
		time.Sleep(pollInterval)
	}
}
