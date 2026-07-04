package services

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/match/admin-gateway/config"
	"github.com/match/admin-gateway/logging"
)

// OperationsService handles complex cluster operations
type OperationsService struct {
	cfg           *config.Config
	systemd       *Systemd
	cluster       *Cluster
	progress      *Progress
	clusterStatus *ClusterStatus
	procMgr       *ProcessManager
	statusSvc     *StatusService
	log           *slog.Logger
}

func NewOperationsService(cfg *config.Config, systemd *Systemd, cluster *Cluster, progress *Progress, status *ClusterStatus) *OperationsService {
	return &OperationsService{
		cfg:           cfg,
		systemd:       systemd,
		cluster:       cluster,
		progress:      progress,
		clusterStatus: status,
		log:           logging.Component("ops"),
	}
}

// SetProcessManager injects the process manager (avoids circular init)
func (o *OperationsService) SetProcessManager(pm *ProcessManager) {
	o.procMgr = pm
}

// SetStatusService injects the status service for the archive-op lag guard.
func (o *OperationsService) SetStatusService(s *StatusService) {
	o.statusSvc = s
}

// archiveOpBlockReason returns a non-empty reason when snapshot/housekeeping
// would risk stranding a member (match#35): housekeeping purges log segments
// below the latest snapshot, so running either while any node is down,
// wedged, or lagging permanently strands that node (2026-07-02 incident).
// Judged with the truthful per-node health from #16.
func (o *OperationsService) archiveOpBlockReason() string {
	if o.statusSvc == nil {
		return ""
	}
	s := o.statusSvc.GetStatus()
	if all, ok := s["allNodesHealthy"].(bool); ok && all {
		return ""
	}
	detail := ""
	if nodes, ok := s["nodes"].([]map[string]interface{}); ok {
		for _, n := range nodes {
			if h, _ := n["health"].(string); h != "" && h != "HEALTHY" {
				detail += fmt.Sprintf(" node%v=%s", n["id"], h)
			}
		}
	}
	if detail == "" {
		detail = " (health fields unavailable)"
	}
	return "cluster not fully healthy:" + detail
}

// isNodeRunning checks if a node is running via ProcessManager
func (o *OperationsService) isNodeRunning(nodeId int) bool {
	if o.procMgr == nil {
		return false // No PM means can't determine state
	}
	info := o.procMgr.Get(fmt.Sprintf("node%d", nodeId))
	return info != nil && info.Running
}

// startService starts a service via process manager
func (o *OperationsService) startService(name string) {
	if o.procMgr == nil {
		o.log.Error("process manager not initialized, cannot start service", "service", name)
		return
	}
	if err := o.procMgr.startByName(name); err != nil {
		o.log.Error("start service failed", "service", name, "err", err)
	}
}

// stopService stops a service via process manager
func (o *OperationsService) stopService(name string) {
	if o.procMgr == nil {
		o.log.Error("process manager not initialized, cannot stop service", "service", name)
		return
	}
	if err := o.procMgr.stopProcess(name, true); err != nil {
		o.log.Error("stop service failed", "service", name, "err", err)
	}
}

// restartService restarts a service via process manager
func (o *OperationsService) restartService(name string) {
	if o.procMgr == nil {
		o.log.Error("process manager not initialized, cannot restart service", "service", name)
		return
	}
	if err := o.procMgr.Restart(name); err != nil {
		o.log.Error("restart service failed", "service", name, "err", err)
	}
}

// RollingUpdate performs a rolling update of all cluster nodes
func (o *OperationsService) RollingUpdate() error {
	if !o.progress.TryStart("rolling-update", 11) {
		return fmt.Errorf("another operation in progress")
	}

	go o.doRollingUpdate()
	return nil
}

// stageClusterJar builds match-cluster in an isolated copy of the project
// tree and copies the result into staging. No shell involved: every step is
// an arg-vector exec or a direct filesystem call (admin-gateway#11). The
// temp build dir is removed on every exit path.
func (o *OperationsService) stageClusterJar(tempBuildDir, stagingDir, stagingJar string) error {
	if err := os.RemoveAll(tempBuildDir); err != nil {
		return fmt.Errorf("clean temp build dir: %w", err)
	}
	if err := os.MkdirAll(tempBuildDir, 0o755); err != nil {
		return fmt.Errorf("create temp build dir: %w", err)
	}
	defer os.RemoveAll(tempBuildDir)
	if err := os.MkdirAll(stagingDir, 0o755); err != nil {
		return fmt.Errorf("create staging dir: %w", err)
	}
	rsync := exec.Command("rsync", "-a",
		"--exclude=*/target", "--exclude=.git", "--exclude=admin-gateway",
		"--exclude=backup", "--exclude=binaries", "--exclude=binance-replay",
		o.cfg.ProjectDir+"/", tempBuildDir+"/")
	if output, err := rsync.CombinedOutput(); err != nil {
		return fmt.Errorf("rsync: %v: %s", err, output)
	}
	mvn := exec.Command("mvn", "package", "-pl", "match-cluster", "-am", "-DskipTests", "-q")
	mvn.Dir = tempBuildDir
	if output, err := mvn.CombinedOutput(); err != nil {
		return fmt.Errorf("mvn: %v: %s", err, output)
	}
	cp := exec.Command("cp", filepath.Join(tempBuildDir, "match-cluster/target/match-cluster.jar"), stagingJar)
	if output, err := cp.CombinedOutput(); err != nil {
		return fmt.Errorf("copy staged jar: %v: %s", err, output)
	}
	return nil
}

