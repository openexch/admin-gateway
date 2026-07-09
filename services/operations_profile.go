// SPDX-License-Identifier: Apache-2.0
package services

import (
	"fmt"
	"log/slog"
	"net"
	"reflect"
	"strings"
	"time"

	"github.com/match/admin-gateway/config"
)

// Stack-wide profile switching (Phase 2). POST /api/admin/profile applies a new
// runtime profile live via one of two tiers, classified by what the switch
// changes:
//
//   - HOT (same driver mode): rebuild the catalog in-process and roll only the
//     services whose launch spec changed — nodes one at a time (followers first,
//     leader last) with a rejoin+catch-up gate so quorum is never gambled,
//     gateways/backup/sim plain-restarted. No downtime.
//   - MEMBERSHIP (the embedded↔external driver-mode boundary, i.e. to/from the
//     light profile): the managed-service set itself changes (driverN processes
//     appear/disappear), which no live roll can do. The cluster is briefly
//     stopped as a whole, the catalog swapped membership-aware while quiesced,
//     and the cluster restarted — STATE IS PRESERVED (same Raft membership; the
//     nodes recover from snapshot + log). Brief full-cluster downtime.
//
// Neither tier can ever destroy cluster state; topology (node count) changes are
// deliberately NOT profile business (see the cluster-topology work).

// catalogReloader is the slice of the process agent the profile switch needs to
// swap the in-process service catalog. The production *ProcessManager satisfies
// it; a fake in tests can too. Kept out of agent.ProcessAgent because
// ReloadCatalog takes services.ServiceDef and agent must not import services.
type catalogReloader interface {
	ReloadCatalog(newServices []ServiceDef) error
	ReloadCatalogMembership(newServices []ServiceDef) error
}

// totalHeapMB is the committed JVM heap a profile asks for across the stack's
// JVMs (nodeCount matching-engine nodes + oms + market + backup). The sim is
// Go; drivers are C. Used only to judge switch-up headroom.
func totalHeapMB(p config.Profile, nodeCount int) int {
	return p.NodeHeapMB*nodeCount + p.OmsHeapMB + p.MarketHeapMB + p.BackupHeapMB
}

// sameLaunch is true when two service defs launch identically — same argv, env,
// pre-start and working dir. The profile only ever changes these, so this is the
// exact "does this service need rolling" test.
func sameLaunch(a, b ServiceDef) bool {
	return reflect.DeepEqual(a.Command, b.Command) &&
		reflect.DeepEqual(a.Env, b.Env) &&
		reflect.DeepEqual(a.PreStart, b.PreStart) &&
		a.WorkDir == b.WorkDir
}

// changedServices returns the set of service names whose launch spec differs
// between two catalogs of identical membership.
func changedServices(oldCat, newCat []ServiceDef) map[string]bool {
	old := make(map[string]ServiceDef, len(oldCat))
	for _, d := range oldCat {
		old[d.Name] = d
	}
	changed := map[string]bool{}
	for _, nd := range newCat {
		od, ok := old[nd.Name]
		if !ok || !sameLaunch(od, nd) {
			changed[nd.Name] = true
		}
	}
	return changed
}

// membershipDiff is the full delta between two catalogs whose membership may
// differ: services only in the new catalog (added — e.g. driver0-2 on a switch
// to external), only in the old (removed — the same drivers on a switch to
// embedded), and surviving services whose launch spec changed. changedServices
// cannot express the first two (it iterates the new catalog only).
type membershipDiff struct {
	added   []string
	removed []string
	changed map[string]bool
}

func diffMembership(oldCat, newCat []ServiceDef) membershipDiff {
	old := make(map[string]ServiceDef, len(oldCat))
	for _, d := range oldCat {
		old[d.Name] = d
	}
	d := membershipDiff{changed: map[string]bool{}}
	seen := make(map[string]bool, len(newCat))
	for _, nd := range newCat {
		seen[nd.Name] = true
		od, ok := old[nd.Name]
		switch {
		case !ok:
			d.added = append(d.added, nd.Name)
		case !sameLaunch(od, nd):
			d.changed[nd.Name] = true
		}
	}
	for _, od := range oldCat {
		if !seen[od.Name] {
			d.removed = append(d.removed, od.Name)
		}
	}
	return d
}

