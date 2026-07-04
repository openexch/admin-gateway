// SPDX-License-Identifier: Apache-2.0
package services

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeHeartbeat(t *testing.T, dir string, ageSec int64, state string) {
	t.Helper()
	now := time.Now().UnixMilli() - ageSec*1000
	json := fmt.Sprintf(`{"pid":1,"startedEpochMs":%d,"updatedEpochMs":%d,"lastProgressEpochMs":%d,`+
		`"lastQueryEpochMs":%d,"lastResponseEpochMs":%d,"lastLiveLogEpochMs":%d,`+
		`"liveLogPosition":42,"snapshotsRetrieved":1,"stallWarnings":0,"state":"%s"}`,
		now, now, now, now, now, now, state)
	if err := os.WriteFile(filepath.Join(dir, "backup-progress.json"), []byte(json), 0644); err != nil {
		t.Fatal(err)
	}
}

// cleanupFixture builds a fake /dev/shm + /tmp layout with driver/client IPC
// dirs, mark/lock files, and cluster archives.
func cleanupFixture(t *testing.T) (shm, tmp string) {
	t.Helper()
	shm, tmp = t.TempDir(), t.TempDir()
	mk := func(path string) {
		t.Helper()
		full := filepath.Join(shm, path)
		if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("x"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	mk("aeron-emre-0-driver/cnc.dat")
	mk("aeron-market-1234/cnc.dat")
	mk("aeron-cluster/node0/cluster/cluster-mark.dat")
	mk("aeron-cluster/node0/cluster/election.lck")
	mk("aeron-cluster/node0/archive/archive-mark.dat")
	mk("aeron-cluster/node0/archive/0-0.rec")
	mk("aeron-cluster/node0/archive/archive.catalog")
	mk("aeron-cluster/node1/archive/5-0.rec")
	mk("aeron-cluster/node1/cluster/recording.log")
	if err := os.MkdirAll(filepath.Join(tmp, "aeron-gw-99"), 0755); err != nil {
		t.Fatal(err)
	}
	return shm, tmp
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// #10 exit criterion: /cleanup provably preserves archives.
func TestCleanupSweepPreservesArchives(t *testing.T) {
	shm, tmp := cleanupFixture(t)

	_, preserved, errs := cleanupSweep(shm, tmp, false, true)
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}

	// IPC dirs, mark files, locks: gone
	for _, gone := range []string{
		"aeron-emre-0-driver", "aeron-market-1234",
		"aeron-cluster/node0/cluster/cluster-mark.dat",
		"aeron-cluster/node0/cluster/election.lck",
		"aeron-cluster/node0/archive/archive-mark.dat",
	} {
		if exists(filepath.Join(shm, gone)) {
			t.Fatalf("%s should have been cleaned", gone)
		}
	}
	if exists(filepath.Join(tmp, "aeron-gw-99")) {
		t.Fatal("tmp aeron dir should have been cleaned")
	}

	// Archives and cluster recovery state: PRESERVED
	for _, kept := range []string{
		"aeron-cluster/node0/archive/0-0.rec",
		"aeron-cluster/node0/archive/archive.catalog",
		"aeron-cluster/node1/archive/5-0.rec",
		"aeron-cluster/node1/cluster/recording.log",
	} {
		if !exists(filepath.Join(shm, kept)) {
			t.Fatalf("%s must be preserved by default cleanup", kept)
		}
	}
	if len(preserved) == 0 || !strings.Contains(preserved[0], "2 archive recording(s)") {
		t.Fatalf("preserved notice missing/wrong: %v", preserved)
	}
}

func TestCleanupSweepIncludeArchiveWipes(t *testing.T) {
	shm, tmp := cleanupFixture(t)

	_, preserved, errs := cleanupSweep(shm, tmp, true, true)
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if exists(filepath.Join(shm, "aeron-cluster")) {
		t.Fatal("includeArchive should wipe the whole cluster state dir")
	}
	if len(preserved) != 0 {
		t.Fatalf("nothing should be reported preserved on a full wipe: %v", preserved)
	}
}

func TestCleanupSweepDryRunTouchesNothing(t *testing.T) {
	shm, tmp := cleanupFixture(t)

	wouldClean, _, _ := cleanupSweep(shm, tmp, false, false)
	if len(wouldClean) == 0 {
		t.Fatal("dry run should report targets")
	}
	for _, kept := range []string{
		"aeron-emre-0-driver/cnc.dat",
		"aeron-cluster/node0/archive/0-0.rec",
		"aeron-cluster/node0/cluster/cluster-mark.dat",
	} {
		if !exists(filepath.Join(shm, kept)) {
			t.Fatalf("dry run must not delete %s", kept)
		}
	}
}

func TestBackupFreshness(t *testing.T) {
	dir := t.TempDir()

	// No heartbeat file
	fresh, reason, hb := BackupFreshness(dir)
	if fresh || hb != nil || !strings.Contains(reason, "no heartbeat") {
		t.Fatalf("missing file should be stale, got fresh=%v reason=%q", fresh, reason)
	}

	// Live OK heartbeat
	writeHeartbeat(t, dir, 2, "OK")
	fresh, _, hb = BackupFreshness(dir)
	if !fresh || hb == nil || hb.LiveLogPosition != 42 {
		t.Fatalf("recent OK heartbeat should be fresh, got fresh=%v hb=%+v", fresh, hb)
	}

	// Stale heartbeat (process dead/wedged)
	writeHeartbeat(t, dir, backupHeartbeatMaxAgeSec+10, "OK")
	fresh, reason, _ = BackupFreshness(dir)
	if fresh || !strings.Contains(reason, "heartbeat stale") {
		t.Fatalf("old heartbeat should be stale, got fresh=%v reason=%q", fresh, reason)
	}

	// Recent but STALLED
	writeHeartbeat(t, dir, 2, "STALLED")
	fresh, reason, _ = BackupFreshness(dir)
	if fresh || !strings.Contains(reason, "STALLED") {
		t.Fatalf("stalled heartbeat should not be fresh, got fresh=%v reason=%q", fresh, reason)
	}
}
