// Command wol-on-boot runs on the TrueNAS NAS. On boot it bursts magic
// packets to every cluster node NIC (harmless for running nodes: the
// packets are ignored by construction), then monitors the UPS: on every
// OB→OL transition it re-bursts for a bounded window so a node that
// missed the first packets is re-woken regularly.
//
// The built-in TrueNAS UPS service (NUT secondary at the Pi) handles the
// NAS's own low-battery shutdown; this command only sends magic packets
// and logs JSON lines to stdout (journald -u wol-on-boot).
package main

import (
	"fmt"
	"os"
	"time"

	"github.com/dan1elhughes/home-os/internal/ups"
	"github.com/dan1elhughes/home-os/internal/wol"
)

// Cluster node NIC MACs, wired to repo pin. Wake-arming on the nodes is
// networkd [Link] WakeOnLan=magic.
var nodeMACs = []string{
	"e8:ff:1e:d8:a0:ca", // cl01
	"e8:ff:1e:d8:a1:15", // cl02
	"e8:ff:1e:d8:a0:f4", // cl03
}

const (
	bootBurstCount   = 6
	bootBurstGap     = 250 * time.Millisecond
	monitorInterval  = 10 * time.Second
	remediationTicks = 10 // ticks
	remediationGap   = 60 * time.Second
	burstCount       = 6
	dialTimeout      = 3 * time.Second
)

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func main() {
	broadcast := env("WOL_BROADCAST", "10.10.10.255:9")
	logNode := env("WOL_LOG_NODE", "nas")

	e := ups.OpenLoggerStdout(logNode)

	// Boot burst: always. Running nodes ignore the packets.
	if err := wol.Burst(broadcast, nodeMACs, bootBurstCount, bootBurstGap); err != nil {
		e.Log("WOL_LOG", "boot burst error: "+err.Error())
	} else {
		e.Log("WOL_LOG", fmt.Sprintf("boot burst: %d packets to %d MACs", bootBurstCount, len(nodeMACs)))
	}

	// Monitor: OB→OL transitions trigger a remediation window.
	cfg, err := nutEnv()
	if err != nil {
		fmt.Fprintln(os.Stderr, "config:", err)
		os.Exit(1)
	}
	m := ups.NewMachine(ups.Config{Role: ups.RoleSurvivor}) // transitions only; no drain
	for {
		status, ok := probeNUT(cfg)
		_, ev := m.OnProbe(ok, status, time.Now().Unix())
		if ev == ups.EvBackOnMains {
			go remediate(broadcast, e)
		}
		time.Sleep(monitorInterval)
	}
}

// nutEnv reads the NUT connection variables; the NAS has no UPS_ROLE.
// UPS_USER/UPS_PASS optional — pi-nut allows anonymous read.
func nutEnv() (ups.DaemonConfig, error) {
	var cfg ups.DaemonConfig
	cfg.UPSHost = os.Getenv("UPS_HOST")
	cfg.UPSName = env("UPS_NAME", "ups")
	cfg.UPSUser = os.Getenv("UPS_USER")
	cfg.UPSPass = os.Getenv("UPS_PASS")
	if cfg.UPSHost == "" {
		return cfg, fmt.Errorf("UPS_HOST is required")
	}
	return cfg, nil
}

// remediate re-bursts every remediationGap for a bounded window. All
// MACs, always: packets to running or awake nodes are ignored, so the
// reachability check the spec mentions is an optimisation this build
// deliberately omits rather than a correctness need.
func remediate(broadcast string, log *ups.Logger) {
	log.Log("WOL_LOG", "OL observed; starting remediation bursts")
	for i := 0; i < remediationTicks; i++ {
		if err := wol.Burst(broadcast, nodeMACs, burstCount, bootBurstGap); err != nil {
			log.Log("WOL_LOG", "burst error: "+err.Error())
		}
		time.Sleep(remediationGap)
	}
	log.Log("WOL_LOG", "remediation window closed")
}

func probeNUT(cfg ups.DaemonConfig) (string, bool) {
	c, err := ups.Dial(cfg.UPSHost, cfg.UPSUser, cfg.UPSPass, cfg.UPSName, dialTimeout)
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
