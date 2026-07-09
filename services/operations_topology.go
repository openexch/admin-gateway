// SPDX-License-Identifier: Apache-2.0
package services

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/match/admin-gateway/config"
)

// Cluster topology change (node count): a GENESIS RE-FORM. Aeron 1.51 has
// static membership — the member list is baked into every node's launch env —
// so a different count is a different cluster identity and the old state
// cannot carry over. The op therefore stops the cluster, WIPES its state, and
// re-forms it fresh with the new member list.
//
// For the matching engine that wipe must be COORDINATED: cluster order/trade
// ids restart at 0, so the OMS's Redis money-state, the OMS Postgres
// order/trade history, and the Timescale market history would all reference a
// dead id space (id collisions in trade history, stale idempotency markers
// suppressing real settlements). The op performs the proven clean-slate in
// the same breath: Redis db0 flushed, Postgres orders/executions truncated
// (USERS AND RISK CONFIG PRESERVED), Timescale trades+candles truncated. The
// assets engine has no external stores yet — its wipe is cluster-state only.
//
// Everything here sits behind the typed DELETE-CLUSTER-STATE confirmation the
// handler enforces; nothing in the profile system can reach this code path.

// meGatewayOrder is the dependency-ordered service list stopped before (in
// reverse) and started after a matching-engine re-form.
var meGatewayOrder = []string{"backup", "oms", "market", "sim"}

// ValidNodeCounts are the Raft-sane member counts: odd, so a majority always
// exists; 7 is already past what this box can host sensibly.
var ValidNodeCounts = map[int]bool{1: true, 3: true, 5: true, 7: true}

// ChangeTopology validates and launches the genesis re-form to newCount nodes.
// The handler has already enforced the typed confirmation phrase.
func (o *OperationsService) ChangeTopology(newCount int) error {
	if !ValidNodeCounts[newCount] {
		return fmt.Errorf("nodeCount must be odd (1, 3, 5 or 7) so a Raft majority exists; got %d", newCount)
	}
	cur := o.cluster.NodeCount()
	if newCount == cur {
		return fmt.Errorf("cluster %q already runs %d node(s)", o.cluster.Name, cur)
	}
	// Dedicated pinning owns 4-core quads 0-3/4-7/8-11 — there is no spare quad
	// for a 4th node on this box. Steer bigger topologies to unpinned profiles.
	if _, prof := o.cfg.Active(); o.cluster.Name == "match" && newCount > 3 && prof.Pinning == "dedicated" {
		return fmt.Errorf("%d nodes do not fit the dedicated core quads (0-11); switch to a pinning:none profile (e.g. light) first", newCount)
	}
	if _, ok := o.procMgr.(catalogReloader); !ok {
		return fmt.Errorf("process manager does not support live catalog reload")
	}

	// steps: stop deps + stop cluster + wipe + commit + reconfigure + start (+ gateways)
	steps := 6
	if o.cluster.Name == "match" {
		steps += len(meGatewayOrder)
	}
	if !o.progress.TryStart("cluster-topology", steps) {
		return fmt.Errorf("another operation in progress")
	}
	go o.doChangeTopology(cur, newCount)
	return nil
}