func (o *OperationsService) doRollingUpdate() {
	log := o.log.With("op", "rolling-update", "op_id", o.progress.CurrentOpID())
	jarPath := o.cfg.JarPath
	stagingDir := filepath.Join(o.cfg.ProjectDir, "match-cluster/target/staging")
	stagingJar := filepath.Join(stagingDir, "match-cluster.jar")

	// Step 1: Build in isolated directory (NEVER touch live JAR)
	// Multi-module: copy entire project tree (excluding target dirs), build match-cluster
	o.progress.Update(1, "Building cluster module in isolated directory...")
	buildId := fmt.Sprintf("%d", time.Now().UnixMilli())
	tempBuildDir := "/tmp/match-rolling-build-" + buildId

	if err := o.stageClusterJar(tempBuildDir, stagingDir, stagingJar); err != nil {
		o.progress.Finish(false, "Build failed: "+err.Error())
		return
	}

	if _, err := os.Stat(stagingJar); os.IsNotExist(err) {
		o.progress.Finish(false, "Build failed: staging JAR not found")
		return
	}

	o.progress.Update(1, "Build complete, staged for deployment")

	// Step 2: Find leader
	o.progress.Update(2, "Finding cluster leader...")
	leader := o.cluster.DetectLeader()
	if leader < 0 {
		leader = o.clusterStatus.GetLeaderId()
	}
	if leader < 0 {
		o.progress.Finish(false, "Could not find cluster leader")
		return
	}

	// Get followers
	followers := []int{}
	for i := 0; i < 3; i++ {
		if i != leader {
			followers = append(followers, i)
		}
	}

	jarSwapped := false
	step := 3

	// Steps 3-8: Update followers
	for _, nodeId := range followers {
		nodeLabel := fmt.Sprintf("Node %d", nodeId)

		// Stop follower
		o.progress.Update(step, "Stopping "+nodeLabel+"...")
		o.clusterStatus.SetNodeStatus(nodeId, "STOPPING", false)
		o.stopService(fmt.Sprintf("node%d", nodeId))
		o.waitForNodeStopped(log, nodeId, 15*time.Second)
		o.clusterStatus.SetNodeStatus(nodeId, "OFFLINE", false)
		step++

		// Swap JAR after first node is stopped
		if !jarSwapped {
			o.progress.Update(step, "Deploying new JAR...")
			exec.Command("mv", stagingJar, jarPath).Run()
			jarSwapped = true
			time.Sleep(100 * time.Millisecond)
		}

		// Clean stale MediaDriver directory for this node
		o.cleanNodeMediaDriver(log, nodeId)

		// Start follower with new code
		o.progress.Update(step, "Starting "+nodeLabel+" with new code...")
		o.clusterStatus.SetNodeStatus(nodeId, "STARTING", false)
		o.startService(fmt.Sprintf("node%d", nodeId))
		step++

		// Wait for the node to actually rejoin — verify via ingress port.
		// HARD-FAIL on timeout (#10): proceeding to stop the NEXT node while
		// this one is down or still replaying drops the cluster below quorum
		// and can wedge the election. Aborting here leaves 2/3 quorum intact
		// for the operator to investigate.
		o.progress.Update(step, nodeLabel+": Waiting to rejoin cluster...")
		o.clusterStatus.SetNodeStatus(nodeId, "REJOINING", true)
		ingressPort := 9000 + (nodeId * 100) + 2 // 9002, 9102, 9202
		if !o.waitForPort("127.0.0.1", ingressPort, 60*time.Second) {
			o.clusterStatus.SetNodeStatus(nodeId, "OFFLINE", false)
			o.progress.Finish(false, fmt.Sprintf(
				"%s did not rejoin within 60s — ABORTED before touching more nodes; "+
					"cluster keeps quorum (2/3), investigate node%d then re-run rolling-update", nodeLabel, nodeId))
			return
		}
		// Wait for the node to catch up to the leader's commit position before moving on.
		// Otherwise we may stop the next node while this one is still replaying the log,
		// transiently dropping the cluster below quorum (2/3).
		o.progress.Update(step, nodeLabel+": Waiting for log catch-up...")
		if !o.waitForFollowerCatchUp(log, nodeId, leader, 60*time.Second) {
			o.progress.Finish(false, fmt.Sprintf(
				"%s rejoined but did not catch up to the leader within 60s — ABORTED before "+
					"touching more nodes; cluster keeps quorum (2/3), investigate node%d then re-run", nodeLabel, nodeId))
			return
		}
		o.progress.Update(step, nodeLabel+" caught up, marking as follower")
		o.clusterStatus.SetNodeStatus(nodeId, "FOLLOWER", true)
		step++
	}

	// Step 9: Stop old leader
	o.progress.Update(9, fmt.Sprintf("Stopping Node %d (Leader)...", leader))
	o.clusterStatus.SetNodeStatus(leader, "STOPPING", false)
	for _, nodeId := range followers {
		o.clusterStatus.SetNodeStatus(nodeId, "ELECTION", true)
	}
	o.stopService(fmt.Sprintf("node%d", leader))
	o.waitForNodeStopped(log, leader, 15*time.Second)
	o.clusterStatus.SetNodeStatus(leader, "OFFLINE", false)

	// Clean stale MediaDriver directory for old leader
	o.cleanNodeMediaDriver(log, leader)

	// Step 10: Wait for new leader election — verify by checking ingress ports
	o.progress.Update(10, "Waiting for leader election...")
	electionOk := false
	for attempt := 0; attempt < 30; attempt++ {
		time.Sleep(2 * time.Second)
		newLeader := o.cluster.DetectLeader()
		if newLeader >= 0 {
			o.clusterStatus.UpdateLeader(newLeader, 0)
			for _, nodeId := range followers {
				if nodeId == newLeader {
					o.clusterStatus.SetNodeStatus(nodeId, "LEADER", true)
				} else {
					o.clusterStatus.SetNodeStatus(nodeId, "FOLLOWER", true)
				}
			}
			o.progress.Update(10, fmt.Sprintf("New leader elected: Node %d", newLeader))
			electionOk = true
			break
		}
	}
	if !electionOk {
		// Fallback: check if ingress ports are open
		for _, nodeId := range followers {
			port := 9000 + (nodeId * 100) + 2
			if o.isPortOpen("127.0.0.1", port) {
				o.clusterStatus.SetNodeStatus(nodeId, "LEADER", true)
				o.clusterStatus.UpdateLeader(nodeId, 0)
				o.progress.Update(10, fmt.Sprintf("Leader detected via ingress: Node %d", nodeId))
				electionOk = true
				break
			}
		}
	}
	if !electionOk {
		o.progress.Finish(false, "Leader election failed after 60s — cluster may need manual recovery")
		return
	}

	// Step 11: Start old leader as follower
	o.progress.Update(11, fmt.Sprintf("Starting Node %d as follower...", leader))
	o.clusterStatus.SetNodeStatus(leader, "STARTING", false)
	o.startService(fmt.Sprintf("node%d", leader))

	// Wait for old leader to rejoin AND catch up to new leader's commit position.
	// HARD-FAIL if it doesn't (#10): the update deployed, but reporting success
	// with a member down/lagging hides a degraded cluster from the operator.
	o.clusterStatus.SetNodeStatus(leader, "REJOINING", true)
	ingressPort := 9000 + (leader * 100) + 2
	newLeader := o.cluster.DetectLeader()
	if !o.waitForPort("127.0.0.1", ingressPort, 60*time.Second) {
		o.clusterStatus.SetNodeStatus(leader, "OFFLINE", false)
		exec.Command("rm", "-rf", stagingDir).Run()
		o.progress.Finish(false, fmt.Sprintf(
			"New code deployed on all nodes, but node%d (old leader) did not rejoin within 60s — "+
				"cluster running at 2/3, investigate node%d", leader, leader))
		return
	}
	if newLeader < 0 || !o.waitForFollowerCatchUp(log, leader, newLeader, 60*time.Second) {
		o.clusterStatus.SetNodeStatus(leader, "FOLLOWER", true)
		exec.Command("rm", "-rf", stagingDir).Run()
		o.progress.Finish(false, fmt.Sprintf(
			"New code deployed on all nodes, but node%d (old leader) rejoined without confirmed "+
				"catch-up within 60s — verify commit positions before further operations", leader))
		return
	}
	o.progress.Update(11, fmt.Sprintf("Node %d rejoined and caught up", leader))
	o.clusterStatus.SetNodeStatus(leader, "FOLLOWER", true)

	// Cleanup staging
	exec.Command("rm", "-rf", stagingDir).Run()

	o.progress.Finish(true, "All nodes updated successfully with new code")
}