// allManagedServices returns every rollable service name (everything with a
// launch command, i.e. not the admin self-entry). Used by a force re-apply of
// the already-active profile to converge stragglers (e.g. nodes still on an
// older catalog after a Phase 1 deploy) onto the current profile.
func allManagedServices(cat []ServiceDef) map[string]bool {
	m := map[string]bool{}
	for _, d := range cat {
		if d.Name == "admin" || len(d.Command) == 0 {
			continue
		}
		m[d.Name] = true
	}
	return m
}

// ApplyProfile validates and starts an asynchronous stack-wide profile switch.
// force overrides the switch-up memory headroom guard AND permits re-applying
// the already-active profile (a full re-roll onto it — the straggler-converge
// path). Returns an error synchronously for a bad request or a busy slot; the
// roll itself reports via the progress slot.
func (o *OperationsService) ApplyProfile(name string, force bool) error {
	prof, ok := o.cfg.Profiles[name]
	if !ok {
		return fmt.Errorf("unknown profile %q (have: %s)", name, strings.Join(config.ProfileNames(o.cfg.Profiles), ", "))
	}
	curName, curProf := o.cfg.Active()

	reapply := name == curName
	if reapply && !force {
		return fmt.Errorf("profile %q is already active; re-apply with {\"force\":true} to re-roll every service onto it", name)
	}

	// Apply-tier classifier: only the embedded↔external driver-mode boundary
	// changes the managed-service set. Same driver mode → the hot rolling path;
	// different → the membership path (brief full-cluster stop, state preserved).
	membership := prof.DriverMode != curProf.DriverMode

	// Switch-up headroom: refuse to commit bigger heaps than the box can hold
	// above the (post-switch, override-aware) floor, unless forced.
	if !reapply {
		if delta := totalHeapMB(prof, o.cluster.NodeCount()) - totalHeapMB(curProf, o.cluster.NodeCount()); delta > 0 && !force {
			if o.preflight != nil {
				if avail := o.preflight.MemAvailableBytes(); avail >= 0 {
					floor := o.cfg.EffectiveMinMem(prof)
					needMB := int64(delta) + int64(floor)
					if avail/(1024*1024) < needMB {
						return fmt.Errorf("insufficient memory to switch up to %q: MemAvailable %dMB < ~%dMB needed "+
							"(heap delta %dMB + floor %dMB); free memory or override with {\"force\":true}",
							name, avail/(1024*1024), needMB, delta, floor)
					}
				}
			}
		}
	}

	// Rebuild both catalogs and diff so we roll only what actually changes.
	oldCatalog := buildServiceCatalog(o.cfg, curProf)
	newCatalog := buildServiceCatalog(o.cfg, prof)

	if _, ok := o.procMgr.(catalogReloader); !ok {
		return fmt.Errorf("process manager does not support live catalog reload")
	}

	if membership {
		// The membership tier: gateways whose spec changes still roll after the
		// cluster comes back; the cluster services themselves are handled by the
		// stop→reconfigure→start sequence, not per-service rolls.
		diff := diffMembership(oldCatalog, newCatalog)
		var gwChanged []string
		for _, gw := range []string{"backup", "oms", "market", "sim"} {
			if diff.changed[gw] {
				gwChanged = append(gwChanged, gw)
			}
		}
		// steps: persist + snapshot + stop + reconfigure + start, then gateways.
		if !o.progress.TryStart("apply-profile", len(gwChanged)+5) {
			return fmt.Errorf("another operation in progress")
		}
		// Start only from a healthy cluster (the stop/start is a full outage; a
		// degraded member would come back into an even riskier restart). gate()
		// releases the slot on refusal; force overrides (and is the rollback
		// path's escape hatch when re-applying from a half-stopped state).
		if err := o.gate("apply-profile", force); err != nil {
			return err
		}
		go o.doApplyProfileMembership(curName, name, prof, newCatalog, diff, gwChanged)
		return nil
	}

	var changed map[string]bool
	if reapply {
		changed = allManagedServices(newCatalog)
	} else {
		changed = changedServices(oldCatalog, newCatalog)
	}

	// steps: persist + activate + one per changed service.
	if !o.progress.TryStart("apply-profile", len(changed)+3) {
		return fmt.Errorf("another operation in progress")
	}
	// Cluster-health gate (quorum/mem/driver-dirs): the node roll carries the
	// same quorum hazard as a rolling update, so refuse to start from a degraded
	// cluster. gate() releases the slot on refusal. force overrides (and is
	// required for a straggler re-apply anyway).
	if err := o.gate("apply-profile", force); err != nil {
		return err
	}
	go o.doApplyProfile(curName, name, prof, newCatalog, changed)
	return nil
}

