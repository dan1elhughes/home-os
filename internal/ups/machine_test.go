package ups

import "testing"

// Peer confirmed on-battery: after ConfirmOB consecutive OB probes the
// machine issues exactly one drain decision, and repeated OB probes keep
// returning none.
func TestPeerThreeConsecutiveOB_BeginDrainOnce(t *testing.T) {
	m := NewMachine(Config{Role: RolePeer})
	for i := int64(0); i < 2; i++ {
		d, ev := m.OnProbe(true, "OB DISCHRG", 10*i)
		if d.Action != ActionNone || ev != EventNone {
			t.Fatalf("probe %d: want none, got %+v ev=%v", i, d, ev)
		}
	}
	d, ev := m.OnProbe(true, "OB DISCHRG", 20)
	if d.Action != ActionDrain || ev != EvOnBattery {
		t.Fatalf("probe 3: want drain+OnBattery, got %+v ev=%v", d, ev)
	}
	d, ev = m.OnProbe(true, "OB DISCHRG", 30)
	if d.Action != ActionNone || ev != EventNone {
		t.Fatalf("probe 4 (repeat): want none, got %+v ev=%v", d, ev)
	}
}

// Survivor: confirmed OB must NOT drain — the survivor is the last node
// and stays up through the outage, backing off only at LB or the
// watchdog (spec: "cl01 (LB rule): react only to LB"). The transition is
// still reported so the log shows what cl01 saw.
func TestSurvivorOB_NoDrain(t *testing.T) {
	m := NewMachine(Config{Role: RoleSurvivor})
	for i := int64(0); i < 2; i++ {
		d, ev := m.OnProbe(true, "OB DISCHRG", 10*i)
		if d.Action != ActionNone || ev != EventNone {
			t.Fatalf("probe %d: want none, got %+v ev=%v", i, d, ev)
		}
	}
	d, ev := m.OnProbe(true, "OB DISCHRG", 20)
	if d.Action != ActionNone || ev != EvOnBattery {
		t.Fatalf("probe 3: want none+OnBattery, got %+v ev=%v", d, ev)
	}
	d, ev = m.OnProbe(true, "OB DISCHRG", 30)
	if d.Action != ActionNone || ev != EventNone {
		t.Fatalf("probe 4 (repeat): want none, got %+v ev=%v", d, ev)
	}
}

// Survivor: mains back after a confirmed OB episode → no undrain (it
// never drained); the OL transition is still reported.
func TestSurvivorOBThenOL_NoUndrain(t *testing.T) {
	m := NewMachine(Config{Role: RoleSurvivor})
	for i := int64(0); i < 3; i++ {
		m.OnProbe(true, "OB DISCHRG", i*10)
	}
	d, ev := m.OnProbe(true, "OL CHRG", 30)
	if d.Action != ActionNone || ev != EvBackOnMains {
		t.Fatalf("OL after OB: want none+BackOnMains, got %+v ev=%v", d, ev)
	}
}

// Peer: OL before the confirm count → no drain was ever begun, so no
// undrain; the transition is still reported. This keeps the undrain
// decision tied to a real drain (the mid-drain abort still works: a
// draining peer has the latch set).
func TestPeerOLBeforeConfirm_NoUndrain(t *testing.T) {
	m := NewMachine(Config{Role: RolePeer})
	m.OnProbe(true, "OB DISCHRG", 0)
	d, ev := m.OnProbe(true, "OL CHRG", 10)
	if d.Action != ActionNone || ev != EvBackOnMains {
		t.Fatalf("OL pre-confirm: want none+BackOnMains, got %+v ev=%v", d, ev)
	}
}

// Mains returns while draining: one undrain decision with the
// BackOnMains event. A real NUT OL reply after mains recovery is
// "OL CHRG" (charged) or "OL" (charging disabled), so probe both.
func TestPeerOLMidDrain_Undrain(t *testing.T) {
	for _, olStatus := range []string{"OL CHRG", "OL"} {
		m := NewMachine(Config{Role: RolePeer})
		for i := int64(0); i < 3; i++ {
			m.OnProbe(true, "OB DISCHRG", i*10)
		}
		d, ev := m.OnProbe(true, olStatus, 40)
		if d.Action != ActionUndrain || ev != EvBackOnMains {
			t.Fatalf("OL after OB: want undrain+BackOnMains, got %+v ev=%v", d, ev)
		}
	}
}

// A drain already issued must not repeat: after BeginDrain, further OB
// probes keep returning none (the daemon owns the wait, not the machine).
func TestPeerBeginDrain_NoRepeat(t *testing.T) {
	m := NewMachine(Config{Role: RolePeer})
	for i := int64(0); i < 4; i++ {
		m.OnProbe(true, "OB DISCHRG", i*10)
	}
	for i := int64(4); i < 8; i++ {
		d, ev := m.OnProbe(true, "OB DISCHRG", i*10)
		if d.Action != ActionNone || ev != EventNone {
			t.Fatalf("probe %d: drain repeated: %+v ev=%v", i, d, ev)
		}
	}
}

