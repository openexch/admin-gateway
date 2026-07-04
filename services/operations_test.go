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