func (o *OperationsService) doApplyProfile(fromName, toName string, toProf config.Profile, newCatalog []ServiceDef, changed map[string]bool) {
	log := o.log.With("op", "apply-profile", "op_id", o.progress.CurrentOpID(), "from", fromName, "to", toName)
	log.Info("applying stack profile", "changed", len(changed))

	// Step 1: persist the choice FIRST, so an admin crash/restart mid-roll boots
	// straight onto the target profile and the operator can finish the roll with
	// a force re-apply.
	o.progress.Update(1, "Persisting profile choice: "+toName)
	if err := config.PersistActiveProfile(o.cfg.AdminDir, toName, time.Now()); err != nil {
		o.progress.Finish(false, "could not persist profile choice: "+err.Error())
		return
	}

	// Step 2: swap the PM catalog, THEN commit the in-memory active profile + OS
	// knobs. Reload before SetActive so a (guarded, so unreachable) membership
	// failure leaves cfg untouched — only the persisted intent moved, which the
	// next admin boot reconciles. After this, explicit restarts pick up the new
	// argv/env and the preflight mem gate + reported active profile move.
	o.progress.Update(2, "Activating profile "+toName+" (catalog, governor/THP)")
	if err := o.procMgr.(catalogReloader).ReloadCatalog(newCatalog); err != nil {
		o.progress.Finish(false, "catalog reload refused: "+err.Error())
		return
	}
	o.cfg.SetActive(toName, toProf)
	ApplyProfileOSKnobs(toProf)

	if len(changed) == 0 {
		o.progress.Finish(true, "Profile "+toName+" active (OS-level only; no service needed rolling)")
		return
	}

	// Step 3+: roll the cluster nodes first (quorum-safe), then the gateways.
	step := 3
	nextStep, ok := o.rollNodesOntoProfile(log, toName, changed, step)
	if !ok {
		return // rollNodesOntoProfile already Finished the slot with the abort reason
	}
	step = nextStep

	// Non-quorum services: plain restarts, sequentially (one JVM at a time on a
	// memory-constrained box). Order respects dependencies (backup/oms/market
	// before the sim that rides them). A restart that errors, or a TCP port that
	// never comes back, is recorded and fails the operation — the cluster is
	// safe, but the run must not report success with a service down.
	var failed []string
	for _, name := range []string{"backup", "oms", "market", "sim"} {
		if !changed[name] {
			continue
		}
		o.progress.Update(step, "Restarting "+name+" onto "+toName+"...")
		if err := o.procMgr.Restart(name); err != nil {
			log.Error("service restart failed during profile roll", "service", name, "err", err)
			failed = append(failed, name)
			step++
			continue
		}
		// Readiness: these are TCP HTTP/WS listeners (oms 8080, market 8081, sim
		// 8090); backup has no port. waitForPort is UDP-only (Aeron), so use a
		// TCP dial here.
		if info := o.procMgr.Get(name); info != nil && info.Port > 0 {
			if !waitForTCP(fmt.Sprintf("127.0.0.1:%d", info.Port), 60*time.Second) {
				log.Warn("service port not back after profile restart", "service", name, "port", info.Port)
				failed = append(failed, fmt.Sprintf("%s (port %d not ready)", name, info.Port))
			}
		}
		step++
	}

	if len(failed) > 0 {
		o.progress.Finish(false, fmt.Sprintf("Profile %s: cluster rolled, but these services did not return healthy: %s "+
			"— investigate, then re-apply %s with force to converge", toName, strings.Join(failed, ", "), toName))
		log.Error("profile roll: services did not return", "failed", failed)
		return
	}

	o.progress.Finish(true, fmt.Sprintf("Profile %s applied — rolled %d service(s)", toName, len(changed)))
	log.Info("profile applied", "rolled", len(changed))
}