// LB reacts to the FIRST LB probe — no confirmation count. A peer shuts
// down (same shutdown path as the drain flow; drain already handled) with
// EvShutdown and ShutdownDrain=true; the survivor shuts down too but with
// ShutdownDrain=false (it is the last node — nothing left to drain).
func TestLB_Shutdown(t *testing.T) {
	// Peer (drained already by the OB flow).
	mp := NewMachine(Config{Role: RolePeer})
	for i := int64(0); i < 3; i++ {
		mp.OnProbe(true, "OB DISCHRG", i*10)
	}
	d, ev := mp.OnProbe(true, "OB LB", 30)
	if d.Action != ActionShutdown || !d.ShutdownDrain || ev != EvShutdown {
		t.Fatalf("peer LB: want shutdown(drain)+EvShutdown, got %+v ev=%v", d, ev)
	}

	// Survivor.
	ms := NewMachine(Config{Role: RoleSurvivor})
	d, ev = ms.OnProbe(true, "OB LB DISCHRG", 0)
	if d.Action != ActionShutdown || d.ShutdownDrain || ev != EvShutdown {
		t.Fatalf("survivor LB: want shutdown(no drain)+EvShutdown, got %+v ev=%v", d, ev)
	}
}

// After any shutdown decision the machine is done: every later probe —
// OL included — returns none.
func TestAfterShutdown_NoFurtherAction(t *testing.T) {
	m := NewMachine(Config{Role: RoleSurvivor})
	m.OnProbe(true, "OB LB DISCHRG", 0)
	for i := int64(1); i < 4; i++ {
		d, ev := m.OnProbe(true, "OL CHRG", i*10)
		if d.Action != ActionNone || ev != EventNone {
			t.Fatalf("post-shutdown probe %d: got %+v ev=%v", i, d, ev)
		}
	}
}

// Watchdog: the Pi answered before, then went silent. At SilentSecs the
// machine assumes the Pi died on battery and treats it as LB: survivor →
// shutdown without drain, peer → shutdown with drain, event EvWatchdog.
// Before the window elapses: no action. This runs regardless of the
// current status source, including while statuses stay OB.
func TestWatchdogSilent901(t *testing.T) {
	// Survivor, previously reachable at t=100.
	ms := NewMachine(Config{Role: RoleSurvivor, SilentSecs: 900})
	ms.OnProbe(true, "OL CHRG", 100)
	// Before the window: none.
	d, ev := ms.OnProbe(false, "", 999)
	if d.Action != ActionNone || ev != EventNone {
		t.Fatalf("t=999 (<901): got %+v ev=%v", d, ev)
	}
	d, ev = ms.OnProbe(false, "", 1001)
	if d.Action != ActionShutdown || d.ShutdownDrain || ev != EvWatchdog {
		t.Fatalf("t=1001: want shutdown(no drain)+EvWatchdog, got %+v ev=%v", d, ev)
	}

	// Peer, previously reachable.
	mp := NewMachine(Config{Role: RolePeer, SilentSecs: 900})
	mp.OnProbe(true, "OL CHRG", 0)
	d, ev = mp.OnProbe(false, "", 901)
	if d.Action != ActionShutdown || !d.ShutdownDrain || ev != EvWatchdog {
		t.Fatalf("peer watchdog: want shutdown(drain)+EvWatchdog, got %+v ev=%v", d, ev)
	}

	// Never reachable: no watchdog ever fires.
	mn := NewMachine(Config{Role: RolePeer, SilentSecs: 900})
	for i := int64(10); i <= 100000; i += 1000 {
		d, ev := mn.OnProbe(false, "", i)
		if d.Action != ActionNone || ev != EventNone {
			t.Fatalf("never-reachable probe t=%d: got %+v ev=%v", i, d, ev)
		}
	}
}

// IsOL reports mains present — the predicate the boot-time undrain waits
// on: token-bounded "OL", any co-flags ("OL CHRG", "OL DISCHRG", …).
func TestIsOL(t *testing.T) {
	for _, s := range []string{"OL", "OL CHRG", "OL DISCHRG", "HB OL"} {
		if !IsOL(s) {
			t.Fatalf("IsOL(%q) = false, want true", s)
		}
	}
	for _, s := range []string{"OB DISCHRG", "OB LB", "", "RATION"} {
		if IsOL(s) {
			t.Fatalf("IsOL(%q) = true, want false", s)
		}
	}
}
