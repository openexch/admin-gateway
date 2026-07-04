// SPDX-License-Identifier: Apache-2.0
package services

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

// Rebuild-admin post-restart verification (#12 / P3.2).
//
// doRebuildAdmin cannot observe its own successor: the systemd restart kills
// the process that ran the build. So success used to be reported BEFORE the
// restart, and a new binary that crash-looped went unnoticed. The handshake
// fixing that:
//
//  1. doRebuildAdmin stages the binary, records its sha256, keeps the old
//     binary as admin-gateway.prev, swaps, writes rebuild-pending.json, and
//     only then restarts.
//  2. The NEW process, once its HTTP server answers /health, promotes the
//     pending file to rebuild-result.json (ok = running binary's sha matches
//     the staged sha) — see FinalizeRebuildVerification, called from main.
//  3. GET /api/admin/rebuild-status (and status.lastRebuild) report the
//     handshake: "pending" that outlives a restart by more than a grace
//     period means the new binary never came up — roll back with
//     admin-gateway.prev (see docs/RUNBOOKS.md).

const rebuildPendingFile = "rebuild-pending.json"
const rebuildResultFile = "rebuild-result.json"

// rebuildPendingGraceSec: a pending older than this with a serving gateway
// means verification never ran (crash-looping new binary brought back an old
// process some other way, or the handshake file was orphaned).
const rebuildPendingGraceSec = 30

type rebuildPending struct {
	StartedAt    string `json:"startedAt"`
	OpID         string `json:"opId"`
	StagedSha256 string `json:"stagedSha256"`
	OldPid       int    `json:"oldPid"`
}

type rebuildResult struct {
	StartedAt    string `json:"startedAt"`
	VerifiedAt   string `json:"verifiedAt"`
	OpID         string `json:"opId"`
	BinarySha256 string `json:"binarySha256"`
	Pid          int    `json:"pid"`
	Ok           bool   `json:"ok"`
	Reason       string `json:"reason,omitempty"`
}

func fileSha256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func writeJSONAtomic(path string, v interface{}) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// FinalizeRebuildVerification is called by main in the NEW process once the
// HTTP server is serving. Outside a rebuild handshake it is a no-op.
func (o *OperationsService) FinalizeRebuildVerification() {
	pendingPath := filepath.Join(o.cfg.AdminDir, rebuildPendingFile)
	data, err := os.ReadFile(pendingPath)
	if err != nil {
		return // no handshake in flight (normal start)
	}
	var pending rebuildPending
	if err := json.Unmarshal(data, &pending); err != nil {
		o.log.Error("rebuild-pending.json unreadable, removing", "err", err)
		os.Remove(pendingPath)
		return
	}

	result := rebuildResult{
		StartedAt:  pending.StartedAt,
		VerifiedAt: time.Now().Format(time.RFC3339),
		OpID:       pending.OpID,
		Pid:        os.Getpid(),
		Ok:         true,
	}
	if exe, err := os.Executable(); err == nil {
		if sha, err := fileSha256(exe); err == nil {
			result.BinarySha256 = sha
			if sha != pending.StagedSha256 {
				// Serving, but NOT on the staged binary (rename raced, or an
				// operator rolled back to .prev before we verified).
				result.Ok = false
				result.Reason = "running binary sha does not match the staged sha"
			}
		}
	}

	if err := writeJSONAtomic(filepath.Join(o.cfg.AdminDir, rebuildResultFile), result); err != nil {
		o.log.Error("failed to write rebuild-result.json", "err", err)
		return
	}
	os.Remove(pendingPath)
	if result.Ok {
		o.log.Info("rebuild-admin verified: new binary is up and serving",
			"op_id", pending.OpID, "sha256", result.BinarySha256, "pid", result.Pid)
	} else {
		o.log.Error("rebuild-admin verification FAILED", "op_id", pending.OpID, "reason", result.Reason)
	}
}

// ReadRebuildStatus reports the handshake state for /api/admin/rebuild-status
// and status.lastRebuild.
func ReadRebuildStatus(adminDir string) map[string]interface{} {
	if data, err := os.ReadFile(filepath.Join(adminDir, rebuildPendingFile)); err == nil {
		var pending rebuildPending
		if json.Unmarshal(data, &pending) == nil {
			out := map[string]interface{}{
				"state":     "pending",
				"startedAt": pending.StartedAt,
				"opId":      pending.OpID,
			}
			if t, err := time.Parse(time.RFC3339, pending.StartedAt); err == nil {
				age := int64(time.Since(t).Seconds())
				out["ageSec"] = age
				if age > rebuildPendingGraceSec {
					out["hint"] = fmt.Sprintf(
						"pending for %ds: the new binary likely never came up — check "+
							"`systemctl --user status admin`, roll back by restoring admin-gateway.prev", age)
				}
			}
			return out
		}
	}
	if data, err := os.ReadFile(filepath.Join(adminDir, rebuildResultFile)); err == nil {
		var result rebuildResult
		if json.Unmarshal(data, &result) == nil {
			return map[string]interface{}{
				"state":        "verified",
				"ok":           result.Ok,
				"reason":       result.Reason,
				"startedAt":    result.StartedAt,
				"verifiedAt":   result.VerifiedAt,
				"opId":         result.OpID,
				"binarySha256": result.BinarySha256,
				"pid":          result.Pid,
			}
		}
	}
	return map[string]interface{}{"state": "none"}
}

// RebuildStatus is the handler-facing accessor.
func (o *OperationsService) RebuildStatus() map[string]interface{} {
	return ReadRebuildStatus(o.cfg.AdminDir)
}
