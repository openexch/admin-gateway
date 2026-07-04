package services

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/match/admin-gateway/config"
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
}

func NewOperationsService(cfg *config.Config, systemd *Systemd, cluster *Cluster, progress *Progress, status *ClusterStatus) *OperationsService {
	return &OperationsService{
		cfg:           cfg,
		systemd:       systemd,
		cluster:       cluster,
		progress:      progress,
		clusterStatus: status,
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
		fmt.Printf("[ops] ERROR: ProcessManager not initialized, cannot start %s\n", name)
		return
	}
	if err := o.procMgr.startByName(name); err != nil {
		fmt.Printf("[ops] PM start %s failed: %v\n", name, err)
	}
}

// stopService stops a service via process manager
func (o *OperationsService) stopService(name string) {
	if o.procMgr == nil {
		fmt.Printf("[ops] ERROR: ProcessManager not initialized, cannot stop %s\n", name)
		return
	}
	if err := o.procMgr.stopProcess(name, true); err != nil {
		fmt.Printf("[ops] PM stop %s failed: %v\n", name, err)
	}
}

// restartService restarts a service via process manager
func (o *OperationsService) restartService(name string) {
	if o.procMgr == nil {
		fmt.Printf("[ops] ERROR: ProcessManager not initialized, cannot restart %s\n", name)
		return
	}
	if err := o.procMgr.Restart(name); err != nil {
		fmt.Printf("[ops] PM restart %s failed: %v\n", name, err)
	}
}

// RollingUpdate performs a rolling update of all cluster nodes
func (o *OperationsService) RollingUpdate() error {
	if o.progress.IsRunning() {
		return fmt.Errorf("another operation in progress")
	}

	go o.doRollingUpdate()
	return nil
}

func (o *OperationsService) doRollingUpdate() {
	o.progress.Start("rolling-update", 11)

	jarPath := o.cfg.JarPath
	stagingDir := filepath.Join(o.cfg.ProjectDir, "match-cluster/target/staging")
	stagingJar := filepath.Join(stagingDir, "match-cluster.jar")

	// Step 1: Build in isolated directory (NEVER touch live JAR)
	// Multi-module: copy entire project tree (excluding target dirs), build match-cluster
	o.progress.Update(1, "Building cluster module in isolated directory...")
	buildId := fmt.Sprintf("%d", time.Now().UnixMilli())
	tempBuildDir := "/tmp/match-rolling-build-" + buildId

	buildScript := fmt.Sprintf(`
		rm -rf %s &&
		mkdir -p %s &&
		mkdir -p %s &&
		rsync -a --exclude='*/target' --exclude='.git' --exclude='admin-gateway' --exclude='backup' --exclude='binaries' --exclude='binance-replay' %s/ %s/ &&
		cd %s &&
		mvn package -pl match-cluster -am -DskipTests -q &&
		cp %s/match-cluster/target/match-cluster.jar %s &&
		rm -rf %s
	`, tempBuildDir, tempBuildDir, stagingDir,
		o.cfg.ProjectDir, tempBuildDir,
		tempBuildDir,
		tempBuildDir, stagingJar, tempBuildDir)

	cmd := exec.Command("bash", "-c", buildScript)
	if output, err := cmd.CombinedOutput(); err != nil {
		o.progress.Finish(false, "Build failed: "+err.Error()+" output: "+string(output))
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
		o.waitForNodeStopped(nodeId, 15*time.Second)
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
		o.cleanNodeMediaDriver(nodeId)

		// Start follower with new code
		o.progress.Update(step, "Starting "+nodeLabel+" with new code...")
		o.clusterStatus.SetNodeStatus(nodeId, "STARTING", false)
		o.startService(fmt.Sprintf("node%d", nodeId))
		step++

		// Wait for the node to actually rejoin — verify via ingress port
		o.progress.Update(step, nodeLabel+": Waiting to rejoin cluster...")
		o.clusterStatus.SetNodeStatus(nodeId, "REJOINING", true)
		ingressPort := 9000 + (nodeId * 100) + 2 // 9002, 9102, 9202
		if o.waitForPort("127.0.0.1", ingressPort, 60*time.Second) {
			// Wait for the node to catch up to the leader's commit position before moving on.
			// Otherwise we may stop the next node while this one is still replaying the log,
			// transiently dropping the cluster below quorum (2/3).
			o.progress.Update(step, nodeLabel+": Waiting for log catch-up...")
			if o.waitForFollowerCatchUp(nodeId, leader, 60*time.Second) {
				o.progress.Update(step, nodeLabel+" caught up, marking as follower")
			} else {
				o.progress.Update(step, nodeLabel+" catch-up timeout — continuing (cluster may have transient quorum risk)")
			}
			o.clusterStatus.SetNodeStatus(nodeId, "FOLLOWER", true)
		} else {
			o.progress.Update(step, nodeLabel+" rejoin timeout — continuing")
			o.clusterStatus.SetNodeStatus(nodeId, "FOLLOWER", true)
		}
		step++
	}

	// Step 9: Stop old leader
	o.progress.Update(9, fmt.Sprintf("Stopping Node %d (Leader)...", leader))
	o.clusterStatus.SetNodeStatus(leader, "STOPPING", false)
	for _, nodeId := range followers {
		o.clusterStatus.SetNodeStatus(nodeId, "ELECTION", true)
	}
	o.stopService(fmt.Sprintf("node%d", leader))
	o.waitForNodeStopped(leader, 15*time.Second)
	o.clusterStatus.SetNodeStatus(leader, "OFFLINE", false)

	// Clean stale MediaDriver directory for old leader
	o.cleanNodeMediaDriver(leader)

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
	o.clusterStatus.SetNodeStatus(leader, "REJOINING", true)
	ingressPort := 9000 + (leader * 100) + 2
	newLeader := o.cluster.DetectLeader()
	if o.waitForPort("127.0.0.1", ingressPort, 60*time.Second) {
		if newLeader >= 0 && o.waitForFollowerCatchUp(leader, newLeader, 60*time.Second) {
			o.progress.Update(11, fmt.Sprintf("Node %d rejoined and caught up", leader))
		} else {
			o.progress.Update(11, fmt.Sprintf("Node %d rejoined but catch-up not confirmed", leader))
		}
		o.clusterStatus.SetNodeStatus(leader, "FOLLOWER", true)
	} else {
		o.clusterStatus.SetNodeStatus(leader, "FOLLOWER", true)
		o.progress.Update(11, fmt.Sprintf("Node %d rejoin timeout — may still be catching up", leader))
	}

	// Cleanup staging
	exec.Command("rm", "-rf", stagingDir).Run()

	o.progress.Finish(true, "All nodes updated successfully with new code")
}

