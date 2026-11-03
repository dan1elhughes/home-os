package main

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dan1elhughes/home-os/internal/ups"
)

func TestLockDrainHonorsDeadline(t *testing.T) {
	t.Setenv("UPS_FLAG_DIR", t.TempDir())
	unlock, err := lockDrain(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if release, err := lockDrain(ctx); err == nil {
		release()
		t.Fatal("lock acquisition must time out")
	}
}

func TestCommandHonorsDeadline(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := command(ctx, "/bin/sleep", "10"); err == nil {
		t.Fatal("command must time out")
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("command did not stop promptly")
	}
}

func TestDrainAttemptRetriesAfterDockerFailure(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("UPS_FLAG_DIR", dir)
	t.Setenv("PATH", dir)
	path := filepath.Join(dir, "docker")
	if err := os.WriteFile(path, []byte("#!/bin/sh\ncase \"$1\" in ps) exit 0;; esac\nexit 1\n"), 0755); err != nil {
		t.Fatal(err)
	}
	log := ups.OpenLoggerStdout("test")
	var services []string
	var snapshotted bool
	if drainAttempt(context.Background(), log, &services, &snapshotted) {
		t.Fatal("failed update reported success")
	}
	if !drainedAlready() {
		t.Fatal("marker missing")
	}
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0755); err != nil {
		t.Fatal(err)
	}
	if !drainAttempt(context.Background(), log, &services, &snapshotted) {
		t.Fatal("retry did not succeed")
	}
}

func TestDrainAttemptRetriesFailedSnapshot(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("UPS_FLAG_DIR", dir)
	t.Setenv("PATH", dir)
	path := filepath.Join(dir, "docker")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nif [ \"$1\" = ps ]; then exit 1; fi\nexit 0\n"), 0755); err != nil {
		t.Fatal(err)
	}
	var services []string
	var snapshotted bool
	log := ups.OpenLoggerStdout("test")
	if drainAttempt(context.Background(), log, &services, &snapshotted) {
		t.Fatal("snapshot failure must not start a drain")
	}
	if snapshotted || drainedAlready() {
		t.Fatal("snapshot failure was treated as success")
	}
	if err := os.WriteFile(path, []byte("#!/bin/sh\nif [ \"$1\" = ps ]; then echo test.service; fi\nexit 0\n"), 0755); err != nil {
		t.Fatal(err)
	}
	if !drainAttempt(context.Background(), log, &services, &snapshotted) || !snapshotted || len(services) != 1 {
		t.Fatalf("snapshot retry failed: %v", services)
	}
}

func TestDrainAttemptLockTimeoutDoesNotRunDocker(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("UPS_FLAG_DIR", dir)
	t.Setenv("PATH", dir)
	unlock, err := lockDrain(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	var services []string
	var snapshotted bool
	if drainAttempt(ctx, ups.OpenLoggerStdout("test"), &services, &snapshotted) {
		t.Fatal("locked attempt succeeded")
	}
	if snapshotted || drainedAlready() {
		t.Fatal("drain ran without lock")
	}
	unlock()
	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte("#!/bin/sh\nexit 0\n"), 0755); err != nil {
		t.Fatal(err)
	}
	if !drainAttempt(context.Background(), ups.OpenLoggerStdout("test"), &services, &snapshotted) {
		t.Fatal("drain did not succeed after lock was released")
	}
}

func TestPoweroffRetriesFailures(t *testing.T) {
	attempts := 0
	requestPoweroff(func() bool { attempts++; return attempts == 3 }, func() {})
	if attempts != 3 {
		t.Fatalf("got %d attempts", attempts)
	}
}

