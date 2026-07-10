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
		AssetsStateDir: "/dev/shm/aeron-assets", // mirrors config.Load()'s default
		Profiles:       profiles,
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

// The membership diff must express both directions of the driver-mode boundary:
// demo→light removes the external drivers, light→demo adds them back, and the
// nodes (whose env changes with the driver mode) land in changed either way.
func TestDiffMembership(t *testing.T) {
	cfg := testProfileCfg(t)
	demo := buildServiceCatalog(cfg, cfg.Profiles["demo"])
	light := buildServiceCatalog(cfg, cfg.Profiles["light"])

	d := diffMembership(demo, light)
	if len(d.added) != 0 {
		t.Errorf("demo→light should add nothing, got %v", d.added)
	}
	wantRemoved := map[string]bool{"driver0": true, "driver1": true, "driver2": true}
	if len(d.removed) != 3 {
		t.Fatalf("demo→light should remove the 3 drivers, got %v", d.removed)
	}
	for _, name := range d.removed {
		if !wantRemoved[name] {
			t.Errorf("unexpected removed service %q", name)
		}
	}
	for _, node := range []string{"node0", "node1", "node2"} {
		if !d.changed[node] {
			t.Errorf("%s env changes with the driver mode; must be in changed", node)
		}
	}
	if !d.changed["oms"] {
		t.Error("oms heap differs demo↔light; must be in changed")
	}

	back := diffMembership(light, demo)
	if len(back.added) != 3 || len(back.removed) != 0 {
		t.Errorf("light→demo should add the 3 drivers and remove nothing, got added=%v removed=%v", back.added, back.removed)
	}

	same := diffMembership(demo, demo)
	if len(same.added) != 0 || len(same.removed) != 0 || len(same.changed) != 0 {
		t.Errorf("identical catalogs should diff empty, got %+v", same)
	}
}

// ReloadCatalogMembership re-keys the proc map: removed services must be
// stopped, survivors keep their state records, and added services get fresh
// ones. The plain ReloadCatalog stays membership-refusing.
func TestReloadCatalogMembership(t *testing.T) {
	cfg := testProfileCfg(t)
	pm := NewProcessManager(cfg)
	light := buildServiceCatalog(cfg, cfg.Profiles["light"])
	demo := buildServiceCatalog(cfg, cfg.Profiles["demo"])

	// Mark a survivor's history to prove the record survives the re-key.
	nodeProc := pm.proc("node0")
	nodeProc.mu.Lock()
	nodeProc.restartCount = 7
	nodeProc.mu.Unlock()

	// A removed service that is still running must refuse the reload.
	drv := pm.proc("driver0")
	drv.mu.Lock()
	drv.running = true
	drv.mu.Unlock()
	if err := pm.ReloadCatalogMembership(light); err == nil || !strings.Contains(err.Error(), "still running") {
		t.Fatalf("running removed service must refuse the reload, got %v", err)
	}
	// Quiesce every driver record — on a box with a live stack, adoptExisting
	// may have adopted real driver processes from their pid files.
	for i := 0; i < 3; i++ {
		p := pm.proc(fmt.Sprintf("driver%d", i))
		p.mu.Lock()
		p.running = false
		p.starting = false
		p.mu.Unlock()
	}

	// Quiesced: demo→light drops the drivers and keeps node state.
	if err := pm.ReloadCatalogMembership(light); err != nil {
		t.Fatalf("quiesced membership reload should succeed: %v", err)
	}
	if pm.findDef("driver0") != nil || pm.proc("driver0") != nil {
		t.Fatal("driver0 must be gone from catalog and proc map after →embedded")
	}
	if got := pm.proc("node0"); got != nodeProc {
		t.Fatal("surviving node0 must keep its managedProcess record")
	}
	mustContain(t, pm.findDef("node0").Command, fmt.Sprintf("-Xmx%dm", cfg.Profiles["light"].NodeHeapMB))

	// Reverse: light→demo re-adds fresh driver records.
	if err := pm.ReloadCatalogMembership(demo); err != nil {
		t.Fatalf("reverse membership reload should succeed: %v", err)
	}
	newDrv := pm.proc("driver0")
	if newDrv == nil {
		t.Fatal("driver0 must exist again after →external")
	}
	newDrv.mu.Lock()
	status := newDrv.status
	newDrv.mu.Unlock()
	if status != "stopped" {
		t.Fatalf("re-added driver must start from a fresh stopped record, got %q", status)
	}
	if got := pm.proc("node0"); got != nodeProc {
		t.Fatal("node0 record must survive both re-keys")
	}
}

// The proc-map re-key is the new race surface: hammer the readers (which all go
// through proc()/findDef()/servicesSnapshot) while membership flips demo↔light.
// -race proves the locking.
func TestReloadCatalogMembershipConcurrentReadersNoRace(t *testing.T) {
	cfg := testProfileCfg(t)
	pm := NewProcessManager(cfg)
	demo := buildServiceCatalog(cfg, cfg.Profiles["demo"])
	light := buildServiceCatalog(cfg, cfg.Profiles["light"])

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
	go reader(func() { pm.proc("driver0") })
	go reader(func() { _ = pm.Get("node0") })

	for i := 0; i < 200; i++ {
		if i%2 == 0 {
			_ = pm.ReloadCatalogMembership(light)
		} else {
			_ = pm.ReloadCatalogMembership(demo)
		}
	}
	close(stop)
	wg.Wait()
}

// ApplyProfile's synchronous validation must reject bad requests before it
// claims the progress slot or spawns the roll.
func TestApplyProfileValidation(t *testing.T) {
	cfg := testProfileCfg(t) // active = demo
	o := &OperationsService{cfg: cfg, cluster: NewMatchCluster(cfg), progress: NewProgress(), log: logging.Component("test")}
	o.SetProcessManager(newFakeAgent())

	if err := o.ApplyProfile("nope", false); err == nil || !strings.Contains(err.Error(), "unknown profile") {
		t.Fatalf("unknown profile: got %v", err)
	}
	if err := o.ApplyProfile("demo", false); err == nil || !strings.Contains(err.Error(), "already active") {
		t.Fatalf("already-active without force: got %v", err)
	}
	// A driver-mode switch (demo→light) is no longer refused: it classifies as
	// the membership tier and proceeds — here it stops at the reloader check
	// because the fake agent is not a catalogReloader.
	if err := o.ApplyProfile("light", false); err == nil || !strings.Contains(err.Error(), "catalog reload") {
		t.Fatalf("driver-mode switch should classify as membership and reach the reloader check: got %v", err)
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
