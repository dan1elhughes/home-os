package ups

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Env is the set of daemon configuration keys. Secrets ride through ONLY
// at runtime: they are never baked into ignition (systemd renders the
// unit's EnvironmentFile=/etc/ups-watcher/creds, write-protected 0600).
//
//	UPS_ROLE=peer|survivor
//	UPS_HOST       host:port of the Pi's u psd, default port 3493
//	UPS_NAME       NUT device name, default "ups"
//	UPS_USER, UPS_PASS            NUT credentials
//	UPS_CONFIRM_OB                 optional; consecutive OB probes (default 3)
//	UPS_WATCHDOG_SILENT            optional; silent-for-N-seconds (default 900)
const (
	envRole           = "UPS_ROLE"
	envHost           = "UPS_HOST"
	envName           = "UPS_NAME"
	envUser           = "UPS_USER"
	envPass           = "UPS_PASS"
	envConfirmOB      = "UPS_CONFIRM_OB"
	envWatchdogSilent = "UPS_WATCHDOG_SILENT"

	defaultConfirmOB = 3
	defaultSilent    = int64(900)
)

// DaemonConfig is the daemon's full environment config: the pure policy
// pieces (Role, ConfirmOB, SilentSecs) plus the NUT connection details.
type DaemonConfig struct {
	Config
	UPSHost string
	UPSName string
	UPSUser string
	UPSPass string
}

// LoadConfig parses env into DaemonConfig. UPS_ROLE is mandatory and must
// be exactly "peer" or "survivor"; credentials ride along untouched.
func LoadConfig(env map[string]string) (DaemonConfig, error) {
	var c DaemonConfig

	// Strict: no trim. An accidental trailing space in a unit file must
	// be an error, not a silently-accepted role.
	role := env[envRole]
	switch Role(role) {
	case RolePeer, RoleSurvivor:
		c.Role = Role(role)
	default:
		return c, fmt.Errorf("UPS_ROLE: must be \"peer\" or \"survivor\", got %q", role)
	}

	c.UPSHost = env["UPS_HOST"]
	c.UPSName = envOr(env["UPS_NAME"], "ups")
	c.UPSUser = env["UPS_USER"]
	c.UPSPass = env["UPS_PASS"]

	if c.UPSHost == "" {
		return c, fmt.Errorf("UPS_HOST: required")
	}
	// UPS_USER/UPS_PASS are optional: pi-nut serves read-only status to
	// any LAN client (same access HA's nut integration uses).

	c.ConfirmOB = envInt(env[envConfirmOB], defaultConfirmOB)
	c.SilentSecs = envInt64(env[envWatchdogSilent], defaultSilent)
	return c, nil
}

func envOr(v, def string) string {
	if strings.TrimSpace(v) == "" {
		return def
	}
	return v
}

func envInt(v string, def int) int {
	v = strings.TrimSpace(v)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return def
	}
	return n
}

func envInt64(v string, def int64) int64 {
	v = strings.TrimSpace(v)
	if v == "" {
		return def
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n <= 0 {
		return def
	}
	return n
}

// EnvOS snapshots the process environment as a map for LoadConfig.
func EnvOS() map[string]string {
	m := make(map[string]string)
	for _, kv := range os.Environ() {
		if i := strings.IndexByte(kv, '='); i > 0 {
			m[kv[:i]] = kv[i+1:]
		}
	}
	return m
}
