package ups

// Role selects the shutdown posture. A peer (cl02/cl03) migrates its Swarm
// work to the survivor and powers off; a survivor (cl01) stays up as long as
// possible and backs off only at low battery.
type Role string

const (
	RoleSurvivor Role = "survivor"
	RolePeer     Role = "peer"
)

// Action describes what the caller must do after a probe.
type Action int

const (
	ActionNone Action = iota
	ActionDrain
	ActionUndrain
	ActionShutdown
)

// Event is the outcome of a probe evaluation; the caller logs one line per
// non-None event. There is at most one event per probe.
type Event int

const (
	EventNone Event = iota
	EvOnBattery
	EvBackOnMains
	EvWatchdog
	EvShutdown
)

// Decision tells the caller what to execute. ShutdownDrain is only
// meaningful with ActionShutdown: a peer first drains before powering off.
type Decision struct {
	Action        Action
	ShutdownDrain bool
}

// Config carries policy knobs. Zero values fall back to the spec defaults.
type Config struct {
	Role       Role
	ConfirmOB  int   // consecutive on-battery probes before draining (default 3)
	SilentSecs int64 // silent-for-N-seconds watchdog (default 900)
}

// Machine is the pure decision core: OnProbe only; no I/O, no sleeps, no clock.
type Machine struct {
	cfg      Config
	obCount  int   // consecutive confirmed on-battery probes
	lastSeen int64 // timestamp of last successful probe; -1 = never reachable

	drained     bool // begin-drain already issued for this battery event
	poweredDown bool // a shutdown decision was made; no further action
	wasOB       bool
}

// NewMachine builds a machine with defaults applied.
func NewMachine(cfg Config) *Machine {
	if cfg.ConfirmOB <= 0 {
		cfg.ConfirmOB = 3
	}
	if cfg.SilentSecs <= 0 {
		cfg.SilentSecs = 900
	}
	return &Machine{cfg: cfg, lastSeen: -1}
}

// IsOL reports mains present — used by the boot-time undrain, which
// waits for the UPS to be back on line before releasing the node.
func IsOL(status string) bool {
	return containsWord(status, "OL")
}

func isOB(status string) bool {
	return containsWord(status, "OB")
}

func isLB(status string) bool {
	return containsWord(status, "LB")
}

func containsWord(s, w string) bool {
	for i := 0; i+len(w) <= len(s); i++ {
		if s[i:i+len(w)] != w {
			continue
		}
		before := i == 0 || s[i-1] == ' '
		after := i+len(w) == len(s) || s[i+len(w)] == ' '
		if before && after {
			return true
		}
	}
	return false
}

// OnProbe records one NUT status observation (ok=false means the probe did
// not yield a status) and returns the next decision and, if this probe
// crossed a boundary, the matching event.
func (m *Machine) OnProbe(ok bool, status string, now int64) (Decision, Event) {
	if m.poweredDown {
		return Decision{Action: ActionNone}, EventNone
	}

	var dec Decision
	var ev Event

	switch {
	case ok && isLB(status):
		// LB reacts to the first LB probe — the pack is nearly gone, no
		// time for a confirm count. The survivor powers off directly (it
		// is the last node); a peer uses the same shutdown path but its
		// drain has already begun (or begins now, as for a peer that never
		// confirmed OB because the pack fell too fast).
		m.poweredDown = true
		dec.Action = ActionShutdown
		dec.ShutdownDrain = m.cfg.Role == RolePeer
		ev = EvShutdown

	case ok && isOB(status):
		m.obCount++
		if !m.wasOB {
			m.wasOB = true
		}
		if m.obCount == m.cfg.ConfirmOB {
			// Confirmed on-battery. Only a peer begins the drain,
			// exactly once per machine lifetime; the survivor is the
			// last node and stays up through the outage — per spec it
			// backs off only at LB or the watchdog. Both roles report
			// the transition so the log shows what the node saw.
			if m.cfg.Role == RolePeer && !m.drained {
				m.drained = true
				dec.Action = ActionDrain
			}
			ev = EvOnBattery
		}

	case ok:
		m.obCount = 0
		if m.wasOB {
			m.wasOB = false
			// Stand down only a node that actually began a drain. An
			// OL return before the confirm count, or the end of a
			// survivor's OB episode, leaves nothing to release — but
			// the transition is still reported.
			if m.cfg.Role == RolePeer && m.drained {
				dec.Action = ActionUndrain
			}
			ev = EvBackOnMains
		}

	case !ok && m.lastSeen >= 0 && now-m.lastSeen >= m.cfg.SilentSecs:
		// The endpoint answered before and has now been silent past the
		// window: assume the Pi died on battery and treat it as low
		// battery — same shutdown split, but with the watchdog event.
		m.poweredDown = true
		dec.Action = ActionShutdown
		dec.ShutdownDrain = m.cfg.Role == RolePeer
		ev = EvWatchdog
	}

	if ok {
		m.lastSeen = now
	}
	return dec, ev
}
