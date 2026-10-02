package ups

import (
	"reflect"
	"strings"
	"testing"
)

// The drain gate must key on CLUSTER convergence, not local emptiness:
// Swarm tears a draining node's tasks down before the replacements reach
// Running elsewhere, and powering off in that gap removes raft quorum
// mid-reschedule and strands the work.

// SnapshotLocalServices parses `docker ps --format
// '{{.Label "com.docker.swarm.service.name"}}'` output: one service name
// per running Swarm task on this node. Non-Swarm containers produce an
// empty label line and are dropped (drain does not reschedule them).
func TestSnapshotLocalServices(t *testing.T) {
	got := SnapshotLocalServices(strings.Join([]string{
		"homeassistant_homeassistant",
		"homeassistant_homeassistant", // two replicas of one service
		"",
		"traefik_traefik",
		"   ",
	}, "\n"))
	want := []string{"homeassistant_homeassistant", "traefik_traefik"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

// No running Swarm tasks (all containers stopped, or plain `docker run`
// containers only) → empty snapshot, nothing to wait for.
func TestSnapshotLocalServicesEmpty(t *testing.T) {
	if got := SnapshotLocalServices("\n\n"); got != nil {
		t.Fatalf("want nil, got %v", got)
	}
}

// MissingServices parses `docker node ps <survivor> --filter
// desired-state=running --format '{{.Name}}\t{{.CurrentState}}'` and
// reports snapshot services with no Running task on the survivor.
func TestMissingServices(t *testing.T) {
	nodePs := strings.Join([]string{
		"homeassistant_homeassistant.1.abc123\tRunning 2 minutes ago",
		"traefik_traefik.2.def456\tPreparing 1 second ago", // not Running yet
		"unrelated_service.1.xyz789\tRunning 5 minutes ago",
		"stale_task.1.old111\tShutdown 3 minutes ago",
		"", // trailing newline
	}, "\n")
	services := []string{"homeassistant_homeassistant", "traefik_traefik", "predbat_predbat"}

	got := MissingServices(services, nodePs)
	want := []string{"predbat_predbat", "traefik_traefik"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

// Converged: every snapshotted service has at least one Running task.
func TestMissingServicesConverged(t *testing.T) {
	nodePs := strings.Join([]string{
		"a.1.t1\tRunning about a minute",
		"b.g.t2\tRunning about a minute",
	}, "\n")
	if got := MissingServices([]string{"a", "b"}, nodePs); got != nil {
		t.Fatalf("want nil, got %v", got)
	}
}

// Empty snapshot (node ran no Swarm tasks) is always converged.
func TestMissingServicesNoSnapshot(t *testing.T) {
	if got := MissingServices(nil, "a.1.t\tRunning"); got != nil {
		t.Fatalf("want nil, got %v", got)
	}
}

// Task names carry the service name as a dotted prefix. A service name
// that is a prefix of another (a, a.b) must resolve to the longest match,
// and a state that merely contains "Running" later in the string must
// still be gated on the Running prefix.
func TestMissingServicesPrefixAmbiguity(t *testing.T) {
	nodePs := strings.Join([]string{
		"a.b.1.t1\tRunning about a minute", // satisfies a.b, NOT a
		"a.1.t2\tRunning about a minute",   // satisfies a
		"c.1.t3\tNot Running 1 minute ago",
	}, "\n")
	services := []string{"a", "a.b", "c"}
	got := MissingServices(services, nodePs)
	want := []string{"c"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}