func TestUrgentShutdownSkipsUpdateForConfirmedDrain(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("UPS_FLAG_DIR", dir)
	t.Setenv("PATH", dir)
	if err := os.WriteFile(flagFile(), []byte("drained"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte("#!/bin/sh\nif [ \"$2\" = inspect ]; then echo drain; exit 0; fi\nexit 1\n"), 0755); err != nil {
		t.Fatal(err)
	}
	if !prepareUrgentDrain(context.Background(), ups.OpenLoggerStdout("test")) {
		t.Fatal("confirmed drain should not need update")
	}
}

func TestUrgentShutdownRetriesUnconfirmedMarker(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("UPS_FLAG_DIR", dir)
	t.Setenv("PATH", dir)
	if err := os.WriteFile(flagFile(), []byte("drained"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte("#!/bin/sh\nif [ \"$2\" = inspect ]; then echo active; exit 0; fi\nexit 7\n"), 0755); err != nil {
		t.Fatal(err)
	}
	if prepareUrgentDrain(context.Background(), ups.OpenLoggerStdout("test")) {
		t.Fatal("failed drain update must not be reported as success")
	}
	if !drainedAlready() {
		t.Fatal("marker must remain")
	}
}

func TestUrgentDrainDeadlineIncludesDocker(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("UPS_FLAG_DIR", dir)
	t.Setenv("PATH", dir)
	if err := os.WriteFile(flagFile(), []byte("drained"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte("#!/bin/sh\nif [ \"$2\" = inspect ]; then /bin/sleep 0.06; echo active; exit 0; fi\nexec /bin/sleep 1\n"), 0755); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	if prepareUrgentDrain(ctx, ups.OpenLoggerStdout("test")) {
		t.Fatal("timed out drain succeeded")
	}
	if time.Since(start) > 500*time.Millisecond {
		t.Fatal("docker exceeded the total deadline")
	}
}

func TestMonitoredDrainCancelsOnLowBattery(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("UPS_FLAG_DIR", dir)
	t.Setenv("PATH", dir)
	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte("#!/bin/sh\nif [ \"$1\" = ps ]; then exit 0; fi\nexec /bin/sleep 10\n"), 0755); err != nil {
		t.Fatal(err)
	}
	var services []string
	var snapshotted bool
	start := time.Now()
	machine := ups.NewMachine(ups.Config{Role: ups.RolePeer})
	_, action := monitoredDrain(func(ctx context.Context) bool {
		return drainAttempt(ctx, ups.OpenLoggerStdout("test"), &services, &snapshotted)
	}, func() ups.Decision {
		status := "OB DISCHRG"
		if drainedAlready() {
			status = "OB LB"
		}
		d, _ := machine.OnProbe(true, status, time.Now().Unix())
		return d
	}, 100*time.Millisecond)
	if action != ups.ActionShutdown {
		t.Fatalf("got action %v", action)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("low battery did not cancel docker promptly")
	}
	if !drainedAlready() {
		t.Fatal("marker must remain")
	}
}

func TestMonitoredDrainCancelsAndRecoversOnMains(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("UPS_FLAG_DIR", dir)
	t.Setenv("PATH", dir)
	script := "#!/bin/sh\nif [ \"$1\" = ps ] || [ \"$4\" = active ]; then exit 0; fi\nexec /bin/sleep 10\n"
	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			bufio.NewReader(conn).ReadString('\n')
			status := "OB"
			if drainedAlready() {
				status = "OL"
			}
			fmt.Fprintf(conn, "VAR ups ups.status %q\n", status)
			conn.Close()
		}
	}()
	cfg := ups.DaemonConfig{UPSHost: listener.Addr().String(), UPSName: "ups"}
	log := ups.OpenLoggerStdout("test")
	m := ups.NewMachine(ups.Config{Role: ups.RolePeer, ConfirmOB: 1})
	m.OnProbe(true, "OB", time.Now().Unix())
	var services []string
	var snapshotted bool
	_, action := monitoredDrain(func(ctx context.Context) bool { return drainAttempt(ctx, log, &services, &snapshotted) }, func() ups.Decision { return feed(m, cfg, log, "") }, 100*time.Millisecond)
	if action != ups.ActionUndrain {
		t.Fatalf("got action %v", action)
	}
	if !recoverDrain(cfg, log) || drainedAlready() {
		t.Fatal("safe recovery did not clear marker")
	}
}