func (o *OperationsService) doChangeTopology(oldCount, newCount int) {
	log := o.log.With("op", "cluster-topology", "op_id", o.progress.CurrentOpID(),
		"cluster", o.cluster.Name, "from", oldCount, "to", newCount)
	log.Info("genesis re-form starting")
	isMatch := o.cluster.Name == "match"

	// Step 1: stop dependents (ME only — the AE has none yet). Reverse boot
	// order; tolerate already-stopped services.
	o.progress.Update(1, "Stopping dependent services...")
	if isMatch {
		for i := len(meGatewayOrder) - 1; i >= 0; i-- {
			o.stopService(meGatewayOrder[i])
		}
	}

	// Step 2: stop the cluster itself — nodes then external drivers (old count).
	o.progress.Update(2, fmt.Sprintf("Stopping cluster (%d node(s))...", oldCount))
	for i := 0; i < oldCount; i++ {
		o.clusterStatus.SetNodeStatus(i, "STOPPING", false)
		o.stopService(o.cluster.NodeName(i))
		o.waitForNodeStopped(log, i, 15*time.Second)
		o.clusterStatus.SetNodeStatus(i, "OFFLINE", false)
	}
	if !o.cluster.Embedded {
		for i := 0; i < oldCount; i++ {
			if name := o.cluster.DriverName(i); name != "" {
				o.stopService(name)
			}
		}
	}
	o.clusterStatus.UpdateLeader(-1, 0)

	// Step 3: wipe. Cluster state first (exact, owned dirs only — never a
	// janitor glob near a peer cluster), then the ME's coordinated stores.
	o.progress.Update(3, "Wiping cluster state (genesis re-form)...")
	if errs := o.wipeOwnClusterState(maxInt(oldCount, newCount)); len(errs) > 0 {
		o.progress.Finish(false, "cluster state wipe failed: "+strings.Join(errs, "; ")+
			" — nothing restarted; investigate, then re-run the topology change")
		return
	}
	if isMatch {
		o.progress.Update(3, "Wiping order/trade stores (users + risk config preserved)...")
		if err := wipeMatchStores(log); err != nil {
			o.progress.Finish(false, "store wipe failed: "+err.Error()+
				" — cluster state is already wiped; fix the store issue and re-run the topology change before restarting trading")
			return
		}
	}

	// Step 4: commit the topology — live descriptor, tracker, config, disk.
	o.progress.Update(4, fmt.Sprintf("Committing topology: %d node(s)", newCount))
	o.cluster.SetNodeCount(newCount)
	o.clusterStatus.Resize(newCount)
	snap := o.cfg.SetNodeCount(o.cluster.Name, newCount)
	if err := config.PersistTopology(o.cfg.AdminDir, snap, time.Now()); err != nil {
		o.progress.Finish(false, "could not persist topology: "+err.Error())
		return
	}

	// Step 5: rebuild the catalog for the new member set (adds/removes node and
	// driver services — the quiesced membership reload from the profile work).
	o.progress.Update(5, "Reconfiguring service catalog...")
	_, prof := o.cfg.Active()
	newCatalog := buildServiceCatalog(o.cfg, prof)
	if err := o.procMgr.(catalogReloader).ReloadCatalogMembership(newCatalog); err != nil {
		o.progress.Finish(false, "membership catalog reload refused: "+err.Error())
		return
	}

	// Step 6: genesis start — external drivers first (nodes gate on cnc.dat),
	// then every node; require ingress + an elected leader.
	o.progress.Update(6, fmt.Sprintf("Starting %d-node cluster from genesis...", newCount))
	if !o.cluster.Embedded {
		for i := 0; i < newCount; i++ {
			if name := o.cluster.DriverName(i); name != "" {
				o.startService(name)
			}
		}
	}
	for i := 0; i < newCount; i++ {
		o.clusterStatus.SetNodeStatus(i, "STARTING", false)
		o.startService(o.cluster.NodeName(i))
	}
	for i := 0; i < newCount; i++ {
		o.clusterStatus.SetNodeStatus(i, "REJOINING", true)
		if !o.waitForPort("127.0.0.1", o.cluster.IngressPort(i), 120*time.Second) {
			o.clusterStatus.SetNodeStatus(i, "OFFLINE", false)
			o.progress.Finish(false, fmt.Sprintf("%s did not come up within 120s of the genesis start — %s; "+
				"investigate its log, then start the remaining services by hand or re-run the topology change",
				o.cluster.NodeName(i), o.clusterStateAtAbort()))
			return
		}
		o.clusterStatus.SetNodeStatus(i, "FOLLOWER", true)
	}
	leader := -1
	for attempt := 0; attempt < 30; attempt++ {
		if l := o.cluster.DetectLeader(); l >= 0 {
			leader = l
			break
		}
		time.Sleep(2 * time.Second)
	}
	if leader < 0 {
		o.progress.Finish(false, "cluster started but no leader was detected; "+o.clusterStateAtAbort())
		return
	}
	o.clusterStatus.UpdateLeader(leader, 0)
	o.clusterStatus.SetNodeStatus(leader, "LEADER", true)

	// Step 7+: bring the ME's dependents back in boot order.
	step := 7
	var failed []string
	if isMatch {
		for _, name := range meGatewayOrder {
			o.progress.Update(step, "Starting "+name+"...")
			if err := o.procMgr.StartUnchecked(name); err != nil {
				log.Error("dependent start failed after re-form", "service", name, "err", err)
				failed = append(failed, name)
				step++
				continue
			}
			if info := o.procMgr.Get(name); info != nil && info.Port > 0 {
				if !waitForTCP(fmt.Sprintf("127.0.0.1:%d", info.Port), 60*time.Second) {
					failed = append(failed, fmt.Sprintf("%s (port %d not ready)", name, info.Port))
				}
			}
			step++
		}
	}

	if len(failed) > 0 {
		o.progress.Finish(false, fmt.Sprintf("topology committed (%d node(s), leader node%d) but these services did not return: %s",
			newCount, leader, strings.Join(failed, ", ")))
		return
	}
	o.progress.Finish(true, fmt.Sprintf("Cluster %s re-formed from genesis with %d node(s) — leader %s",
		o.cluster.Name, newCount, o.cluster.NodeName(leader)))
	log.Info("genesis re-form complete", "leader", leader)
}

