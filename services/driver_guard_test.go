// SPDX-License-Identifier: Apache-2.0
package services

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/match/admin-gateway/logging"
)

// tempDriverDir returns a driver-dir path whose <dir>.pid sibling lives in a
// temp dir — never the box's real /dev/shm layout.
func tempDriverDir(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "aeron-test-0-driver")
}

func writeDriverPid(t *testing.T, driverDir string, pid int) {
	t.Helper()
	if err := os.WriteFile(driverDir+".pid", []byte(strconv.Itoa(pid)+"\n"), 0644); err != nil {
		t.Fatal(err)
	}
}

// reapedPid returns the pid of a process that has already exited and been
// reaped, so kill(pid, 0) fails.
func reapedPid(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	return cmd.Process.Pid
}

func TestDriverPidFileAlive(t *testing.T) {
	dir := tempDriverDir(t)

	if pid, alive := driverPidFileAlive(dir); alive || pid != 0 {
		t.Fatalf("missing pid file should read dead, got pid=%d alive=%v", pid, alive)
	}

	writeDriverPid(t, dir, os.Getpid())
	if pid, alive := driverPidFileAlive(dir); !alive || pid != os.Getpid() {
		t.Fatalf("our own pid should read alive, got pid=%d alive=%v", pid, alive)
	}

	writeDriverPid(t, dir, reapedPid(t))
	if _, alive := driverPidFileAlive(dir); alive {
		t.Fatal("reaped pid should read dead")
	}

	if err := os.WriteFile(dir+".pid", []byte("not-a-pid\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, alive := driverPidFileAlive(dir); alive {
		t.Fatal("garbage pid file should read dead")
	}
}

func TestCanDeleteDriverDir(t *testing.T) {
	dir := tempDriverDir(t)

	// Tracked running always refuses, whatever the pid file says.
	if ok, reason := canDeleteDriverDir(dir, true); ok || reason == "" {
		t.Fatalf("tracked running must refuse with a reason, got ok=%v reason=%q", ok, reason)
	}

	// Tracked stopped + no live pid: deletable (the legitimate stale case).
	if ok, _ := canDeleteDriverDir(dir, false); !ok {
		t.Fatal("stale dir with no live driver should be deletable")
	}

	// Tracked stopped + live pid file: the #42 state — never deletable.
	writeDriverPid(t, dir, os.Getpid())
	ok, reason := canDeleteDriverDir(dir, false)
	if ok {
		t.Fatal("live pid file must refuse deletion despite tracked stopped")
	}
	if !strings.Contains(reason, strconv.Itoa(os.Getpid())) {
		t.Fatalf("reason should name the live pid, got %q", reason)
	}
}

func TestOrphanDriverStartRefused(t *testing.T) {
	dir := tempDriverDir(t)
	writeDriverPid(t, dir, os.Getpid()) // a live "orphan" holds the dir

	def := ServiceDef{Name: "driver0", DriverDir: dir, Command: []string{"/bin/true"}}
	proc := &managedProcess{status: "stopped"}
	pm := &ProcessManager{
		log:      logging.Component("pm"),
		procs:    map[string]*managedProcess{"driver0": proc},
		services: []ServiceDef{def},
	}

	err := pm.startProcessInner(def, true)
	if err == nil {
		t.Fatal("expected start refusal while an orphan driver is alive")
	}
	if !strings.Contains(err.Error(), "orphan media driver") || !strings.Contains(err.Error(), "force-stop") {
		t.Fatalf("refusal should explain the orphan and the recovery, got: %v", err)
	}
	proc.mu.Lock()
	defer proc.mu.Unlock()
	if proc.status != "failed" || proc.lastError == "" {
		t.Fatalf("refusal must land in status/lastError, got status=%q lastError=%q", proc.status, proc.lastError)
	}
	if len(proc.crashTimes) != 0 {
		t.Fatal("a refused start must not count toward the crash-loop cap")
	}
}

func TestForceStopKillsOrphanDriver(t *testing.T) {
	dir := tempDriverDir(t)

	// A real process standing in for the orphan aeronmd, in its own process
	// group like the PM-spawned drivers (stop kills the group).
	orphan := exec.Command("sleep", "60")
	orphan.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := orphan.Start(); err != nil {
		t.Fatal(err)
	}
	opid := orphan.Process.Pid
	go orphan.Wait() // reap on kill
	writeDriverPid(t, dir, opid)

	def := ServiceDef{Name: "driver0", DriverDir: dir, StopTimeout: 1}
	pm := &ProcessManager{
		log:      logging.Component("pm"),
		pidDir:   t.TempDir(),
		procs:    map[string]*managedProcess{"driver0": {status: "stopped"}},
		services: []ServiceDef{def},
	}

	if err := pm.stopProcess("driver0", true); err != nil {
		t.Fatalf("force-stop should succeed, got %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && isProcessAlive(opid) {
		time.Sleep(50 * time.Millisecond)
	}
	if isProcessAlive(opid) {
		syscall.Kill(-opid, syscall.SIGKILL)
		t.Fatalf("orphan pid %d still alive after force-stop", opid)
	}
}
