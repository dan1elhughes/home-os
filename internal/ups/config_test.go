package ups

import "testing"

// LoadConfig reads UPS_* environment variables (as a map for testability)
// into a daemon Config. Unknown roles are an error, never silent defaults:
// a mis-named role would silently change the shutdown posture.
func TestLoadConfig(t *testing.T) {
	c, err := LoadConfig(map[string]string{
		"UPS_ROLE": "peer",
		"UPS_HOST": "10.10.10.5:3493",
		"UPS_NAME": "ups",
		"UPS_USER": "nut",
		"UPS_PASS": "secret-upsd-users-value",
	})
	if err != nil {
		t.Fatal(err)
	}
	if c.Role != RolePeer || c.UPSHost != "10.10.10.5:3493" ||
		c.UPSName != "ups" || c.UPSUser != "nut" || c.UPSPass != "secret-upsd-users-value" {
		t.Fatalf("config = %+v", c)
	}
	if c.ConfirmOB != 3 || c.SilentSecs != 900 {
		t.Fatalf("defaults: ConfirmOB=%d SilentSecs=%d, want 3/900", c.ConfirmOB, c.SilentSecs)
	}

	// Overrides.
	c, err = LoadConfig(map[string]string{
		"UPS_ROLE":            "survivor",
		"UPS_HOST":            "h",
		"UPS_NAME":            "n",
		"UPS_USER":            "u",
		"UPS_PASS":            "p",
		"UPS_CONFIRM_OB":      "2",
		"UPS_WATCHDOG_SILENT": "300",
	})
	if err != nil {
		t.Fatal(err)
	}
	if c.ConfirmOB != 2 || c.SilentSecs != 300 {
		t.Fatalf("overrides: ConfirmOB=%d SilentSecs=%d, want 2/300", c.ConfirmOB, c.SilentSecs)
	}

	// Credentials optional: pi-nut read anonymously like HA does. Host
	// and role remain mandatory.
	c, err = LoadConfig(map[string]string{
		"UPS_ROLE": "peer",
		"UPS_HOST": "h",
	})
	if err != nil {
		t.Fatal(err)
	}
	if c.UPSUser != "" || c.UPSPass != "" {
		t.Fatalf("creds should be optional, got %q/%q", c.UPSUser, c.UPSPass)
	}

	for _, bad := range []string{"", "FOLLOWER", "peer " /* trailing space */} {
		if _, err := LoadConfig(map[string]string{"UPS_ROLE": bad}); err == nil {
			t.Fatalf("UPS_ROLE=%q: want error", bad)
		} else if !contains(err.Error(), "UPS_ROLE") {
			t.Fatalf("UPS_ROLE=%q: error should name UPS_ROLE, got %v", bad, err)
		}
	}
	// Invalid numeric override falls back to the default, not zero.
	c, err = LoadConfig(map[string]string{
		"UPS_ROLE":       "peer",
		"UPS_HOST":       "h",
		"UPS_NAME":       "n",
		"UPS_USER":       "u",
		"UPS_PASS":       "p",
		"UPS_CONFIRM_OB": "x",
	})
	if err != nil {
		t.Fatal(err)
	}
	if c.ConfirmOB != 3 {
		t.Fatalf("bad numeric: ConfirmOB=%d, want default 3", c.ConfirmOB)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// LogName maps machine events to the log-contract names.
func TestLogName(t *testing.T) {
	cases := map[Event]string{
		EvOnBattery:   "STATUS",
		EvBackOnMains: "STATUS",
		EvWatchdog:    "WATCHDOG_FSD",
		EvShutdown:    "SHUTDOWN",
	}
	for ev, want := range cases {
		if got := LogName(ev); got != want {
			t.Fatalf("LogName(%d) = %q, want %q", ev, got, want)
		}
	}
}