// RebuildAdmin builds the admin gateway binary from source and restarts itself via systemd.
// The flow: build to staging → swap binary → systemd restart (which kills us gracefully).
func (o *OperationsService) RebuildAdmin() error {
	if o.progress.IsRunning() {
		return fmt.Errorf("another operation in progress")
	}

	go o.doRebuildAdmin()
	return nil
}

func (o *OperationsService) doRebuildAdmin() {
	o.progress.Start("rebuild-admin", 4)

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

	// Step 3: Atomic swap — rename staging over live binary
	o.progress.Update(3, "Swapping binary (atomic rename)...")
	if err := os.Rename(stagingBinary, liveBinary); err != nil {
		os.Remove(stagingBinary)
		o.progress.Finish(false, "Binary swap failed: "+err.Error())
		return
	}

	// Step 4: Restart ourselves via systemd (this kills us — systemd brings us back with new binary)
	o.progress.Update(4, "Restarting admin service...")
	o.progress.Finish(true, "Admin gateway rebuilt. Restarting now...")

	// Small delay so the progress response can be read by any polling clients
	time.Sleep(500 * time.Millisecond)

	// This is the kill-switch: systemd restarts us with the new binary
	o.systemd.Restart("admin")
}

// Snapshot triggers a cluster snapshot
func (o *OperationsService) Snapshot(force bool) error {
	if o.progress.IsRunning() {
		return fmt.Errorf("another operation in progress")
	}
	if !force {
		if reason := o.archiveOpBlockReason(); reason != "" {
			return fmt.Errorf("refusing snapshot: %s (match#35 lag guard; POST {\"force\":true} to override)", reason)
		}
	}

	go o.doSnapshot()
	return nil
}