// RebuildAdmin builds the admin gateway binary from source and restarts itself via systemd.
// The flow: build to staging → swap binary → systemd restart (which kills us gracefully).
func (o *OperationsService) RebuildAdmin() error {
	if !o.progress.TryStart("rebuild-admin", 4) {
		return fmt.Errorf("another operation in progress")
	}

	go o.doRebuildAdmin()
	return nil
}

func (o *OperationsService) doRebuildAdmin() {
	// AdminDir, not ProjectDir/admin-gateway: this repo split out of match, so the
	// old path builds a checkout that no longer exists.
	adminDir := o.cfg.AdminDir
	liveBinary := filepath.Join(adminDir, "admin-gateway")
	stagingBinary := filepath.Join(adminDir, "admin-gateway.staging")

	// Step 1: Build new binary to staging path (never overwrite live binary directly)
	o.progress.Update(1, "Building admin gateway from source...")
	cmd := exec.Command("go", "build", "-o", stagingBinary, ".")
	cmd.Dir = adminDir
	if output, err := cmd.CombinedOutput(); err != nil {
		os.Remove(stagingBinary)
		o.progress.Finish(false, "Build failed: "+err.Error()+" output: "+string(output))
		return
	}

	// Step 2: Verify the staged binary is valid (basic sanity: exists + executable)
	o.progress.Update(2, "Verifying staged binary...")
	info, err := os.Stat(stagingBinary)
	if err != nil {
		o.progress.Finish(false, "Staged binary not found after build: "+err.Error())
		return
	}
	if info.Size() < 1024 {
		os.Remove(stagingBinary)
		o.progress.Finish(false, "Staged binary suspiciously small, aborting")
		return
	}
	stagedSha, err := fileSha256(stagingBinary)
	if err != nil {
		os.Remove(stagingBinary)
		o.progress.Finish(false, "Cannot hash staged binary: "+err.Error())
		return
	}

	// Step 3: Atomic swap — rename staging over live binary. Keep the old
	// binary as .prev: the manual rollback path when the new one won't boot.
	o.progress.Update(3, "Swapping binary (atomic rename)...")
	log := o.log.With("op", "rebuild-admin", "op_id", o.progress.CurrentOpID())
	if err := copyFile(liveBinary, liveBinary+".prev"); err != nil {
		log.Warn("could not preserve previous binary as .prev", "err", err)
	} else {
		os.Chmod(liveBinary+".prev", 0755) // copyFile does not preserve the exec bit
	}
	if err := os.Rename(stagingBinary, liveBinary); err != nil {
		os.Remove(stagingBinary)
		o.progress.Finish(false, "Binary swap failed: "+err.Error())
		return
	}

	// Step 4: Arm the post-restart verification handshake, then restart.
	// The systemd restart kills THIS process, so completion is reported by
	// the NEW process (FinalizeRebuildVerification): poll
	// GET /api/admin/rebuild-status until state=verified.
	o.progress.Update(4, "Restarting admin service...")
	pending := rebuildPending{
		StartedAt:    time.Now().Format(time.RFC3339),
		OpID:         o.progress.CurrentOpID(),
		StagedSha256: stagedSha,
		OldPid:       os.Getpid(),
	}
	if err := writeJSONAtomic(filepath.Join(adminDir, rebuildPendingFile), pending); err != nil {
		log.Warn("could not write rebuild-pending.json, restart proceeds unverified", "err", err)
	}
	o.progress.Finish(true,
		"Admin gateway rebuilt. Restarting now — poll /api/admin/rebuild-status for post-restart verification")

	// Small delay so the progress response can be read by any polling clients
	time.Sleep(500 * time.Millisecond)

	// This is the kill-switch: systemd restarts us with the new binary
	o.systemd.Restart("admin")
}