// wipeOwnClusterState removes THIS cluster's state root and its per-node driver
// dirs — exact paths derived from the descriptor, nothing else. count covers
// the larger of the old/new topologies so leftover dirs never survive a shrink.
func (o *OperationsService) wipeOwnClusterState(count int) (errs []string) {
	targets := []string{o.cluster.StateDir}
	for i := 0; i < count; i++ {
		targets = append(targets, o.cluster.DriverAeronDir(i))
	}
	for _, t := range targets {
		if err := os.RemoveAll(t); err != nil && !os.IsNotExist(err) {
			errs = append(errs, t+": "+err.Error())
		}
	}
	return errs
}

// wipeMatchStores is the coordinated clean-slate for the matching engine's
// external stores (the proven 2026-07-09 procedure): Redis db0 (bal:/holds:/
// processed: — the idempotency markers MUST die with the cluster ids or stale
// processed: keys suppress real settlements), Postgres order/trade state
// (users + risk_config_* PRESERVED), Timescale market history. Shells out to
// redis-cli/psql exactly like the rest of the gateway's ops tooling; the DB
// passwords come from the admin service environment (systemd drop-ins).
func wipeMatchStores(log interface{ Error(string, ...any) }) error {
	// Redis db0: 100% OMS-owned prefixes, no user records (those live in PG).
	if out, err := exec.Command("redis-cli", "FLUSHDB").CombinedOutput(); err != nil {
		return fmt.Errorf("redis FLUSHDB: %v (%s)", err, strings.TrimSpace(string(out)))
	}

	// OMS Postgres: order/trade state only; users and risk config untouched.
	omsSQL := "TRUNCATE orders, executions, account_balances, balance_snapshots RESTART IDENTITY CASCADE;"
	if err := runPSQL("oms", "oms", os.Getenv("OMS_POSTGRES_PASSWORD"), omsSQL); err != nil {
		return fmt.Errorf("oms postgres: %w", err)
	}

	// Timescale market history: the trades hypertable + every continuous
	// aggregate (TRUNCATE works directly on TSDB caggs).
	pw := os.Getenv("MARKET_PG_PASSWORD")
	if err := runPSQL("market", "marketdata", pw, "TRUNCATE trades;"); err != nil {
		return fmt.Errorf("marketdata postgres: %w", err)
	}
	caggs, err := psqlQuery("market", "marketdata", pw,
		"SELECT view_name FROM timescaledb_information.continuous_aggregates;")
	if err != nil {
		return fmt.Errorf("marketdata caggs list: %w", err)
	}
	for _, cagg := range caggs {
		if cagg == "" {
			continue
		}
		if err := runPSQL("market", "marketdata", pw, "TRUNCATE "+cagg+";"); err != nil {
			return fmt.Errorf("marketdata cagg %s: %w", cagg, err)
		}
	}
	return nil
}

func runPSQL(user, db, password, sql string) error {
	cmd := exec.Command("psql", "-h", "localhost", "-U", user, "-d", db, "-v", "ON_ERROR_STOP=1", "-c", sql)
	cmd.Env = append(os.Environ(), "PGPASSWORD="+password)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%v (%s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func psqlQuery(user, db, password, sql string) ([]string, error) {
	cmd := exec.Command("psql", "-h", "localhost", "-U", user, "-d", db, "-tA", "-c", sql)
	cmd.Env = append(os.Environ(), "PGPASSWORD="+password)
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("%v", err)
	}
	return strings.Split(strings.TrimSpace(string(out)), "\n"), nil
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