// doApplyProfileMembership applies a profile across the embedded↔external
// driver-mode boundary: the managed-service set changes (driverN processes
// appear/disappear), so the cluster is stopped as a whole, the catalog swapped
// membership-aware while quiesced, and the cluster restarted. STATE IS
// PRESERVED — the Raft membership is unchanged and every node recovers from its
// snapshot + log; the cost is a brief full-cluster outage. Failure never loses
// state either: the fix is re-applying the target (or the previous profile to
// roll back) with force — the sequence tolerates already-stopped services.
func (o *OperationsService) doApplyProfileMembership(fromName, toName string, toProf config.Profile, newCatalog []ServiceDef, diff membershipDiff, gwChanged []string) {
	log := o.log.With("op", "apply-profile", "op_id", o.progress.CurrentOpID(), "from", fromName, "to", toName, "tier", "membership")
	log.Info("applying stack profile across the driver-mode boundary",
		"added", diff.added, "removed", diff.removed, "gateways", gwChanged)

	// Step 1: persist FIRST — an admin crash mid-sequence boots straight onto the
	// target profile (and its catalog), and the operator finishes with a force
	// re-apply.
	o.progress.Update(1, "Persisting profile choice: "+toName)
	if err := config.PersistActiveProfile(o.cfg.AdminDir, toName, time.Now()); err != nil {
		o.progress.Finish(false, "could not persist profile choice: "+err.Error())
		return
	}

	// Step 2: snapshot to bound the recovery replay (best-effort — the cluster
	// recovers from snapshot+log either way, a fresh snapshot just shortens it).
	o.progress.Update(2, "Snapshotting cluster before the driver-mode change...")
	if leader := o.cluster.DetectLeader(); leader >= 0 {
		if _, err := o.cluster.TakeSnapshot(leader); err != nil {
			log.Warn("pre-switch snapshot failed; recovery will replay more log", "err", err)
		} else {
			time.Sleep(3 * time.Second) // let the snapshot land before stopping
		}
	} else {
		log.Warn("pre-switch snapshot skipped: leader unknown")
	}

	// Step 3: quiesce — nodes first (they own the client sessions), then the
	// drivers being removed. Stopping an already-stopped service is a no-op,
	// which is exactly what makes this re-runnable from a half-stopped state.
	o.progress.Update(3, "Stopping cluster for the driver-mode change ("+fromName+"→"+toName+")...")
	for i := 0; i < o.cluster.NodeCount(); i++ {
		o.clusterStatus.SetNodeStatus(i, "STOPPING", false)
		o.stopService(o.cluster.NodeName(i))
		o.waitForNodeStopped(log, i, 15*time.Second)
		o.clusterStatus.SetNodeStatus(i, "OFFLINE", false)
	}
	for _, name := range diff.removed {
		o.stopService(name)
	}
	o.clusterStatus.UpdateLeader(-1, 0)

	// Step 4: swap the catalog membership-aware (refused if anything being
	// removed still runs), then commit the in-memory profile + OS knobs.
	o.progress.Update(4, "Reconfiguring service catalog (driver mode → "+toProf.DriverMode+")...")
	if err := o.procMgr.(catalogReloader).ReloadCatalogMembership(newCatalog); err != nil {
		o.progress.Finish(false, "membership catalog reload refused: "+err.Error()+
			" — cluster is stopped but state is intact; stop the named service and re-apply "+toName+" with force")
		return
	}
	o.cfg.SetActive(toName, toProf)
	ApplyProfileOSKnobs(toProf)

	// Step 5: bring the cluster back — added drivers first (nodes gate on their
	// cnc.dat via WaitForFile/GatedBy), then every node; then require ingress +
	// an elected leader before declaring the cluster recovered.
	o.progress.Update(5, "Starting cluster on "+toName+"...")
	for _, name := range diff.added {
		o.startService(name)
	}
	for i := 0; i < o.cluster.NodeCount(); i++ {
		o.clusterStatus.SetNodeStatus(i, "STARTING", false)
		o.startService(o.cluster.NodeName(i))
	}
	for i := 0; i < o.cluster.NodeCount(); i++ {
		o.clusterStatus.SetNodeStatus(i, "REJOINING", true)
		if !o.waitForPort("127.0.0.1", o.cluster.IngressPort(i), 120*time.Second) {
			o.clusterStatus.SetNodeStatus(i, "OFFLINE", false)
			o.progress.Finish(false, fmt.Sprintf("node%d did not come back within 120s after the driver-mode change — %s. "+
				"State is intact; investigate node%d's log, then re-apply %s with force (or apply %s to roll back)",
				i, o.clusterStateAtAbort(), i, toName, fromName))
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
		o.progress.Finish(false, "cluster restarted but no leader was detected after the driver-mode change; "+o.clusterStateAtAbort())
		return
	}
	o.clusterStatus.UpdateLeader(leader, 0)
	o.clusterStatus.SetNodeStatus(leader, "LEADER", true)

	// Step 6+: gateways whose launch spec changed, same discipline as the hot
	// path: one JVM at a time, TCP readiness, failures fail the op.
	step := 6
	var failed []string
	for _, name := range gwChanged {
		o.progress.Update(step, "Restarting "+name+" onto "+toName+"...")
		if err := o.procMgr.Restart(name); err != nil {
			log.Error("service restart failed during membership apply", "service", name, "err", err)
			failed = append(failed, name)
			step++
			continue
		}
		if info := o.procMgr.Get(name); info != nil && info.Port > 0 {
			if !waitForTCP(fmt.Sprintf("127.0.0.1:%d", info.Port), 60*time.Second) {
				log.Warn("service port not back after membership apply", "service", name, "port", info.Port)
				failed = append(failed, fmt.Sprintf("%s (port %d not ready)", name, info.Port))
			}
		}
		step++
	}

	if len(failed) > 0 {
		o.progress.Finish(false, fmt.Sprintf("Profile %s: cluster reconfigured and healthy, but these services did not return: %s "+
			"— investigate, then re-apply %s with force to converge", toName, strings.Join(failed, ", "), toName))
		log.Error("membership apply: services did not return", "failed", failed)
		return
	}

	o.progress.Finish(true, fmt.Sprintf("Profile %s applied across the driver-mode boundary — cluster reconfigured (%s driver), state preserved",
		toName, toProf.DriverMode))
	log.Info("profile applied across the driver-mode boundary", "leader", leader)
}

// waitForTCP blocks until a TCP connect to hostport succeeds or the timeout
// elapses. Used to confirm a restarted gateway/sim's HTTP listener is accepting
// again (waitForPort checks Aeron UDP ports only).
func waitForTCP(hostport string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", hostport, 2*time.Second)
		if err == nil {
			conn.Close()
			return true
		}
		time.Sleep(500 * time.Millisecond)
	}
	return false
}