// Snapshot triggers a cluster snapshot
func (o *OperationsService) Snapshot(force bool) error {
	if !o.progress.TryStart("snapshot", 7) {
		return fmt.Errorf("another operation in progress")
	}
	if !force {
		if reason := o.archiveOpBlockReason(); reason != "" {
			// Release the claimed slot — a guard refusal that kept it wedged
			// EVERY subsequent operation until an admin restart.
			o.progress.Finish(false, "refused: "+reason)
			return fmt.Errorf("refusing snapshot: %s (match#35 lag guard; POST {\"force\":true} to override)", reason)
		}
	}

	go o.doSnapshot()
	return nil
}

func (o *OperationsService) doSnapshot() {
	log := o.log.With("op", "snapshot", "op_id", o.progress.CurrentOpID())
	// Step 1: Find leader
	o.progress.Update(1, "Finding cluster leader...")
	leader := o.cluster.DetectLeader()
	if leader < 0 {
		o.progress.Finish(false, "Could not find cluster leader")
		return
	}

	// Step 2: Take snapshot
	o.progress.Update(2, fmt.Sprintf("Taking snapshot on Node %d...", leader))
	output, err := o.cluster.TakeSnapshot(leader)
	if err != nil {
		o.progress.Finish(false, "Snapshot failed: "+err.Error())
		return
	}

	// Step 3: Wait for propagation
	o.progress.Update(3, "Waiting for snapshot propagation...")
	time.Sleep(2 * time.Second)

	// Step 4: Verify
	o.progress.Update(4, "Verifying snapshot position...")
	pos := o.cluster.GetSnapshotPosition(leader)

	if pos < 0 || (!contains(output, "SNAPSHOT") && !contains(output, "completed")) {
		o.progress.Finish(false, "Snapshot may have failed: "+output)
		return
	}

	// Steps 5-7: Reclaim archive disk on each node. The snapshot makes the log
	// below its position unnecessary, but Aeron never reclaims automatically —
	// purge log segments and superseded snapshots while the cluster runs.
	housekeepingFailures := 0
	for i := 0; i < 3; i++ {
		o.progress.Update(5+i, fmt.Sprintf("Reclaiming archive on Node %d...", i))
		hkOutput, hkErr := o.cluster.ArchiveHousekeeping(i)
		log.Info("node housekeeping output", "node", i, "output", hkOutput)
		if hkErr != nil {
			housekeepingFailures++
			log.Warn("housekeeping failed on node", "node", i, "err", hkErr)
		}
	}

	if housekeepingFailures > 0 {
		o.progress.Finish(true, fmt.Sprintf(
			"Snapshot created at position %d, but archive reclamation failed on %d node(s) — check logs",
			pos, housekeepingFailures))
		return
	}
	o.progress.Finish(true, fmt.Sprintf("Snapshot created at position %d, archives reclaimed", pos))
}

// Housekeeping reclaims archive disk on all nodes without taking a snapshot
// (uses the latest existing snapshot as the purge boundary).
func (o *OperationsService) Housekeeping(force bool) error {
	if !o.progress.TryStart("housekeeping", 3) {
		return fmt.Errorf("another operation in progress")
	}
	if !force {
		if reason := o.archiveOpBlockReason(); reason != "" {
			o.progress.Finish(false, "refused: "+reason)
			return fmt.Errorf("refusing housekeeping: %s (match#35 lag guard; POST {\"force\":true} to override)", reason)
		}
	}

	go o.doHousekeeping()
	return nil
}

func (o *OperationsService) doHousekeeping() {
	log := o.log.With("op", "housekeeping", "op_id", o.progress.CurrentOpID())
	failures := 0
	for i := 0; i < 3; i++ {
		o.progress.Update(1+i, fmt.Sprintf("Reclaiming archive on Node %d...", i))
		output, err := o.cluster.ArchiveHousekeeping(i)
		log.Info("node housekeeping output", "node", i, "output", output)
		if err != nil {
			failures++
			log.Warn("housekeeping failed on node", "node", i, "err", err)
		}
	}

	if failures > 0 {
		o.progress.Finish(false, fmt.Sprintf("Archive reclamation failed on %d node(s) — check logs", failures))
		return
	}
	o.progress.Finish(true, "Archives reclaimed on all nodes")
}

// RebuildGateway builds the gateway module and optionally restarts gateways.
// This is SAFE while the cluster is running since gateway JAR is separate from cluster JAR.
func (o *OperationsService) RebuildGateway(restart bool) error {
	totalSteps := 2
	if restart {
		totalSteps = 3
	}
	if !o.progress.TryStart("rebuild-gateway", totalSteps) {
		return fmt.Errorf("another operation in progress")
	}

	go o.doRebuildGateway(restart)
	return nil
}

