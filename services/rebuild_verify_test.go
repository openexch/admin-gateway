package services

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/match/admin-gateway/config"
)

// The full handshake: pending armed by the old process → finalized by the new
// one → reported verified, with ok reflecting whether the running binary is
// the staged one.
func TestRebuildVerificationHandshake(t *testing.T) {
	dir := t.TempDir()
	ops := NewOperationsService(&config.Config{AdminDir: dir}, nil, nil, NewProgress(), nil)

	if st := ReadRebuildStatus(dir); st["state"] != "none" {
		t.Fatalf("expected none before any rebuild, got %v", st)
	}

	// Arm a pending whose staged sha matches the RUNNING (test) binary → ok.
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	sha, err := fileSha256(exe)
	if err != nil {
		t.Fatal(err)
	}
	pending := rebuildPending{
		StartedAt:    time.Now().Format(time.RFC3339),
		OpID:         "rebuild-admin-test-1",
		StagedSha256: sha,
		OldPid:       12345,
	}
	if err := writeJSONAtomic(filepath.Join(dir, rebuildPendingFile), pending); err != nil {
		t.Fatal(err)
	}

	if st := ReadRebuildStatus(dir); st["state"] != "pending" {
		t.Fatalf("expected pending, got %v", st)
	}

	ops.FinalizeRebuildVerification()

	st := ReadRebuildStatus(dir)
	if st["state"] != "verified" || st["ok"] != true {
		t.Fatalf("expected verified ok, got %v", st)
	}
	if _, err := os.Stat(filepath.Join(dir, rebuildPendingFile)); !os.IsNotExist(err) {
		t.Fatal("pending file must be consumed by finalize")
	}

	// Second finalize outside a handshake is a no-op and keeps the result.
	ops.FinalizeRebuildVerification()
	if st := ReadRebuildStatus(dir); st["state"] != "verified" {
		t.Fatalf("result must persist, got %v", st)
	}
}

// A staged sha that does not match the running binary must be verified but
// not ok (rename raced or an operator rolled back mid-restart).
func TestRebuildVerificationShaMismatch(t *testing.T) {
	dir := t.TempDir()
	ops := NewOperationsService(&config.Config{AdminDir: dir}, nil, nil, NewProgress(), nil)

	pending := rebuildPending{
		StartedAt:    time.Now().Add(-2 * time.Minute).Format(time.RFC3339),
		OpID:         "rebuild-admin-test-2",
		StagedSha256: "deadbeef",
	}
	if err := writeJSONAtomic(filepath.Join(dir, rebuildPendingFile), pending); err != nil {
		t.Fatal(err)
	}

	// Stale pending must carry the operator hint before finalize runs.
	if st := ReadRebuildStatus(dir); st["hint"] == nil {
		t.Fatalf("expected stale-pending hint, got %v", st)
	}

	ops.FinalizeRebuildVerification()
	st := ReadRebuildStatus(dir)
	if st["state"] != "verified" || st["ok"] != false {
		t.Fatalf("expected verified with ok=false on sha mismatch, got %v", st)
	}
}
