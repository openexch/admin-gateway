// SPDX-License-Identifier: Apache-2.0
package services

import (
	"strings"
	"testing"

	"github.com/match/admin-gateway/config"
	"github.com/match/admin-gateway/logging"
)

func TestSummarizeNodeStates(t *testing.T) {
	cases := []struct {
		name    string
		health  []string
		want    []string
		notWant []string
	}{
		{
			name:    "all healthy",
			health:  []string{HealthHealthy, HealthHealthy, HealthHealthy},
			want:    []string{"3/3 healthy", "quorum intact"},
			notWant: []string{"QUORUM LOST"},
		},
		{
			name:    "one down keeps quorum",
			health:  []string{HealthHealthy, HealthOffline, HealthHealthy},
			want:    []string{"2/3 healthy", "quorum intact", "node1=OFFLINE"},
			notWant: []string{"QUORUM LOST"},
		},
		{
			// The exact #43 lie: the abort claimed "cluster keeps quorum (2/3)"
			// while all three nodes were dead after the OOM cascade.
			name:   "full outage",
			health: []string{HealthDead, HealthDead, HealthDead},
			want:   []string{"0/3 healthy", "QUORUM LOST", "node0=DEAD", "node2=DEAD"},
		},
		{
			name:   "one healthy is still quorum lost",
			health: []string{HealthHealthy, HealthDead, HealthOffline},
			want:   []string{"1/3 healthy", "QUORUM LOST"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			nodes := make([]map[string]interface{}, len(c.health))
			for i, h := range c.health {
				nodes[i] = map[string]interface{}{"id": i, "health": h}
			}
			got := summarizeNodeStates(nodes)
			for _, w := range c.want {
				if !strings.Contains(got, w) {
					t.Errorf("summary should contain %q, got %q", w, got)
				}
			}
			for _, nw := range c.notWant {
				if strings.Contains(got, nw) {
					t.Errorf("summary should NOT contain %q, got %q", nw, got)
				}
			}
		})
	}
}

func TestRollingUpdateGateRefusalReleasesProgress(t *testing.T) {
	progress := NewProgress()
	o := &OperationsService{
		cfg:      &config.Config{MinMemMB: 4096, MaxShmUsedPct: 100},
		progress: progress,
		log:      logging.Component("ops"),
	}
	p := NewPreflight(o.cfg)
	p.driverDir = func(int) string { return "/nonexistent/test-driver" }
	p.meminfoPath = writeMeminfo(t, 1000) // far below the 4096MB block threshold
	o.SetPreflight(p)

	err := o.RollingUpdate(false)
	if err == nil {
		t.Fatal("expected pre-flight refusal")
	}
	if !strings.Contains(err.Error(), "mem-available") {
		t.Fatalf("refusal should name the failing check, got %v", err)
	}

	// The refusal must release the progress slot (an early return without
	// Finish wedges every future operation — the #26 lesson).
	if progress.IsRunning() {
		t.Fatal("progress slot still held after refusal")
	}
	pm := progress.ToMap()
	if pm["error"] != true {
		t.Fatalf("refused op should finish in error state, got %+v", pm)
	}
	if !progress.TryStart("snapshot", 1) {
		t.Fatal("slot should be claimable after a refusal")
	}
}