func (o *OperationsService) doRebuildGateway(restart bool) {
	// Step 1: Build gateway module (safe - separate JAR from cluster)
	o.progress.Update(1, "Building gateway module...")
	cmd := exec.Command("mvn", "package", "-pl", "match-gateway", "-am", "-DskipTests", "-q")
	cmd.Dir = o.cfg.ProjectDir
	output, err := cmd.CombinedOutput()
	if err != nil {
		o.progress.Finish(false, "Gateway build failed: "+err.Error()+" output: "+string(output))
		return
	}

	// Step 2: Verify JAR exists
	o.progress.Update(2, "Verifying gateway JAR...")
	if _, err := os.Stat(o.cfg.GatewayJar); os.IsNotExist(err) {
		o.progress.Finish(false, "Build succeeded but JAR not found at: "+o.cfg.GatewayJar)
		return
	}

	if !restart {
		o.progress.Finish(true, "Gateway JAR rebuilt successfully")
		return
	}

	// Step 3: Restart gateways (oms + market, not admin since we'd kill ourselves)
	o.progress.Update(3, "Restarting OMS & market gateways...")
	for _, svc := range []string{"oms", "market"} {
		o.restartService(svc)
	}
	time.Sleep(3 * time.Second)

	o.progress.Finish(true, "Gateway rebuilt and restarted successfully")
}

// RebuildCluster builds the cluster module to staging (does NOT deploy).
// WARNING: The built JAR goes to staging, NOT the live location.
// Use rolling-update to deploy, or manually swap the JAR.
func (o *OperationsService) RebuildCluster() error {
	if !o.progress.TryStart("rebuild-cluster", 3) {
		return fmt.Errorf("another operation in progress")
	}

	go o.doRebuildCluster()
	return nil
}

func (o *OperationsService) doRebuildCluster() {
	stagingDir := filepath.Join(o.cfg.ProjectDir, "match-cluster/target/staging")
	stagingJar := filepath.Join(stagingDir, "match-cluster.jar")

	// Step 1: Build cluster module in isolated directory
	o.progress.Update(1, "Building cluster module in isolated directory...")
	buildId := fmt.Sprintf("%d", time.Now().UnixMilli())
	tempBuildDir := "/tmp/match-cluster-build-" + buildId

	if err := o.stageClusterJar(tempBuildDir, stagingDir, stagingJar); err != nil {
		o.progress.Finish(false, "Cluster build failed: "+err.Error())
		return
	}

	// Step 2: Verify staging JAR
	o.progress.Update(2, "Verifying staged cluster JAR...")
	if _, err := os.Stat(stagingJar); os.IsNotExist(err) {
		o.progress.Finish(false, "Build succeeded but staging JAR not found")
		return
	}

	// Step 3: Report
	o.progress.Update(3, "Cluster JAR built and staged")
	o.progress.Finish(true,
		fmt.Sprintf("Cluster JAR built to staging: %s. Use rolling-update to deploy.", stagingJar))
}

// CleanupOptions configures the cleanup operation
type CleanupOptions struct {
	Force  bool `json:"force"`
	DryRun bool `json:"dryRun"`
	Backup bool `json:"backup"`
	// Archives are PRESERVED by default (#10). Wiping them additionally
	// requires ConfirmArchiveLoss to spell out the exact phrase below.
	IncludeArchive     bool   `json:"includeArchive"`
	ConfirmArchiveLoss string `json:"confirmArchiveLoss"`
}

// The second confirmation required to wipe cluster archives via /cleanup.
const archiveLossConfirmation = "DELETE-CLUSTER-STATE"

// cleanupSweep removes Aeron IPC dirs, mark files, and locks under shmDir and
// tmpDir. Cluster archives (shmDir/aeron-cluster) are PRESERVED unless
// includeArchive: /cleanup used to run `rm -rf /dev/shm/aeron-*`, and that
// glob matches aeron-cluster — nuking the very archives P1.3 makes durable
// (#10). apply=false only reports. Factored out with configurable roots so the
// preserve guarantee is unit-testable.
func cleanupSweep(shmDir, tmpDir string, includeArchive, apply bool) (cleaned, preserved, errs []string) {
	remove := func(path string, all bool) {
		cleaned = append(cleaned, path)
		if !apply {
			return
		}
		var err error
		if all {
			err = os.RemoveAll(path)
		} else {
			err = os.Remove(path)
		}
		if err != nil && !os.IsNotExist(err) {
			errs = append(errs, path+": "+err.Error())
		}
	}

	// 1. Aeron IPC dirs (drivers, clients) — everything aeron-* EXCEPT the
	// cluster state dir, which the old glob wrongly swallowed.
	entries, _ := filepath.Glob(filepath.Join(shmDir, "aeron-*"))
	for _, e := range entries {
		if filepath.Base(e) == "aeron-cluster" {
			continue
		}
		remove(e, true)
	}

	// 2. Stale mark/lock files inside the cluster state dirs (node*, backup)
	for _, pattern := range []string{
		"aeron-cluster/*/cluster/cluster-mark*.dat",
		"aeron-cluster/*/cluster/*.lck",
		"aeron-cluster/*/archive/archive-mark.dat",
	} {
		matches, _ := filepath.Glob(filepath.Join(shmDir, pattern))
		for _, m := range matches {
			remove(m, false)
		}
	}

	// 3. The archives themselves: preserved unless explicitly included
	recordings, _ := filepath.Glob(filepath.Join(shmDir, "aeron-cluster/*/archive/*.rec"))
	clusterDir := filepath.Join(shmDir, "aeron-cluster")
	if includeArchive {
		if _, err := os.Stat(clusterDir); err == nil {
			cleaned = append(cleaned, fmt.Sprintf("%s (FULL WIPE incl. %d recording(s))", clusterDir, len(recordings)))
			if apply {
				if err := os.RemoveAll(clusterDir); err != nil && !os.IsNotExist(err) {
					errs = append(errs, clusterDir+": "+err.Error())
				}
			}
		}
	} else if len(recordings) > 0 {
		preserved = append(preserved, fmt.Sprintf(
			"%d archive recording(s) under %s preserved — pass includeArchive=true with confirmArchiveLoss=%q to wipe",
			len(recordings), clusterDir, archiveLossConfirmation))
	}

	// 4. Gateway/client Aeron dirs under tmp
	tmpEntries, _ := filepath.Glob(filepath.Join(tmpDir, "aeron-*"))
	for _, e := range tmpEntries {
		remove(e, true)
	}

	return cleaned, preserved, errs
}