// rollNodesOntoProfile rolls the cluster nodes (and their media drivers, if the
// driver spec changed too) onto the new profile one at a time, followers before
// the leader, with a rejoin + log-catch-up gate between each so the cluster
// never drops below quorum. It mirrors doRollingUpdate's proven node sequence
// but swaps no jar — the node simply relaunches under its new (already-swapped)
// catalog def. Returns the next progress step and false if it aborted (having
// already Finished the slot).
func (o *OperationsService) rollNodesOntoProfile(log *slog.Logger, toName string, changed map[string]bool, startStep int) (int, bool) {
	nodeChanged := func(i int) bool {
		return changed[o.cluster.NodeName(i)] || (!o.cluster.Embedded && changed[o.cluster.DriverName(i)])
	}
	any := false
	for i := 0; i < o.cluster.NodeCount(); i++ {
		if nodeChanged(i) {
			any = true
		}
	}
	if !any {
		return startStep, true
	}

	leader := o.cluster.DetectLeader()
	if leader < 0 {
		leader = o.clusterStatus.GetLeaderId()
	}
	if leader < 0 {
		o.progress.Finish(false, "cannot roll nodes onto "+toName+": cluster leader unknown; "+o.clusterStateAtAbort())
		return startStep, false
	}

	step := startStep

	// Phase A: changed followers, one at a time, hard-fail on rejoin/catch-up.
	// The leader is fixed for this phase (we never touch it here), so pass it
	// down as the catch-up target rather than re-detecting per node — a transient
	// DetectLeader() failure must NOT silently skip the catch-up gate.
	for i := 0; i < o.cluster.NodeCount(); i++ {
		if i == leader || !nodeChanged(i) {
			continue
		}
		if !o.rollOneNode(log, toName, i, changed, step, leader) {
			return step, false
		}
		step++
	}

	// Phase B: the leader last (only if it, or its driver, changed). Stop it,
	// wait for the followers to elect a new leader, then bring it back as a
	// follower and confirm catch-up.
	if nodeChanged(leader) {
		o.progress.Update(step, fmt.Sprintf("Stopping Node %d (leader) for profile roll...", leader))
		o.clusterStatus.SetNodeStatus(leader, "STOPPING", false)
		o.stopService(o.cluster.NodeName(leader))
		o.waitForNodeStopped(log, leader, 15*time.Second)
		o.clusterStatus.SetNodeStatus(leader, "OFFLINE", false)

		o.progress.Update(step, "Waiting for a new leader election...")
		newLeader := -1
		for attempt := 0; attempt < 30; attempt++ {
			time.Sleep(2 * time.Second)
			if l := o.cluster.DetectLeader(); l >= 0 && l != leader {
				newLeader = l
				break
			}
		}
		if newLeader < 0 {
			o.progress.Finish(false, fmt.Sprintf("no new leader elected after stopping node%d — ABORTED; %s",
				leader, o.clusterStateAtAbort()))
			return step, false
		}
		o.clusterStatus.UpdateLeader(newLeader, 0)
		o.clusterStatus.SetNodeStatus(newLeader, "LEADER", true)

		if !o.rollOneNodeStart(log, toName, leader, changed, step, newLeader) {
			return step, false
		}
		step++
	}

	if l := o.cluster.DetectLeader(); l >= 0 {
		o.clusterStatus.UpdateLeader(l, 0)
		o.clusterStatus.SetNodeStatus(l, "LEADER", true)
	}
	return step, true
}

