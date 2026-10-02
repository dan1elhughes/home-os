package ups

// LogName maps a machine event to its log-contract name (the "event"
// field of the JSON log line). The verification greps these exact names.
func LogName(e Event) string {
	switch e {
	case EvOnBattery:
		return "STATUS"
	case EvBackOnMains:
		return "STATUS"
	case EvWatchdog:
		return "WATCHDOG_FSD"
	case EvShutdown:
		return "SHUTDOWN"
	}
	return ""
}