func (o *OperationsService) doSnapshot() {
	o.progress.Start("snapshot", 7)

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
		fmt.Printf("[SNAPSHOT] Node %d housekeeping:\n%s\n", i, hkOutput)
		if hkErr != nil {
			housekeepingFailures++
			fmt.Printf("[SNAPSHOT] WARNING: housekeeping failed on node %d: %v\n", i, hkErr)
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
	if o.progress.IsRunning() {
		return fmt.Errorf("another operation in progress")
	}
	if !force {
		if reason := o.archiveOpBlockReason(); reason != "" {
			return fmt.Errorf("refusing housekeeping: %s (match#35 lag guard; POST {\"force\":true} to override)", reason)
		}
	}

	go o.doHousekeeping()
	return nil
}

func (o *OperationsService) doHousekeeping() {
	o.progress.Start("housekeeping", 3)

	failures := 0
	for i := 0; i < 3; i++ {
		o.progress.Update(1+i, fmt.Sprintf("Reclaiming archive on Node %d...", i))
		output, err := o.cluster.ArchiveHousekeeping(i)
		fmt.Printf("[HOUSEKEEPING] Node %d:\n%s\n", i, output)
		if err != nil {
			failures++
			fmt.Printf("[HOUSEKEEPING] WARNING: failed on node %d: %v\n", i, err)
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
	if o.progress.IsRunning() {
		return fmt.Errorf("another operation in progress")
	}

	go o.doRebuildGateway(restart)
	return nil
}

func (o *OperationsService) doRebuildGateway(restart bool) {
	totalSteps := 2
	if restart {
		totalSteps = 3
	}
	o.progress.Start("rebuild-gateway", totalSteps)

	// Step 1: Build gateway module (safe - separate JAR from cluster)
	o.progress.Update(1, "Building gateway module...")
	cmd := exec.Command("bash", "-c",
		fmt.Sprintf("cd %s && mvn package -pl match-gateway -am -DskipTests -q 2>&1", o.cfg.ProjectDir))
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
	if o.progress.IsRunning() {
		return fmt.Errorf("another operation in progress")
	}

	go o.doRebuildCluster()
	return nil
}

func (o *OperationsService) doRebuildCluster() {
	o.progress.Start("rebuild-cluster", 3)

	stagingDir := filepath.Join(o.cfg.ProjectDir, "match-cluster/target/staging")
	stagingJar := filepath.Join(stagingDir, "match-cluster.jar")

	// Step 1: Build cluster module in isolated directory
	o.progress.Update(1, "Building cluster module in isolated directory...")
	buildId := fmt.Sprintf("%d", time.Now().UnixMilli())
	tempBuildDir := "/tmp/match-cluster-build-" + buildId

	buildScript := fmt.Sprintf(`
		rm -rf %s &&
		mkdir -p %s &&
		mkdir -p %s &&
		rsync -a --exclude='*/target' --exclude='.git' --exclude='admin-gateway' --exclude='backup' --exclude='binaries' --exclude='binance-replay' %s/ %s/ &&
		cd %s &&
		mvn package -pl match-cluster -am -DskipTests -q &&
		cp %s/match-cluster/target/match-cluster.jar %s &&
		rm -rf %s
	`, tempBuildDir, tempBuildDir, stagingDir,
		o.cfg.ProjectDir, tempBuildDir,
		tempBuildDir,
		tempBuildDir, stagingJar, tempBuildDir)

	cmd := exec.Command("bash", "-c", buildScript)
	if output, err := cmd.CombinedOutput(); err != nil {
		o.progress.Finish(false, "Cluster build failed: "+err.Error()+" output: "+string(output))
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
}

// Cleanup removes stale Aeron files (requires all nodes stopped and force=true)
func (o *OperationsService) Cleanup(opts CleanupOptions) map[string]interface{} {
	result := map[string]interface{}{}

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

	// Dry-run mode: report what would be cleaned
	if opts.DryRun {
		wouldClean := []string{
			"/dev/shm/aeron-*",
			"/dev/shm/aeron-cluster/node*/cluster/cluster-mark*.dat",
			"/dev/shm/aeron-cluster/node*/cluster/*.lck",
			"/dev/shm/aeron-cluster/node*/archive/archive-mark.dat",
			"/tmp/aeron-* (gateway files)",
		}
		result["success"] = true
		result["dryRun"] = true
		result["wouldClean"] = wouldClean
		return result
	}

	// Backup mark files before cleanup if requested
	if opts.Backup {
		backupPath := o.backupMarkFiles()
		result["backupCreated"] = backupPath
	}

	var cleaned []string
	var errors []string

	// Clean shared memory aeron files
	if err := exec.Command("bash", "-c", "rm -rf /dev/shm/aeron-* 2>/dev/null || true").Run(); err != nil {
		errors = append(errors, "Failed to clean /dev/shm: "+err.Error())
	} else {
		cleaned = append(cleaned, "/dev/shm/aeron-*")
	}

	// Clean cluster mark files and lock files
	for i := 0; i < 3; i++ {
		nodeDir := fmt.Sprintf("/dev/shm/aeron-cluster/node%d", i)
		exec.Command("bash", "-c", fmt.Sprintf("rm -rf %s/cluster/cluster-mark*.dat 2>/dev/null || true", nodeDir)).Run()
		exec.Command("bash", "-c", fmt.Sprintf("rm -rf %s/cluster/*.lck 2>/dev/null || true", nodeDir)).Run()
		exec.Command("bash", "-c", fmt.Sprintf("rm -rf %s/archive/archive-mark.dat 2>/dev/null || true", nodeDir)).Run()
		cleaned = append(cleaned, fmt.Sprintf("%s (mark files, locks)", nodeDir))
	}

	// Clean gateway aeron files
	if err := exec.Command("bash", "-c", "rm -rf /tmp/aeron-* 2>/dev/null || true").Run(); err != nil {
		errors = append(errors, "Failed to clean /tmp/aeron-*: "+err.Error())
	} else {
		cleaned = append(cleaned, "/tmp/aeron-* (gateway files)")
	}

	result["success"] = len(errors) == 0
	result["cleaned"] = cleaned
	if len(errors) > 0 {
		result["errors"] = errors
	}
	if len(errors) == 0 {
		result["message"] = "Cleanup completed successfully. You can now start the cluster."
	} else {
		result["message"] = "Cleanup completed with some errors."
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

	// Clean files
	for _, pattern := range files {
		exec.Command("bash", "-c", "rm -f "+pattern+" 2>/dev/null").Run()
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
	addr := fmt.Sprintf("%s:%d", host, port)
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
func (o *OperationsService) waitForNodeStopped(nodeId int, timeout time.Duration) {
	service := fmt.Sprintf("node%d", nodeId)
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !o.isNodeRunning(nodeId) {
			return
		}
		time.Sleep(1 * time.Second)
	}
	// Force kill if still running
	fmt.Printf("Node %d still running after timeout, force killing\n", nodeId)
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
func (o *OperationsService) cleanNodeMediaDriver(nodeId int) {
	if o.procMgr != nil {
		if info := o.procMgr.Get(fmt.Sprintf("driver%d", nodeId)); info != nil && info.Running {
			return
		}
	}
	driverDir := fmt.Sprintf("/dev/shm/aeron-%s-%d-driver", currentUsername(), nodeId)
	if _, err := os.Stat(driverDir); err == nil {
		os.RemoveAll(driverDir)
		fmt.Printf("Cleaned stale MediaDriver: %s\n", driverDir)
	}
}

// waitForFollowerCatchUp blocks until the follower's cluster commit position is within
// catchUpLagBytes of the leader's commit position, OR the timeout elapses. Returns true if
// caught up, false on timeout. Uses the CnC counters (no JVM spawn) so it's cheap to poll.
//
// Why this matters: rolling update used to advance to the next node as soon as the previous
// follower's ingress port was open. The node was up but might still be replaying the log or
// loading a snapshot. Restarting the next node before catch-up risks losing quorum.
func (o *OperationsService) waitForFollowerCatchUp(followerId, leaderId int, timeout time.Duration) bool {
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
			fmt.Printf("Node %d caught up to leader (lag=%d bytes)\n", followerId, lag)
			return true
		}
		if follower.CommitPosition <= lastFollowerPos && follower.CommitPosition > 0 {
			// Position not advancing — log but keep waiting until timeout.
			fmt.Printf("Node %d catch-up stalled at pos=%d (lag=%d)\n",
				followerId, follower.CommitPosition, lag)
		}
		lastFollowerPos = follower.CommitPosition
	}
	return false
}

func contains(s, substr string) bool {
	return strings.Contains(s, substr)
}