// rollOneNode stops a follower, restarts its driver if that changed, then starts
// it and waits for rejoin + catch-up to leader. Returns false (after Finishing
// the slot) on a hard failure.
func (o *OperationsService) rollOneNode(log *slog.Logger, toName string, nodeId int, changed map[string]bool, step, leader int) bool {
	label := fmt.Sprintf("Node %d", nodeId)
	o.progress.Update(step, "Stopping "+label+" for profile roll...")
	o.clusterStatus.SetNodeStatus(nodeId, "STOPPING", false)
	o.stopService(o.cluster.NodeName(nodeId))
	o.waitForNodeStopped(log, nodeId, 15*time.Second)
	o.clusterStatus.SetNodeStatus(nodeId, "OFFLINE", false)

	return o.rollOneNodeStart(log, toName, nodeId, changed, step, leader)
}

// rollOneNodeStart is the shared start half: restart the node's driver if its
// spec changed (safe while the node is down), start the node onto the new
// catalog, and gate on rejoin + catch-up. catchUpLeader < 0 skips the catch-up
// wait (transient election); the node is still confirmed up via its ingress port.
func (o *OperationsService) rollOneNodeStart(log *slog.Logger, toName string, nodeId int, changed map[string]bool, step, catchUpLeader int) bool {
	label := fmt.Sprintf("Node %d", nodeId)

	if !o.cluster.Embedded {
		if changed[o.cluster.DriverName(nodeId)] {
			o.progress.Update(step, label+": restarting media driver onto "+toName+"...")
			o.restartService(o.cluster.DriverName(nodeId))
		}
		o.cleanNodeMediaDriver(log, nodeId)
	}

	o.progress.Update(step, "Starting "+label+" on "+toName+"...")
	o.clusterStatus.SetNodeStatus(nodeId, "STARTING", false)
	o.startService(o.cluster.NodeName(nodeId))

	o.progress.Update(step, label+": waiting to rejoin the cluster...")
	o.clusterStatus.SetNodeStatus(nodeId, "REJOINING", true)
	ingressPort := o.cluster.IngressPort(nodeId)
	if !o.waitForPort("127.0.0.1", ingressPort, 60*time.Second) {
		o.clusterStatus.SetNodeStatus(nodeId, "OFFLINE", false)
		o.progress.Finish(false, fmt.Sprintf("%s did not rejoin within 60s — ABORTED before touching more nodes; "+
			"%s; investigate node%d then re-apply %s with force", label, o.clusterStateAtAbort(), nodeId, toName))
		return false
	}
	if catchUpLeader >= 0 && catchUpLeader != nodeId {
		o.progress.Update(step, label+": waiting for log catch-up...")
		if !o.waitForFollowerCatchUp(log, nodeId, catchUpLeader, 60*time.Second) {
			o.progress.Finish(false, fmt.Sprintf("%s rejoined but did not catch up within 60s — ABORTED; %s",
				label, o.clusterStateAtAbort()))
			return false
		}
	}
	o.clusterStatus.SetNodeStatus(nodeId, "FOLLOWER", true)
	return true
}
