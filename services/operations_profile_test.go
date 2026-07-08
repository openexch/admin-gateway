// SPDX-License-Identifier: Apache-2.0
package services

import (
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/match/admin-gateway/config"
	"github.com/match/admin-gateway/logging"
)

func testProfileCfg(t *testing.T) *config.Config {
	t.Helper()
	profiles, err := config.LoadProfiles()
	if err != nil {
		t.Fatalf("LoadProfiles: %v", err)
	}
	cfg := &config.Config{
		ProjectDir: "/tmp/proj", OmsJar: "/tmp/oms.jar",
		SimBinary: "/tmp/market-sim", JarPath: "/tmp/cluster.jar", GatewayJar: "/tmp/gw.jar",
		Profiles: profiles,
	}
	cfg.SetActive("demo", profiles["demo"])
	return cfg
}

// A real profile switch (demo→performance) changes heaps for every JVM, so the
// diff must flag all six; an identical catalog must flag nothing.
func TestChangedServices(t *testing.T) {
	cfg := testProfileCfg(t)
	demo := buildServiceCatalog(cfg, cfg.Profiles["demo"])
	perf := buildServiceCatalog(cfg, cfg.Profiles["performance"])

	changed := changedServices(demo, perf)
	for _, want := range []string{"node0", "node1", "node2", "oms", "market", "backup"} {
		if !changed[want] {
			t.Errorf("demo→performance should roll %s", want)
		}
	}
	// driverProfile dev→prod also changes the media drivers.
	for _, want := range []string{"driver0", "driver1", "driver2"} {
		if !changed[want] {
			t.Errorf("demo→performance (dev→prod driver) should roll %s", want)
		}
	}

	if same := changedServices(demo, demo); len(same) != 0 {
		t.Errorf("identical catalog should change nothing, got %v", same)
	}
}

// dev↔demo keep the same driver profile (dev), so the drivers must NOT roll —
// proving the diff is spec-accurate, not "restart everything".
func TestChangedServicesSkipsUnchangedDrivers(t *testing.T) {
	cfg := testProfileCfg(t)
	demo := buildServiceCatalog(cfg, cfg.Profiles["demo"])
	dev := buildServiceCatalog(cfg, cfg.Profiles["dev"])
	changed := changedServices(demo, dev)
	for i := 0; i < 3; i++ {
		if changed[fmt.Sprintf("driver%d", i)] {
			t.Errorf("dev and demo share driverProfile=dev; driver%d must not roll", i)
		}
	}
	if !changed["node0"] {
		t.Error("dev and demo differ in node heap; node0 must roll")
	}
}

func TestReloadCatalogSwapsSpecsAndGuardsMembership(t *testing.T) {
	cfg := testProfileCfg(t)
	pm := NewProcessManager(cfg)

	// Before: node0 on demo heap (1536m).
	mustContain(t, pm.findDef("node0").Command, fmt.Sprintf("-Xmx%dm", cfg.Profiles["demo"].NodeHeapMB))

	// Same-membership reload (demo→performance) swaps the launch spec.
	perf := buildServiceCatalog(cfg, cfg.Profiles["performance"])
	if err := pm.ReloadCatalog(perf); err != nil {
		t.Fatalf("same-membership ReloadCatalog should succeed: %v", err)
	}
	mustContain(t, pm.findDef("node0").Command, fmt.Sprintf("-Xmx%dm", cfg.Profiles["performance"].NodeHeapMB))

	// Membership-changing reload (to light = embedded driver, no driver0-2) is refused.
	light := buildServiceCatalog(cfg, cfg.Profiles["light"])
	if err := pm.ReloadCatalog(light); err == nil {
		t.Fatal("membership-changing ReloadCatalog must be refused")
	} else if !strings.Contains(err.Error(), "membership") {
		t.Fatalf("unexpected reload error: %v", err)
	}
	// The refused reload left the catalog on performance (unchanged).
	mustContain(t, pm.findDef("node0").Command, fmt.Sprintf("-Xmx%dm", cfg.Profiles["performance"].NodeHeapMB))
}

// ReloadCatalog swaps pm.services live while the poller/HTTP verbs read it; run
// the readers and the swap concurrently under -race to prove the pm.mu guard
// holds (these reads were lock-free before Phase 2 made the catalog mutable).
func TestReloadCatalogConcurrentReadersNoRace(t *testing.T) {
	cfg := testProfileCfg(t)
	pm := NewProcessManager(cfg)
	demo := buildServiceCatalog(cfg, cfg.Profiles["demo"])
	perf := buildServiceCatalog(cfg, cfg.Profiles["performance"])

	var wg sync.WaitGroup
	stop := make(chan struct{})
	reader := func(fn func()) {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				fn()
			}
		}
	}
	wg.Add(4)
	go reader(func() { pm.List() })
	go reader(func() { pm.Summary() })
	go reader(func() { pm.findDef("node0") })
	go reader(func() { pm.servicesSnapshot() })

	for i := 0; i < 200; i++ {
		if i%2 == 0 {
			_ = pm.ReloadCatalog(perf)
		} else {
			_ = pm.ReloadCatalog(demo)
		}
	}
	close(stop)
	wg.Wait()
}

// ApplyProfile's synchronous validation must reject bad requests before it
// claims the progress slot or spawns the roll.
func TestApplyProfileValidation(t *testing.T) {
	cfg := testProfileCfg(t) // active = demo
	o := &OperationsService{cfg: cfg, progress: NewProgress(), log: logging.Component("test")}
	o.SetProcessManager(newFakeAgent())

	if err := o.ApplyProfile("nope", false); err == nil || !strings.Contains(err.Error(), "unknown profile") {
		t.Fatalf("unknown profile: got %v", err)
	}
	if err := o.ApplyProfile("demo", false); err == nil || !strings.Contains(err.Error(), "already active") {
		t.Fatalf("already-active without force: got %v", err)
	}
	if err := o.ApplyProfile("light", false); err == nil || !strings.Contains(err.Error(), "driver mode") {
		t.Fatalf("driver-mode switch must be refused: got %v", err)
	}
	// A valid switch gets past validation; the fake agent isn't a catalogReloader
	// so it stops there (proving it did NOT fall over in validation) without
	// spawning the async roll.
	if err := o.ApplyProfile("performance", false); err == nil || !strings.Contains(err.Error(), "catalog reload") {
		t.Fatalf("valid switch should reach the reloader check: got %v", err)
	}
	if o.progress.IsRunning() {
		t.Fatal("no progress slot should be held after a synchronous rejection")
	}
}
