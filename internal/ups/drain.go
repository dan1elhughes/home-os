package ups

import (
	"sort"
	"strings"
)

// Drain-convergence predicates: pure string logic over `docker` output,
// testable without a cluster.
//
// Why this gate exists: Swarm tears a draining node's tasks down as soon
// as it reschedules them, so "local containers empty" is NOT evidence
// that the work found a new home — the replacement tasks may still be in
// a scheduling pass or image pull. Powering off in that gap removes raft
// quorum mid-reschedule and strands the workload. The daemon therefore
// snapshots the local running Swarm services before issuing the drain
// and polls the survivor until every snapshotted service shows a Running
// task there (SnapshotLocalServices + MissingServices). The hard drain
// deadline remains as a bounded fallback only.

// SnapshotLocalServices parses `docker ps --filter
// label=com.docker.swarm.service.id --format
// '{{.Label "com.docker.swarm.service.name"}}'` output: one service name
// per running Swarm task on this node. Empty-label lines (no label, or a
// newline) are dropped; duplicates (replicas) collapse. Sorted for a
// stable log/compare order.
func SnapshotLocalServices(psOut string) []string {
	return parseNames(psOut)
}

// MissingServices reports which snapshot services have no Running task
// on the survivor. survivorPsOut is `docker node ps <survivor> --filter
// desired-state=running --format '{{.Name}}\t{{.CurrentState}}'` output;
// a task line only counts when CurrentState begins with "Running"
// (Preparing/New carry the desired state but hold no work yet).
//
// Task names carry the service name as a dotted prefix
// (`<service>.<slot|g>.<taskid>`). The longest matching snapshot service
// wins so `a.b` tasks are not credited to service `a`. Tasks of services
// absent from the snapshot are ignored.
func MissingServices(services []string, survivorPsOut string) []string {
	if len(services) == 0 {
		return nil
	}
	want := make(map[string]struct{}, len(services))
	for _, s := range services {
		want[s] = struct{}{}
	}

	satisfied := make(map[string]struct{})
	for _, line := range strings.Split(survivorPsOut, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		name, state, ok := strings.Cut(line, "\t")
		if !ok || !strings.HasPrefix(state, "Running") {
			continue
		}
		if svc := longestServicePrefix(name, want); svc != "" {
			satisfied[svc] = struct{}{}
		}
	}

	var missing []string
	for s := range want {
		if _, ok := satisfied[s]; !ok {
			missing = append(missing, s)
		}
	}
	sort.Strings(missing)
	return missing
}

// longestServicePrefix maps a task name (`svc.slot.id` or `svc.g.id`) to
// the longest snapshot service whose name is the task name's dotted
// prefix; "" when the task belongs to a service outside the snapshot.
func longestServicePrefix(taskName string, want map[string]struct{}) string {
	best := ""
	for svc := range want {
		if strings.HasPrefix(taskName, svc+".") && len(svc) > len(best) {
			best = svc
		}
	}
	return best
}

// parseNames extracts non-empty, trimmed lines (the docker `--format`
// output shape for one field per line).
func parseNames(out string) []string {
	var names []string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if !containsString(names, line) {
			names = append(names, line)
		}
	}
	sort.Strings(names)
	if len(names) == 0 {
		return nil
	}
	return names
}

func containsString(ss []string, s string) bool {
	for _, v := range ss {
		if v == s {
			return true
		}
	}
	return false
}
