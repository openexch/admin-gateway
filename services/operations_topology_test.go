// SPDX-License-Identifier: Apache-2.0
package services

import (
	"strings"
	"testing"

	"github.com/match/admin-gateway/logging"
)

// The catalog must be generated from the topology store's node counts: the
// member list (CLUSTER_ADDRESSES) IS the Raft topology, service sets grow and
// shrink with it, and nodes beyond the three dedicated core quads run unpinned.
func TestCatalogGeneratedFromTopology(t *testing.T) {
	cfg := testProfileCfg(t) // active = demo (external drivers, dedicated pinning)

	find := func(cat []ServiceDef, name string) *ServiceDef {
		for i := range cat {
			if cat[i].Name == name {
				return &cat[i]
			}
		}
		return nil
	}

	// Single-node matching engine.
	cfg.SetNodeCount("match", 1)
	cat := buildServiceCatalog(cfg, cfg.Profiles["demo"])
	n0 := find(cat, "node0")
	if n0 == nil || find(cat, "node1") != nil {
		t.Fatal("nodeCount=1 must generate exactly node0")
	}
	if got := n0.Env["CLUSTER_ADDRESSES"]; got != "127.0.0.1" {
		t.Fatalf("single-node member list must be one host, got %q", got)
	}
	if oms := find(cat, "oms"); len(oms.DependsOn) != 1 || oms.DependsOn[0] != "node0" {
		t.Fatalf("oms must depend on exactly the topology's nodes, got %v", oms.DependsOn)
	}
	if backup := find(cat, "backup"); backup.Env["CLUSTER_ADDRESSES"] != "127.0.0.1" {
		t.Fatal("backup must get the topology's member list")
	}

	// Five-node matching engine: 5 members, nodes 3-4 have no spare quad.
	cfg.SetNodeCount("match", 5)
	cat = buildServiceCatalog(cfg, cfg.Profiles["demo"])
	n4 := find(cat, "node4")
	if n4 == nil {
		t.Fatal("nodeCount=5 must generate node4")
	}
	if got := n4.Env["CLUSTER_ADDRESSES"]; strings.Count(got, ",") != 4 {
		t.Fatalf("five-node member list must have 5 hosts, got %q", got)
	}
	if n4.Command[0] == "/usr/bin/taskset" {
		t.Fatal("node4 has no dedicated quad and must run unpinned")
	}
	if n1 := find(cat, "node1"); n1.Command[0] != "/usr/bin/taskset" {
		t.Fatal("node1 keeps its dedicated quad under dedicated pinning")
	}
	if find(cat, "driver4") == nil {
		t.Fatal("external-driver profile must generate a driver per node")
	}

	// Three-node assets engine: ae0-2, three-host member list, still embedded.
	cfg.SetNodeCount("match", 3)
	cfg.SetNodeCount("assets", 3)
	cat = buildServiceCatalog(cfg, cfg.Profiles["demo"])
	ae2 := find(cat, "ae2")
	if ae2 == nil {
		t.Fatal("assets nodeCount=3 must generate ae2")
	}
	if got := ae2.Env["CLUSTER_ADDRESSES"]; got != "127.0.0.1,127.0.0.1,127.0.0.1" {
		t.Fatalf("assets member list must have 3 hosts, got %q", got)
	}
	if ae2.Env["TRANSPORT_DRIVER_MODE"] != "embedded" {
		t.Fatal("assets nodes stay embedded regardless of the stack profile")
	}
	if ae2.Env["BASE_DIR"] != "/dev/shm/aeron-assets/ae2" {
		t.Fatalf("assets node state dir must derive from the descriptor, got %q", ae2.Env["BASE_DIR"])
	}
}

// ChangeTopology's synchronous validation: everything wrong is rejected before
// the progress slot is claimed (a refusal must never wedge the shared slot).
func TestChangeTopologyValidation(t *testing.T) {
	cfg := testProfileCfg(t) // active = demo (pinning: dedicated)
	o := &OperationsService{cfg: cfg, cluster: NewMatchCluster(cfg), progress: NewProgress(), log: logging.Component("test")}
	o.SetProcessManager(newFakeAgent())

	if err := o.ChangeTopology(2); err == nil || !strings.Contains(err.Error(), "odd") {
		t.Fatalf("even counts must be rejected: %v", err)
	}
	if err := o.ChangeTopology(3); err == nil || !strings.Contains(err.Error(), "already") {
		t.Fatalf("same count must be rejected: %v", err)
	}
	if err := o.ChangeTopology(5); err == nil || !strings.Contains(err.Error(), "pinning") {
		t.Fatalf("5 nodes under dedicated pinning must steer to pinning:none: %v", err)
	}
	// A valid change reaches the reloader check (the fake agent is not a
	// catalogReloader) without claiming the slot.
	if err := o.ChangeTopology(1); err == nil || !strings.Contains(err.Error(), "catalog reload") {
		t.Fatalf("valid change should reach the reloader check: %v", err)
	}
	if o.progress.IsRunning() {
		t.Fatal("no progress slot may be held after a synchronous rejection")
	}
}