// Cleanup removes stale Aeron files (requires all nodes stopped and force=true)
func (o *OperationsService) Cleanup(opts CleanupOptions) map[string]interface{} {
	result := map[string]interface{}{}

	// Dry-run changes nothing: allow it anytime (even with nodes running) so
	// ops can preview the sweep and the archive-preservation notice.
	if opts.DryRun {
		wouldClean, preserved, _ := cleanupSweep("/dev/shm", "/tmp", opts.IncludeArchive, false)
		result["success"] = true
		result["dryRun"] = true
		result["wouldClean"] = wouldClean
		if len(preserved) > 0 {
			result["preserved"] = preserved
		}
		return result
	}

	// Require force flag for destructive operation
	if !opts.Force {
		result["success"] = false
		result["error"] = "Destructive operation requires force=true"
		return result
	}

	// Check if any nodes are running via ProcessManager
	for i := 0; i < 3; i++ {
		if o.isNodeRunning(i) {
			result["success"] = false
			result["error"] = fmt.Sprintf("Node %d is still running. Stop all nodes before cleanup.", i)
			return result
		}
	}

	// External media drivers own /dev/shm/aeron-<user>-N-driver, which the wipe below
	// deletes — they must be stopped too or their IPC files are pulled out from under them
	if o.procMgr != nil {
		for i := 0; i < 3; i++ {
			if info := o.procMgr.Get(fmt.Sprintf("driver%d", i)); info != nil && info.Running {
				result["success"] = false
				result["error"] = fmt.Sprintf("Media driver %d is still running. Stop all drivers before cleanup.", i)
				return result
			}
		}
	}

	// Wiping archives destroys the recovery source (#10): demand an explicit
	// second confirmation beyond force=true.
	if opts.IncludeArchive && opts.ConfirmArchiveLoss != archiveLossConfirmation {
		result["success"] = false
		result["error"] = fmt.Sprintf(
			"includeArchive=true wipes ALL cluster archives; set confirmArchiveLoss=%q to confirm",
			archiveLossConfirmation)
		return result
	}

	// Backup mark files before cleanup if requested
	if opts.Backup {
		backupPath := o.backupMarkFiles()
		result["backupCreated"] = backupPath
	}

	cleaned, preserved, errors := cleanupSweep("/dev/shm", "/tmp", opts.IncludeArchive, true)

	result["success"] = len(errors) == 0
	result["cleaned"] = cleaned
	if len(preserved) > 0 {
		result["preserved"] = preserved
	}
	if len(errors) > 0 {
		result["errors"] = errors
		result["message"] = "Cleanup completed with some errors."
	} else {
		result["message"] = "Cleanup completed successfully. You can now start the cluster."
	}

	return result
}

// CleanupNode removes stale Aeron files for a single node
func (o *OperationsService) CleanupNode(nodeId int, force, dryRun bool) map[string]interface{} {
	result := map[string]interface{}{"nodeId": nodeId}

	if nodeId < 0 || nodeId > 2 {
		result["success"] = false
		result["error"] = "Invalid nodeId (must be 0, 1, or 2)"
		return result
	}

	if !force {
		result["success"] = false
		result["error"] = "Destructive operation requires force=true"
		return result
	}

	if o.isNodeRunning(nodeId) {
		result["success"] = false
		result["error"] = fmt.Sprintf("Node %d is still running", nodeId)
		return result
	}

	nodeDir := fmt.Sprintf("%s/node%d", o.cfg.ClusterDir, nodeId)
	files := []string{
		nodeDir + "/cluster/cluster-mark*.dat",
		nodeDir + "/cluster/*.lck",
		nodeDir + "/archive/archive-mark.dat",
	}

	if dryRun {
		result["success"] = true
		result["dryRun"] = true
		result["wouldClean"] = files
		return result
	}

	// Clean files — glob in-process, no shell (admin-gateway#11)
	for _, pattern := range files {
		matches, err := filepath.Glob(pattern)
		if err != nil {
			continue
		}
		for _, match := range matches {
			os.Remove(match)
		}
	}

	result["success"] = true
	result["cleaned"] = files
	result["message"] = fmt.Sprintf("Node %d mark files cleaned", nodeId)
	return result
}

// BackupInfo contains information about available backups
type BackupInfo struct {
	BackupDir       string `json:"backupDir"`
	HasRecordingLog bool   `json:"hasRecordingLog"`
	HasArchive      bool   `json:"hasArchive"`
	RecordingCount  int    `json:"recordingCount"`
	// Freshness (match#36 / #9): is the backup actually tracking the leader?
	RecordingLogBytes  int64            `json:"recordingLogBytes"`
	CatalogModifiedAgo int64            `json:"catalogModifiedAgoSec"` // -1 when absent
	Heartbeat          *BackupHeartbeat `json:"heartbeat,omitempty"`
	Fresh              bool             `json:"fresh"`
	FreshReason        string           `json:"freshReason"`
}