func TestUndrainPreservesMarkerWhenDockerFails(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("UPS_FLAG_DIR", dir)
	t.Setenv("PATH", dir)
	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte("#!/bin/sh\nexit 1\n"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(flagFile(), []byte("drained"), 0644); err != nil {
		t.Fatal(err)
	}
	undrainNow(ups.OpenLoggerStdout("test"))
	if _, err := os.Stat(flagFile()); err != nil {
		t.Fatalf("marker must remain after docker fails: %v", err)
	}
}

func TestUndrainReportsMarkerRemovalFailure(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("UPS_FLAG_DIR", dir)
	t.Setenv("PATH", dir)
	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte("#!/bin/sh\nexit 0\n"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(flagFile(), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(flagFile(), "child"), nil, 0644); err != nil {
		t.Fatal(err)
	}
	if undrainNow(ups.OpenLoggerStdout("test")) {
		t.Fatal("marker removal failure must be retried")
	}
	if !drainedAlready() {
		t.Fatal("marker must remain")
	}
}

func TestRecoveryChecksStatusOnEachAttempt(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("UPS_FLAG_DIR", dir)
	t.Setenv("PATH", dir)
	docker := filepath.Join(dir, "docker")
	if err := os.WriteFile(docker, []byte("#!/bin/sh\nexit 1\n"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(flagFile(), []byte("drained"), 0644); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	statuses := make(chan string, 4)
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			status := <-statuses
			reader := bufio.NewReader(conn)
			reader.ReadString('\n')
			fmt.Fprintf(conn, "VAR ups ups.status %q\n", status)
			conn.Close()
		}
	}()
	cfg := ups.DaemonConfig{UPSHost: listener.Addr().String(), UPSName: "ups"}
	log := ups.OpenLoggerStdout("test")
	statuses <- "OL"
	if recoverDrain(cfg, log) {
		t.Fatal("failed docker update must be retried")
	}
	if err := os.WriteFile(docker, []byte("#!/bin/sh\nexit 0\n"), 0755); err != nil {
		t.Fatal(err)
	}
	statuses <- "OB DISCHRG"
	if recoverDrain(cfg, log) {
		t.Fatal("battery status must not release node")
	}
	if !drainedAlready() {
		t.Fatal("marker lost during unsafe status")
	}
	statuses <- "OL LB"
	if recoverDrain(cfg, log) {
		t.Fatal("low battery must not release node")
	}
	statuses <- "OL CHRG"
	if !recoverDrain(cfg, log) {
		t.Fatal("safe status and successful update must release node")
	}
	if drainedAlready() {
		t.Fatal("marker remains after successful update")
	}
}

func TestRecoveryWithoutMarkerDoesNotContactNUT(t *testing.T) {
	t.Setenv("UPS_FLAG_DIR", t.TempDir())
	if !recoverDrain(ups.DaemonConfig{}, ups.OpenLoggerStdout("test")) {
		t.Fatal("nothing to recover")
	}
}

func TestRecoveryKeepsMarkerWhenNUTIsUnavailable(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("UPS_FLAG_DIR", dir)
	if err := os.WriteFile(flagFile(), []byte("drained"), 0644); err != nil {
		t.Fatal(err)
	}
	if recoverDrain(ups.DaemonConfig{UPSHost: "127.0.0.1:0"}, ups.OpenLoggerStdout("test")) {
		t.Fatal("unavailable NUT must not release node")
	}
	if !drainedAlready() {
		t.Fatal("marker lost when NUT was unavailable")
	}
}

func TestSafeForUndrain(t *testing.T) {
	for _, tc := range []struct {
		status string
		safe   bool
	}{
		{"OL", true}, {"OL CHRG", true}, {"OB", false}, {"", false},
		{"OL OB", false}, {"OL LB", false}, {"ONLINE", false},
	} {
		t.Run(strings.ReplaceAll(tc.status, " ", "_"), func(t *testing.T) {
			if got := safeForUndrain(tc.status); got != tc.safe {
				t.Fatalf("got %v", got)
			}
		})
	}
}
