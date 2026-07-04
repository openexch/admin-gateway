// SPDX-License-Identifier: Apache-2.0
package services

import (
	"log/slog"
	"sync"
	"time"

	"github.com/match/admin-gateway/logging"
)

// AutoSnapshot manages periodic snapshot scheduling
type AutoSnapshot struct {
	mu                sync.RWMutex
	enabled           bool
	intervalMinutes   int64
	lastSnapshotPos   int64
	snapshotCount     int
	stopChan          chan struct{}
	opsSvc            *OperationsService
	log               *slog.Logger
}

func NewAutoSnapshot(opsSvc *OperationsService) *AutoSnapshot {
	return &AutoSnapshot{
		lastSnapshotPos: -1,
		opsSvc:          opsSvc,
		log:             logging.Component("autosnapshot"),
	}
}

// Start begins periodic snapshots at the given interval
func (a *AutoSnapshot) Start(intervalMinutes int64) {
	a.Stop() // Stop existing scheduler

	a.mu.Lock()
	a.enabled = true
	a.intervalMinutes = intervalMinutes
	a.stopChan = make(chan struct{})
	a.mu.Unlock()

	go func() {
		ticker := time.NewTicker(time.Duration(intervalMinutes) * time.Minute)
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				a.runSnapshotCycle()
			case <-a.stopChan:
				return
			}
		}
	}()

	a.log.Info("auto-snapshot enabled", "interval_minutes", intervalMinutes)
}

// Stop disables periodic snapshots
func (a *AutoSnapshot) Stop() {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.stopChan != nil {
		close(a.stopChan)
		a.stopChan = nil
	}
	a.enabled = false
	a.intervalMinutes = 0
}

// IsEnabled returns whether auto-snapshot is active
func (a *AutoSnapshot) IsEnabled() bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.enabled
}

// GetIntervalMinutes returns the current interval
func (a *AutoSnapshot) GetIntervalMinutes() int64 {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.intervalMinutes
}

// SetLastPosition records the last snapshot position
func (a *AutoSnapshot) SetLastPosition(pos int64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.lastSnapshotPos = pos
}

// GetLastPosition returns the last snapshot position
func (a *AutoSnapshot) GetLastPosition() int64 {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.lastSnapshotPos
}

// runSnapshotCycle takes a snapshot. The snapshot path itself reclaims archive
// disk via the live-safe ArchiveHousekeeping (purges log segments below the
// snapshot position) on each node, so no separate compaction step is needed.
//
// Aeron's offline ArchiveTool compaction is deliberately NOT run here: running
// it against a live node corrupts the latest snapshot recording and breaks
// recover-from-snapshot (nodes crash on restart with "unknown recording id"
// and the cluster comes up unable to serve ingress).
func (a *AutoSnapshot) runSnapshotCycle() {
	if a.opsSvc.progress.IsRunning() {
		a.log.Warn("cycle skipped: another operation in progress")
		return
	}

	a.log.Info("triggering snapshot")
	// Never forced: an unhealthy/lagging member makes snapshotting dangerous
	// (match#35) — skipping a cycle is always safe, stranding a member is not.
	if err := a.opsSvc.Snapshot(false); err != nil {
		a.log.Warn("snapshot skipped", "err", err)
		return
	}

	if !a.waitForOperation(5 * time.Minute) {
		a.log.Warn("snapshot did not complete in time")
		return
	}

	a.mu.Lock()
	a.snapshotCount++
	a.mu.Unlock()
}

// waitForOperation polls until the current operation finishes or timeout is reached.
func (a *AutoSnapshot) waitForOperation(timeout time.Duration) bool {
	deadline := time.After(timeout)
	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()

	for {
		select {
		case <-deadline:
			return false
		case <-tick.C:
			if !a.opsSvc.progress.IsRunning() {
				return true
			}
		case <-a.stopChan:
			return false
		}
	}
}

// ToMap returns status as a map
func (a *AutoSnapshot) ToMap() map[string]interface{} {
	a.mu.RLock()
	defer a.mu.RUnlock()

	result := map[string]interface{}{
		"enabled":         a.enabled,
		"intervalMinutes": a.intervalMinutes,
	}
	if a.lastSnapshotPos >= 0 {
		result["lastPosition"] = a.lastSnapshotPos
	}
	return result
}