// BackupHeartbeat mirrors backup-progress.json written by ClusterBackupApp's
// watchdog (match-cluster) every 5s next to the backup data.
type BackupHeartbeat struct {
	Pid                 int64  `json:"pid"`
	StartedEpochMs      int64  `json:"startedEpochMs"`
	UpdatedEpochMs      int64  `json:"updatedEpochMs"`
	LastProgressEpochMs int64  `json:"lastProgressEpochMs"`
	LastQueryEpochMs    int64  `json:"lastQueryEpochMs"`
	LastResponseEpochMs int64  `json:"lastResponseEpochMs"`
	LastLiveLogEpochMs  int64  `json:"lastLiveLogEpochMs"`
	LiveLogPosition     int64  `json:"liveLogPosition"`
	SnapshotsRetrieved  int64  `json:"snapshotsRetrieved"`
	StallWarnings       int64  `json:"stallWarnings"`
	State               string `json:"state"`
}

// heartbeat must be at most this old to count as live (watchdog writes every 5s)
const backupHeartbeatMaxAgeSec = 30

// BackupFreshness reads the ClusterBackupApp heartbeat and derives whether the
// backup is live and tracking the leader. A "running" backup process proves
// nothing (match#36: the agent wedged silently for days while the process
// looked healthy) — only a recent heartbeat in state OK does.
func BackupFreshness(backupDir string) (fresh bool, reason string, hb *BackupHeartbeat) {
	data, err := os.ReadFile(filepath.Join(backupDir, "backup-progress.json"))
	if err != nil {
		return false, "no heartbeat file (backup app not running, or predates the watchdog)", nil
	}
	hb = &BackupHeartbeat{}
	if err := json.Unmarshal(data, hb); err != nil {
		return false, "unreadable heartbeat: " + err.Error(), nil
	}
	ageSec := (time.Now().UnixMilli() - hb.UpdatedEpochMs) / 1000
	if ageSec > backupHeartbeatMaxAgeSec {
		return false, fmt.Sprintf("heartbeat stale: last written %ds ago (backup process dead or wedged)", ageSec), hb
	}
	if hb.State != "OK" {
		return false, fmt.Sprintf("backup reports state %s (no progress; watchdog about to restart it)", hb.State), hb
	}
	return true, "heartbeat live, backup making progress", hb
}

// GetBackupInfo returns information about backup data availability
func (o *OperationsService) GetBackupInfo() BackupInfo {
	backupDir := filepath.Join(o.cfg.ProjectDir, "backup")
	info := BackupInfo{BackupDir: backupDir, CatalogModifiedAgo: -1}

	if st, err := os.Stat(filepath.Join(backupDir, "cluster/recording.log")); err == nil {
		info.HasRecordingLog = true
		info.RecordingLogBytes = st.Size()
	}
	if st, err := os.Stat(filepath.Join(backupDir, "archive/archive.catalog")); err == nil {
		info.HasArchive = true
		info.CatalogModifiedAgo = int64(time.Since(st.ModTime()).Seconds())
	}
	matches, _ := filepath.Glob(filepath.Join(backupDir, "archive/*.rec"))
	info.RecordingCount = len(matches)

	info.Fresh, info.FreshReason, info.Heartbeat = BackupFreshness(backupDir)

	return info
}

// RecoverFromBackup restores a node's cluster data from the backup directory
func (o *OperationsService) RecoverFromBackup(nodeId int, force, dryRun bool) map[string]interface{} {
	result := map[string]interface{}{"nodeId": nodeId}

	if nodeId < 0 || nodeId > 2 {
		result["success"] = false
		result["error"] = "Invalid nodeId (must be 0, 1, or 2)"
		return result
	}

	if !force {
		result["success"] = false
		result["error"] = "Destructive operation requires force=true"
		return result
	}

	if o.isNodeRunning(nodeId) {
		result["success"] = false
		result["error"] = fmt.Sprintf("Node %d must be stopped before recovery", nodeId)
		return result
	}

	backupDir := filepath.Join(o.cfg.ProjectDir, "backup")
	nodeDir := fmt.Sprintf("%s/node%d", o.cfg.ClusterDir, nodeId)

	// Check backup exists
	if _, err := os.Stat(filepath.Join(backupDir, "archive/archive.catalog")); os.IsNotExist(err) {
		result["success"] = false
		result["error"] = "No backup found at " + backupDir + "/archive/archive.catalog"
		return result
	}

	if dryRun {
		result["success"] = true
		result["dryRun"] = true
		result["source"] = backupDir
		result["target"] = nodeDir
		return result
	}

	// Create directories
	os.MkdirAll(filepath.Join(nodeDir, "cluster"), 0755)
	os.MkdirAll(filepath.Join(nodeDir, "archive"), 0755)

	// Copy archive catalog and recordings
	if err := copyFile(filepath.Join(backupDir, "archive/archive.catalog"),
		filepath.Join(nodeDir, "archive/archive.catalog")); err != nil {
		result["success"] = false
		result["error"] = "Failed to copy archive.catalog: " + err.Error()
		return result
	}

	recFiles, _ := filepath.Glob(filepath.Join(backupDir, "archive/*.rec"))
	for _, src := range recFiles {
		if err := copyFile(src, filepath.Join(nodeDir, "archive", filepath.Base(src))); err != nil {
			result["success"] = false
			result["error"] = "Failed to copy " + filepath.Base(src) + ": " + err.Error()
			return result
		}
	}

	// Copy recording.log if exists
	recordingLogSrc := filepath.Join(backupDir, "cluster/recording.log")
	if _, err := os.Stat(recordingLogSrc); err == nil {
		copyFile(recordingLogSrc, filepath.Join(nodeDir, "cluster/recording.log"))
	}

	// Seed from snapshot
	output, err := o.cluster.SeedRecordingLogFromSnapshot(nodeId)
	if err != nil {
		result["success"] = false
		result["error"] = "SeedRecordingLogFromSnapshot failed: " + err.Error()
		result["output"] = output
		return result
	}

	result["success"] = true
	result["message"] = fmt.Sprintf("Node %d recovered from backup", nodeId)
	result["recordingsCopied"] = len(recFiles)
	return result
}

// backupMarkFiles creates a timestamped backup of mark files before cleanup
func (o *OperationsService) backupMarkFiles() string {
	timestamp := time.Now().Format("20060102-150405")
	backupDir := filepath.Join(o.cfg.ProjectDir, "backup/pre-cleanup", timestamp)
	os.MkdirAll(backupDir, 0755)

	for i := 0; i < 3; i++ {
		nodeDir := fmt.Sprintf("%s/node%d", o.cfg.ClusterDir, i)
		nodeBackup := filepath.Join(backupDir, fmt.Sprintf("node%d", i))
		os.MkdirAll(nodeBackup, 0755)

		files := []string{"cluster/cluster-mark.dat", "cluster/recording.log",
			"archive/archive-mark.dat", "archive/archive.catalog"}
		for _, f := range files {
			src := filepath.Join(nodeDir, f)
			if _, err := os.Stat(src); err == nil {
				copyFile(src, filepath.Join(nodeBackup, filepath.Base(f)))
			}
		}
	}
	return backupDir
}

// copyFile copies a file from src to dst
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()

	_, err = io.Copy(out, in)
	return err
}

// waitForPort polls until a UDP port is open (bound) on the given host
func (o *OperationsService) waitForPort(host string, port int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	addr := net.JoinHostPort(host, strconv.Itoa(port))
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("udp", addr, 500*time.Millisecond)
		if err == nil {
			conn.Close()
			// UDP "connect" always succeeds — check if port is actually bound via ss
			if o.isPortOpen(host, port) {
				return true
			}
		}
		time.Sleep(2 * time.Second)
	}
	return false
}

// isPortOpen checks if a UDP port is bound using ss
func (o *OperationsService) isPortOpen(host string, port int) bool {
	cmd := exec.Command("ss", "-ulnp")
	output, err := cmd.Output()
	if err != nil {
		return false
	}
	target := fmt.Sprintf("%s:%d", host, port)
	return strings.Contains(string(output), target)
}

// waitForNodeStopped waits until a node's process is no longer running
func (o *OperationsService) waitForNodeStopped(log *slog.Logger, nodeId int, timeout time.Duration) {
	service := fmt.Sprintf("node%d", nodeId)
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !o.isNodeRunning(nodeId) {
			return
		}
		time.Sleep(1 * time.Second)
	}
	// Force kill if still running
	log.Warn("node still running after stop timeout, force killing", "node", nodeId)
	if o.procMgr != nil {
		info := o.procMgr.Get(service)
		if info != nil && info.PID > 0 {
			exec.Command("kill", "-9", fmt.Sprintf("%d", info.PID)).Run()
			time.Sleep(1 * time.Second)
		}
	}
}

// cleanNodeMediaDriver removes stale Aeron MediaDriver directories for a node.
// Never touches the dir while an external media driver (driverN) owns it — in external
// mode the driver process survives node restarts and deleting its dir corrupts the IPC.
func (o *OperationsService) cleanNodeMediaDriver(log *slog.Logger, nodeId int) {
	if o.procMgr != nil {
		if info := o.procMgr.Get(fmt.Sprintf("driver%d", nodeId)); info != nil && info.Running {
			return
		}
	}
	driverDir := fmt.Sprintf("/dev/shm/aeron-%s-%d-driver", currentUsername(), nodeId)
	if _, err := os.Stat(driverDir); err == nil {
		os.RemoveAll(driverDir)
		log.Info("cleaned stale media driver dir", "dir", driverDir)
	}
}

// waitForFollowerCatchUp blocks until the follower's cluster commit position is within
// catchUpLagBytes of the leader's commit position, OR the timeout elapses. Returns true if
// caught up, false on timeout. Uses the CnC counters (no JVM spawn) so it's cheap to poll.
//
// Why this matters: rolling update used to advance to the next node as soon as the previous
// follower's ingress port was open. The node was up but might still be replaying the log or
// loading a snapshot. Restarting the next node before catch-up risks losing quorum.
func (o *OperationsService) waitForFollowerCatchUp(log *slog.Logger, followerId, leaderId int, timeout time.Duration) bool {
	const catchUpLagBytes int64 = 1 * 1024 * 1024 // 1 MB lag is fine; term buffer is 16 MB
	counters := NewAeronCounters()
	deadline := time.Now().Add(timeout)
	var lastFollowerPos int64 = -1
	for time.Now().Before(deadline) {
		time.Sleep(500 * time.Millisecond)
		leader, lerr := counters.GetNodeCounters(leaderId)
		follower, ferr := counters.GetNodeCounters(followerId)
		if lerr != nil || ferr != nil || leader.CommitPosition < 0 || follower.CommitPosition < 0 {
			continue // CnC not yet ready; keep polling
		}
		lag := leader.CommitPosition - follower.CommitPosition
		if lag <= catchUpLagBytes {
			log.Info("node caught up to leader", "node", followerId, "lag_bytes", lag)
			return true
		}
		if follower.CommitPosition <= lastFollowerPos && follower.CommitPosition > 0 {
			// Position not advancing — log but keep waiting until timeout.
			log.Warn("node catch-up stalled", "node", followerId, "pos", follower.CommitPosition, "lag_bytes", lag)
		}
		lastFollowerPos = follower.CommitPosition
	}
	return false
}

func contains(s, substr string) bool {
	return strings.Contains(s, substr)
}
