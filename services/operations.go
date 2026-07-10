// SPDX-License-Identifier: Apache-2.0
package services

import (
	"crypto/sha256"
	"encoding/hex"
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

	"github.com/match/admin-gateway/agent"
	"github.com/match/admin-gateway/config"
	"github.com/match/admin-gateway/logging"
)

// OperationsService handles complex cluster operations
type OperationsService struct {
	cfg           *config.Config
	systemd       *Systemd
	cluster       *Cluster
	peers         []*Cluster // OTHER registered clusters; their dirs are off-limits to this cluster's cleanup
	progress      *Progress
	clusterStatus *ClusterStatus
	procMgr       agent.ProcessAgent
	statusSvc     *StatusService
	preflight     *Preflight
	log           *slog.Logger

	// execBuild runs one staged-build step (rsync/mvn) and returns its
	// combined output. Injectable so op-level tests can record the commands
	// and assert builds never execute in the live tree (#45).
	execBuild func(dir string, argv ...string) ([]byte, error)
}

func NewOperationsService(cfg *config.Config, systemd *Systemd, cluster *Cluster, progress *Progress, status *ClusterStatus) *OperationsService {
	o := &OperationsService{
		cfg:           cfg,
		systemd:       systemd,
		cluster:       cluster,
		progress:      progress,
		clusterStatus: status,
		log:           logging.Component("ops"),
	}
	o.execBuild = func(dir string, argv ...string) ([]byte, error) {
		return o.buildCmd(dir, argv...).CombinedOutput()
	}
	return o
}

// SetPeerClusters registers the OTHER clusters on this box, whose state/driver
// dirs this service's cleanup must never touch.
func (o *OperationsService) SetPeerClusters(peers []*Cluster) { o.peers = peers }

// Cluster returns the descriptor this ops service manages. Cluster-scoped handlers
// resolve the ops via ?cluster= (opsFor) and read the descriptor from here, so one
// code path serves every cluster (node names, count, capability flags, DetectLeader).
func (o *OperationsService) Cluster() *Cluster { return o.cluster }

// Status returns this cluster's transitional-state tracker (STOPPING/STARTING/…),
// written by the node lifecycle handlers and read by the status poller.
func (o *OperationsService) Status() *ClusterStatus { return o.clusterStatus }

// SetProcessManager injects the process agent (avoids circular init)
func (o *OperationsService) SetProcessManager(pm agent.ProcessAgent) {
	o.procMgr = pm
}

// SetStatusService injects the status service for the archive-op lag guard.
func (o *OperationsService) SetStatusService(s *StatusService) {
	o.statusSvc = s
}

// SetPreflight injects the invariant engine that gates destructive operations.
func (o *OperationsService) SetPreflight(p *Preflight) {
	o.preflight = p
}

// gate runs the pre-flight gate for op inside an already-claimed progress
// slot, releasing the slot on refusal (an early return without Finish would
// wedge every future operation — the #26 lesson, same shape as Snapshot's
// lag guard).
func (o *OperationsService) gate(op string, force bool) error {
	if o.preflight == nil {
		return nil
	}
	// Cluster-scoped: the match-specific checks (quorum, driver-dirs) are skipped
	// for other clusters (e.g. the single-node assets engine) while the global
	// mem/disk gates still apply — see Preflight.GateForCluster (ag#83). A nil
	// cluster (some unit tests) defaults to the matching-engine semantics.
	clusterName := matchClusterName
	if o.cluster != nil {
		clusterName = o.cluster.Name
	}
	if err := o.preflight.GateForCluster(op, clusterName, force); err != nil {
		o.progress.Finish(false, "refused: "+err.Error())
		return err
	}
	return nil
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
	info := o.procMgr.Get(o.cluster.NodeName(nodeId))
	return info != nil && info.Running
}

// startService starts a service via process manager
func (o *OperationsService) startService(name string) {
	if o.procMgr == nil {
		o.log.Error("process manager not initialized, cannot start service", "service", name)
		return
	}
	if err := o.procMgr.StartUnchecked(name); err != nil {
		o.log.Error("start service failed", "service", name, "err", err)
	}
}

// stopService stops a service via process manager
func (o *OperationsService) stopService(name string) {
	if o.procMgr == nil {
		o.log.Error("process manager not initialized, cannot stop service", "service", name)
		return
	}
	if err := o.procMgr.ForceStop(name); err != nil {
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

// buildCmd returns an exec.Cmd for a heavy build step (mvn, go build, rsync),
// niced so compiles cannot starve the trading processes: an unniced maven
// build on this box is the same resource-pressure family that OOM-crashed the
// cluster during the #43 rolling update. ADMIN_BUILD_NICE (default 10) tunes
// CPU niceness, 0 disables; disk I/O drops to idle best-effort when ionice
// is available.
func (o *OperationsService) buildCmd(dir string, argv ...string) *exec.Cmd {
	if n := o.cfg.BuildNice; n > 0 {
		prefix := []string{"nice", "-n", strconv.Itoa(n)}
		if _, err := exec.LookPath("ionice"); err == nil {
			prefix = append([]string{"ionice", "-c2", "-n7"}, prefix...)
		}
		argv = append(prefix, argv...)
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = dir
	return cmd
}

// installStagedJar deploys a staged JAR to its live path through the agent's
// artifact primitive (sha256-verified, atomic). Removes the staging file on
// success, preserving the old mv semantics.
func (o *OperationsService) installStagedJar(stagingJar, jarPath string) error {
	if o.procMgr == nil {
		return fmt.Errorf("process agent not initialized")
	}
	f, err := os.Open(stagingJar)
	if err != nil {
		return err
	}
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		f.Close()
		return err
	}
	spec := agent.ArtifactSpec{
		DestPath: jarPath,
		Sha256:   hex.EncodeToString(h.Sum(nil)),
		Mode:     0644,
	}
	err = o.procMgr.InstallArtifact(spec, f)
	f.Close()
	if err != nil {
		return err
	}
	os.Remove(stagingJar)
	return nil
}

// RollingUpdate performs a rolling update of all cluster nodes
func (o *OperationsService) RollingUpdate(force bool) error {
	if !o.progress.TryStart("rolling-update", 11) {
		return fmt.Errorf("another operation in progress")
	}
	// Pre-flight (#43): a node restart's catchup transient on a box without
	// memory headroom OOM-stalled the whole cluster on 2026-07-06, and rolling
	// at 2/3 gambles the remaining follower. Refuse unless forced.
	if err := o.gate("rolling-update", force); err != nil {
		return err
	}

	go o.doRollingUpdate()
	return nil
}

// clusterStateAtAbort renders the truthful cluster state for abort messages,
// from the same 2s status cache the archive-op guard trusts. Hardcoded
// "cluster keeps quorum (2/3)" claims lied during the #43 incident: the abort
// fired after a full-cluster OOM crash with all three nodes down.
func (o *OperationsService) clusterStateAtAbort() string {
	if o.statusSvc == nil {
		return "cluster state unknown (status service unavailable)"
	}
	s := o.statusSvc.GetStatus()
	nodes, ok := s["nodes"].([]map[string]interface{})
	if !ok {
		return "cluster state unknown (node health unavailable)"
	}
	return summarizeNodeStates(nodes)
}

// summarizeNodeStates is the pure core of clusterStateAtAbort:
// "state at abort per last poll: node0=OFFLINE node1=HEALTHY node2=DEAD — 1/3 healthy, QUORUM LOST".
func summarizeNodeStates(nodes []map[string]interface{}) string {
	healthy := 0
	var parts []string
	for _, n := range nodes {
		h, _ := n["health"].(string)
		if h == "" {
			h = "UNKNOWN"
		}
		if h == HealthHealthy {
			healthy++
		}
		parts = append(parts, fmt.Sprintf("node%v=%s", n["id"], h))
	}
	quorum := "quorum intact"
	if healthy < 2 {
		quorum = "QUORUM LOST"
	}
	return fmt.Sprintf("state at abort per last poll: %s — %d/%d healthy, %s",
		strings.Join(parts, " "), healthy, len(nodes), quorum)
}

// stageModuleJar builds one maven module in an isolated copy of srcDir and
// copies the built jar to stagingJar. The live tree is never touched: mvn
// (whose clean/-am phases rebuild upstream modules too) runs only inside the
// rsync'd temp tree — running it against the live tree is how rebuild-gateway
// deleted the running cluster jar out from under the nodes on 2026-07-06
// (#45). No shell involved: arg-vector execs and direct filesystem calls
// only (admin-gateway#11). The temp build dir is removed on every exit path.
func (o *OperationsService) stageModuleJar(srcDir, module, builtJarRel, tempBuildDir, stagingJar string) error {
	if err := os.RemoveAll(tempBuildDir); err != nil {
		return fmt.Errorf("clean temp build dir: %w", err)
	}
	if err := os.MkdirAll(tempBuildDir, 0o755); err != nil {
		return fmt.Errorf("create temp build dir: %w", err)
	}
	defer os.RemoveAll(tempBuildDir)
	if err := os.MkdirAll(filepath.Dir(stagingJar), 0o755); err != nil {
		return fmt.Errorf("create staging dir: %w", err)
	}
	if output, err := o.execBuild("", "rsync", "-a",
		"--exclude=*/target", "--exclude=.git", "--exclude=admin-gateway",
		"--exclude=backup", "--exclude=binaries", "--exclude=binance-replay",
		srcDir+"/", tempBuildDir+"/"); err != nil {
		return fmt.Errorf("rsync: %v: %s", err, output)
	}
	if output, err := o.execBuild(tempBuildDir,
		"mvn", "package", "-pl", module, "-am", "-DskipTests", "-q"); err != nil {
		return fmt.Errorf("mvn: %v: %s", err, output)
	}
	if err := copyFile(filepath.Join(tempBuildDir, builtJarRel), stagingJar); err != nil {
		return fmt.Errorf("copy staged jar: %w", err)
	}
	return nil
}

// stageClusterJar stages the cluster module (rolling-update, rebuild-cluster).
func (o *OperationsService) stageClusterJar(tempBuildDir, stagingJar string) error {
	return o.stageModuleJar(o.cluster.ProjectDir, o.cluster.Module,
		o.cluster.Module+"/target/"+o.cluster.Module+".jar", tempBuildDir, stagingJar)
}

func (o *OperationsService) doRollingUpdate() {
	defer o.recoverOp() // ag#67: contain+record a panic, free the slot
	log := o.log.With("op", "rolling-update", "op_id", o.progress.CurrentOpID())
	jarPath := o.cluster.Jar
	stagingDir := filepath.Join(o.cluster.ProjectDir, o.cluster.Module+"/target/staging")
	stagingJar := filepath.Join(stagingDir, o.cluster.Module+".jar")

	// Step 1: Build in isolated directory (NEVER touch live JAR)
	// Multi-module: copy entire project tree (excluding target dirs), build the cluster module
	o.progress.Update(1, "Building cluster module in isolated directory...")
	buildId := fmt.Sprintf("%d", time.Now().UnixMilli())
	tempBuildDir := "/tmp/" + o.cluster.Name + "-rolling-build-" + buildId

	if err := o.stageClusterJar(tempBuildDir, stagingJar); err != nil {
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
	for i := 0; i < o.cluster.NodeCount(); i++ {
		if i != leader {
			followers = append(followers, i)
		}
	}

	// Single-node cluster: there is no quorum to preserve and no follower/leader
	// handoff — a "rolling" update is just a swap-and-restart of the sole node
	// (brief downtime, by design; for the assets engine the settlement projector's
	// gap recovery absorbs it). This never runs for the ≥2-node matching engine.
	if o.cluster.NodeCount() == 1 {
		node := leader // == 0
		o.progress.Update(3, "Stopping node (single-node cluster)...")
		o.clusterStatus.SetNodeStatus(node, "STOPPING", false)
		o.stopService(o.cluster.NodeName(node))
		o.waitForNodeStopped(log, node, 15*time.Second)
		o.clusterStatus.SetNodeStatus(node, "OFFLINE", false)

		o.progress.Update(3, "Deploying new JAR...")
		if err := o.installStagedJar(stagingJar, jarPath); err != nil {
			o.progress.Finish(false, "JAR deploy failed: "+err.Error())
			return
		}
		if !o.cluster.Embedded {
			o.cleanNodeMediaDriver(log, node)
		}

		o.progress.Update(3, "Starting node with new code...")
		o.clusterStatus.SetNodeStatus(node, "STARTING", false)
		o.startService(o.cluster.NodeName(node))
		o.clusterStatus.SetNodeStatus(node, "REJOINING", true)
		if !o.waitForPort("127.0.0.1", o.cluster.IngressPort(node), 60*time.Second) {
			o.clusterStatus.SetNodeStatus(node, "OFFLINE", false)
			os.RemoveAll(stagingDir)
			o.progress.Finish(false, "node did not come back within 60s after update")
			return
		}
		o.clusterStatus.SetNodeStatus(node, "LEADER", true)
		o.clusterStatus.UpdateLeader(node, 0)
		os.RemoveAll(stagingDir)
		o.progress.Finish(true, "Single-node cluster updated (restart)")
		return
	}

	jarSwapped := false
	step := 3

	// Steps 3-8: Update followers
	for _, nodeId := range followers {
		nodeLabel := fmt.Sprintf("Node %d", nodeId)

		// Stop follower
		o.progress.Update(step, "Stopping "+nodeLabel+"...")
		o.clusterStatus.SetNodeStatus(nodeId, "STOPPING", false)
		o.stopService(o.cluster.NodeName(nodeId))
		o.waitForNodeStopped(log, nodeId, 15*time.Second)
		o.clusterStatus.SetNodeStatus(nodeId, "OFFLINE", false)
		step++

		// Swap JAR after first node is stopped (agent artifact primitive:
		// sha-verified temp-file write + atomic rename — a partial copy is
		// never visible at jarPath, and this works cross-filesystem where a
		// bare rename from /tmp staging would not)
		if !jarSwapped {
			o.progress.Update(step, "Deploying new JAR...")
			if err := o.installStagedJar(stagingJar, jarPath); err != nil {
				o.clusterStatus.SetNodeStatus(nodeId, "OFFLINE", false)
				o.progress.Finish(false, fmt.Sprintf(
					"JAR deploy failed (%v) — ABORTED with node%d stopped; %s", err, nodeId, o.clusterStateAtAbort()))
				return
			}
			jarSwapped = true
			time.Sleep(100 * time.Millisecond)
		}

		// Clean stale MediaDriver directory for this node (external drivers only;
		// an embedded-driver cluster has no separate driver dir to clean).
		if !o.cluster.Embedded {
			o.cleanNodeMediaDriver(log, nodeId)
		}

		// Start follower with new code
		o.progress.Update(step, "Starting "+nodeLabel+" with new code...")
		o.clusterStatus.SetNodeStatus(nodeId, "STARTING", false)
		o.startService(o.cluster.NodeName(nodeId))
		step++

		// Wait for the node to actually rejoin — verify via ingress port.
		// HARD-FAIL on timeout (#10): proceeding to stop the NEXT node while
		// this one is down or still replaying drops the cluster below quorum
		// and can wedge the election. Aborting here leaves 2/3 quorum intact
		// for the operator to investigate.
		o.progress.Update(step, nodeLabel+": Waiting to rejoin cluster...")
		o.clusterStatus.SetNodeStatus(nodeId, "REJOINING", true)
		ingressPort := o.cluster.IngressPort(nodeId)
		if !o.waitForPort("127.0.0.1", ingressPort, 60*time.Second) {
			o.clusterStatus.SetNodeStatus(nodeId, "OFFLINE", false)
			o.progress.Finish(false, fmt.Sprintf(
				"%s did not rejoin within 60s — ABORTED before touching more nodes; "+
					"%s; investigate node%d then re-run rolling-update", nodeLabel, o.clusterStateAtAbort(), nodeId))
			return
		}
		// Wait for the node to catch up to the leader's commit position before moving on.
		// Otherwise we may stop the next node while this one is still replaying the log,
		// transiently dropping the cluster below quorum (2/3).
		o.progress.Update(step, nodeLabel+": Waiting for log catch-up...")
		if !o.waitForFollowerCatchUp(log, nodeId, leader, 60*time.Second) {
			o.progress.Finish(false, fmt.Sprintf(
				"%s rejoined but did not catch up to the leader within 60s — ABORTED before "+
					"touching more nodes; %s; investigate node%d then re-run", nodeLabel, o.clusterStateAtAbort(), nodeId))
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
	o.stopService(o.cluster.NodeName(leader))
	o.waitForNodeStopped(log, leader, 15*time.Second)
	o.clusterStatus.SetNodeStatus(leader, "OFFLINE", false)

	// Clean stale MediaDriver directory for old leader (external drivers only)
	if !o.cluster.Embedded {
		o.cleanNodeMediaDriver(log, leader)
	}

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
			port := o.cluster.IngressPort(nodeId)
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
		o.progress.Finish(false, "Leader election failed after 60s — cluster may need manual recovery; "+
			o.clusterStateAtAbort())
		return
	}

	// Step 11: Start old leader as follower
	o.progress.Update(11, fmt.Sprintf("Starting Node %d as follower...", leader))
	o.clusterStatus.SetNodeStatus(leader, "STARTING", false)
	o.startService(o.cluster.NodeName(leader))

	// Wait for old leader to rejoin AND catch up to new leader's commit position.
	// HARD-FAIL if it doesn't (#10): the update deployed, but reporting success
	// with a member down/lagging hides a degraded cluster from the operator.
	o.clusterStatus.SetNodeStatus(leader, "REJOINING", true)
	ingressPort := o.cluster.IngressPort(leader)
	newLeader := o.cluster.DetectLeader()
	if !o.waitForPort("127.0.0.1", ingressPort, 60*time.Second) {
		o.clusterStatus.SetNodeStatus(leader, "OFFLINE", false)
		os.RemoveAll(stagingDir)
		o.progress.Finish(false, fmt.Sprintf(
			"New code deployed on all nodes, but node%d (old leader) did not rejoin within 60s — "+
				"%s; investigate node%d", leader, o.clusterStateAtAbort(), leader))
		return
	}
	if newLeader < 0 || !o.waitForFollowerCatchUp(log, leader, newLeader, 60*time.Second) {
		o.clusterStatus.SetNodeStatus(leader, "FOLLOWER", true)
		os.RemoveAll(stagingDir)
		o.progress.Finish(false, fmt.Sprintf(
			"New code deployed on all nodes, but node%d (old leader) rejoined without confirmed "+
				"catch-up within 60s — %s; verify commit positions before further operations",
			leader, o.clusterStateAtAbort()))
		return
	}
	o.progress.Update(11, fmt.Sprintf("Node %d rejoined and caught up", leader))
	o.clusterStatus.SetNodeStatus(leader, "FOLLOWER", true)

	// Cleanup staging
	os.RemoveAll(stagingDir)

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
	defer o.recoverOp() // ag#67: contain+record a panic, free the slot
	// AdminDir, not ProjectDir/admin-gateway: this repo split out of match, so the
	// old path builds a checkout that no longer exists.
	adminDir := o.cfg.AdminDir
	liveBinary := filepath.Join(adminDir, "admin-gateway")
	stagingBinary := filepath.Join(adminDir, "admin-gateway.staging")

	// Step 1: Build new binary to staging path (never overwrite live binary
	// directly). Explicit toolchain + GOTOOLCHAIN=local (#36): the systemd
	// user env can resolve an older apt go whose toolchain download is
	// disabled, so `go build` must not depend on ambient PATH resolution or
	// on downloading the go.mod toolchain.
	o.progress.Update(1, "Building admin gateway from source...")
	cmd := o.buildCmd(adminDir, o.cfg.GoBin, "build", "-o", stagingBinary, ".")
	cmd.Env = append(os.Environ(), "GOTOOLCHAIN=local")
	if output, err := cmd.CombinedOutput(); err != nil {
		os.Remove(stagingBinary)
		reason := "Build failed (" + o.cfg.GoBin + "): " + err.Error() + " output: " + tailString(string(output), 500)
		o.failRebuild(reason)
		return
	}

	// Step 2: Verify the staged binary is valid (basic sanity: exists + executable)
	o.progress.Update(2, "Verifying staged binary...")
	info, err := os.Stat(stagingBinary)
	if err != nil {
		o.failRebuild("Staged binary not found after build: " + err.Error())
		return
	}
	if info.Size() < 1024 {
		os.Remove(stagingBinary)
		o.failRebuild("Staged binary suspiciously small, aborting")
		return
	}
	stagedSha, err := fileSha256(stagingBinary)
	if err != nil {
		os.Remove(stagingBinary)
		o.failRebuild("Cannot hash staged binary: " + err.Error())
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
		o.failRebuild("Binary swap failed: " + err.Error())
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

// failRebuild reports a pre-restart rebuild-admin failure BOTH to the
// progress slot and durably to rebuild-result.json (#36): the progress
// message is transient, and before this the last SUCCESSFUL result kept
// being served by /api/admin/rebuild-status, hiding the failure entirely.
func (o *OperationsService) failRebuild(reason string) {
	o.log.Error("rebuild-admin failed", "reason", reason)
	if err := WriteRebuildFailure(o.cfg.AdminDir, o.progress.CurrentOpID(), reason); err != nil {
		o.log.Error("could not persist rebuild failure", "err", err)
	}
	o.progress.Finish(false, reason)
}

// tailString returns at most the last n bytes of s (for log/output tails).
func tailString(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n:]
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
	defer o.recoverOp() // ag#67: contain+record a panic, free the slot
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
	for i := 0; i < o.cluster.NodeCount(); i++ {
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
	defer o.recoverOp() // ag#67: contain+record a panic, free the slot
	log := o.log.With("op", "housekeeping", "op_id", o.progress.CurrentOpID())
	failures := 0
	for i := 0; i < o.cluster.NodeCount(); i++ {
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
func (o *OperationsService) RebuildGateway(restart, force bool) error {
	totalSteps := 2
	if restart {
		totalSteps = 3
	}
	if !o.progress.TryStart("rebuild-gateway", totalSteps) {
		return fmt.Errorf("another operation in progress")
	}
	if err := o.gate("rebuild-gateway", force); err != nil {
		return err
	}

	go o.doRebuildGateway(restart)
	return nil
}

func (o *OperationsService) doRebuildGateway(restart bool) {
	defer o.recoverOp() // ag#67: contain+record a panic, free the slot
	stagingJar := filepath.Join(o.cfg.ProjectDir, "match-gateway/target/staging/match-gateway.jar")

	// Step 1: Build in an isolated tree — NEVER mvn against the live tree.
	// The old in-place build's clean/-am phases rebuilt upstream modules and
	// deleted match-cluster/target/ out from under the running nodes; the
	// next restart crash-looped into disarm and took the cluster down for 10
	// minutes on 2026-07-06 (#45).
	o.progress.Update(1, "Building gateway module in isolated directory...")
	tempBuildDir := fmt.Sprintf("/tmp/match-gateway-build-%d", time.Now().UnixMilli())
	if err := o.stageModuleJar(o.cfg.ProjectDir, "match-gateway",
		"match-gateway/target/match-gateway.jar", tempBuildDir, stagingJar); err != nil {
		o.progress.Finish(false, "Gateway build failed: "+err.Error())
		return
	}

	// Step 2: Install the staged jar over the live one (sha-verified, atomic).
	o.progress.Update(2, "Installing gateway JAR...")
	if err := o.installStagedJar(stagingJar, o.cfg.GatewayJar); err != nil {
		o.progress.Finish(false, "Gateway JAR install failed: "+err.Error())
		return
	}

	if !restart {
		o.progress.Finish(true, "Gateway JAR rebuilt and installed")
		return
	}

	// Step 3: Restart the market gateway — the only service running this jar.
	// (oms runs oms-app.jar from the OMS repo; restarting it here never picked
	// up new code and was dropped — use rebuild-oms for that.)
	o.progress.Update(3, "Restarting market gateway...")
	o.restartService("market")
	time.Sleep(3 * time.Second)

	o.progress.Finish(true, "Gateway rebuilt, installed and market gateway restarted")
}

// RebuildCluster builds the cluster module to staging (does NOT deploy).
// WARNING: The built JAR goes to staging, NOT the live location.
// Use rolling-update to deploy, or manually swap the JAR.
func (o *OperationsService) RebuildCluster(force bool) error {
	if !o.progress.TryStart("rebuild-cluster", 3) {
		return fmt.Errorf("another operation in progress")
	}
	if err := o.gate("rebuild-cluster", force); err != nil {
		return err
	}

	go o.doRebuildCluster()
	return nil
}

func (o *OperationsService) doRebuildCluster() {
	defer o.recoverOp() // ag#67: contain+record a panic, free the slot
	stagingDir := filepath.Join(o.cluster.ProjectDir, o.cluster.Module+"/target/staging")
	stagingJar := filepath.Join(stagingDir, o.cluster.Module+".jar")

	// Step 1: Build cluster module in isolated directory
	o.progress.Update(1, "Building cluster module in isolated directory...")
	buildId := fmt.Sprintf("%d", time.Now().UnixMilli())
	tempBuildDir := "/tmp/" + o.cluster.Module + "-build-" + buildId

	if err := o.stageClusterJar(tempBuildDir, stagingJar); err != nil {
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
// tmpDir FOR ONE CLUSTER. stateBase is that cluster's state-dir base name
// (aeron-cluster / aeron-assets): its archives are PRESERVED unless
// includeArchive — /cleanup used to run `rm -rf /dev/shm/aeron-*`, and that
// glob matches aeron-cluster — nuking the very archives P1.3 makes durable
// (#10). exclude lists OTHER clusters' dir base names (state dirs + driver
// dirs): a match cleanup must never touch /dev/shm/aeron-assets and vice versa
// (the 2026-07-09 clean-slate wiped the assets engine's state under a live
// ae0 before this scoping existed). driverGuard vets each aeron-* entry that IS
// one of THIS cluster's node media-driver dirs through canDeleteDriverDir (the
// #42/ag#68 guard) so the sweep can never delete a driver dir out from under a
// live driver — it was the only unguarded /dev/shm driver-dir deleter before
// ag#68 (nil = vet nothing, used by the pure archive/scoping unit tests).
// Refused entries are returned in refused, not deleted. apply=false only
// reports. Factored out with configurable roots so every guarantee is unit-testable.
func cleanupSweep(shmDir, tmpDir, stateBase string, exclude map[string]bool, driverGuard func(base, path string) (ok bool, reason string), includeArchive, apply bool) (cleaned, preserved, errs, refused []string) {
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

	// 1. Aeron IPC dirs (drivers, clients) — everything aeron-* EXCEPT this
	// cluster's state dir (the old glob wrongly swallowed it) and every other
	// cluster's dirs (state + drivers), which are not ours to touch. A node
	// media-driver dir is deleted only if driverGuard clears it (ag#68).
	entries, _ := filepath.Glob(filepath.Join(shmDir, "aeron-*"))
	for _, e := range entries {
		base := filepath.Base(e)
		if base == stateBase || exclude[base] {
			continue
		}
		if driverGuard != nil {
			if ok, reason := driverGuard(base, e); !ok {
				refused = append(refused, e+": "+reason)
				continue
			}
		}
		remove(e, true)
	}

	// 2. Stale mark/lock files inside the cluster state dirs (node*, backup)
	for _, pattern := range []string{
		stateBase + "/*/cluster/cluster-mark*.dat",
		stateBase + "/*/cluster/*.lck",
		stateBase + "/*/archive/archive-mark.dat",
	} {
		matches, _ := filepath.Glob(filepath.Join(shmDir, pattern))
		for _, m := range matches {
			remove(m, false)
		}
	}

	// 3. The archives themselves: preserved unless explicitly included
	recordings, _ := filepath.Glob(filepath.Join(shmDir, stateBase+"/*/archive/*.rec"))
	clusterDir := filepath.Join(shmDir, stateBase)
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

	// 4. Gateway/client Aeron dirs under tmp (same exclusions apply)
	tmpEntries, _ := filepath.Glob(filepath.Join(tmpDir, "aeron-*"))
	for _, e := range tmpEntries {
		if exclude[filepath.Base(e)] {
			continue
		}
		remove(e, true)
	}

	return cleaned, preserved, errs, refused
}

// driverDirGuard returns the per-entry deletion guard cleanupSweep applies to
// aeron-* entries that are THIS cluster's node media-driver dirs (ag#68). It
// maps the driver-dir base name back to the owning node/driver services and
// routes the delete decision through canDeleteDriverDir with their live state,
// so the sweep can never delete a dir out from under a live driver — including
// the embedded-driver case (assets always, matching engine on the light
// profile) where there is no external driver pid file to trust. Entries that
// are NOT this cluster's driver dirs (client/gateway IPC dirs, etc.) always
// clear (ok=true), preserving the existing stale-IPC reclaim.
func (o *OperationsService) driverDirGuard() func(base, path string) (bool, string) {
	type owner struct{ node, driver string }
	owners := map[string]owner{}
	for i := 0; i < o.cluster.NodeCount(); i++ {
		b := filepath.Base(o.cluster.DriverAeronDir(i))
		owners[b] = owner{node: o.cluster.NodeName(i), driver: o.cluster.DriverName(i)}
	}
	return func(base, path string) (bool, string) {
		ow, ok := owners[base]
		if !ok {
			return true, "" // not one of this cluster's driver dirs
		}
		driverTracked, nodeRunning := false, false
		if o.procMgr != nil {
			if ow.driver != "" {
				if info := o.procMgr.Get(ow.driver); info != nil && info.Running {
					driverTracked = true
				}
			}
			if info := o.procMgr.Get(ow.node); info != nil && info.Running {
				nodeRunning = true
			}
		}
		return canDeleteDriverDir(path, driverTracked, nodeRunning)
	}
}

// peerClusterDirs is the exclusion set for this cluster's sweep: every OTHER
// registered cluster's state-dir and per-node driver-dir base names.
func (o *OperationsService) peerClusterDirs() map[string]bool {
	exclude := map[string]bool{}
	for _, p := range o.peers {
		exclude[filepath.Base(p.StateDir)] = true
		for i := 0; i < p.NodeCount(); i++ {
			exclude[filepath.Base(p.DriverAeronDir(i))] = true
		}
	}
	return exclude
}

// Cleanup removes stale Aeron files (requires all nodes stopped and force=true)
func (o *OperationsService) Cleanup(opts CleanupOptions) map[string]interface{} {
	result := map[string]interface{}{}

	// Dry-run changes nothing: allow it anytime (even with nodes running) so
	// ops can preview the sweep and the archive-preservation notice.
	if opts.DryRun {
		wouldClean, preserved, _, refused := cleanupSweep("/dev/shm", "/tmp",
			filepath.Base(o.cluster.StateDir), o.peerClusterDirs(), o.driverDirGuard(), opts.IncludeArchive, false)
		result["success"] = true
		result["dryRun"] = true
		result["wouldClean"] = wouldClean
		if len(preserved) > 0 {
			result["preserved"] = preserved
		}
		if len(refused) > 0 {
			result["refused"] = refused
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
	for i := 0; i < o.cluster.NodeCount(); i++ {
		if o.isNodeRunning(i) {
			result["success"] = false
			result["error"] = fmt.Sprintf("Node %d is still running. Stop all nodes before cleanup.", i)
			return result
		}
	}

	// External media drivers own /dev/shm/aeron-<user>-N-driver, which the wipe below
	// deletes — they must be stopped too or their IPC files are pulled out from under them.
	// An embedded-driver cluster has no separate driver services to check.
	if o.procMgr != nil && !o.cluster.Embedded {
		for i := 0; i < o.cluster.NodeCount(); i++ {
			if info := o.procMgr.Get(o.cluster.DriverName(i)); info != nil && info.Running {
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

	cleaned, preserved, errors, refused := cleanupSweep("/dev/shm", "/tmp",
		filepath.Base(o.cluster.StateDir), o.peerClusterDirs(), o.driverDirGuard(), opts.IncludeArchive, true)

	// Log every deletion and every refusal to the admin log (component=ops), not
	// just the API response, so a destructive sweep leaves an audit trail even if
	// the caller never reads the body (ag#68).
	for _, path := range cleaned {
		o.log.Info("cleanup removed", "cluster", o.cluster.Name, "path", path)
	}
	for _, r := range refused {
		o.log.Warn("cleanup refused driver dir (ag#68 guard)", "cluster", o.cluster.Name, "detail", r)
	}

	result["success"] = len(errors) == 0
	result["cleaned"] = cleaned
	if len(preserved) > 0 {
		result["preserved"] = preserved
	}
	if len(refused) > 0 {
		result["refused"] = refused
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

	if nodeId < 0 || nodeId >= o.cluster.NodeCount() {
		result["success"] = false
		result["error"] = fmt.Sprintf("Invalid nodeId (must be 0..%d)", o.cluster.NodeCount()-1)
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

	nodeDir := o.cluster.NodeStateDir(nodeId)
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
	backupDir := o.cluster.BackupDir
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

	if nodeId < 0 || nodeId >= o.cluster.NodeCount() {
		result["success"] = false
		result["error"] = fmt.Sprintf("Invalid nodeId (must be 0..%d)", o.cluster.NodeCount()-1)
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

	backupDir := o.cluster.BackupDir
	nodeDir := o.cluster.NodeStateDir(nodeId)

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
	backupDir := filepath.Join(o.cluster.BackupDir, "pre-cleanup", timestamp)
	os.MkdirAll(backupDir, 0755)

	for i := 0; i < o.cluster.NodeCount(); i++ {
		nodeDir := o.cluster.NodeStateDir(i)
		nodeBackup := filepath.Join(backupDir, o.cluster.NodeName(i))
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
	service := o.cluster.NodeName(nodeId)
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
// Ownership is judged by tracked state AND the launch script's pid file: tracked
// state reads stopped during driver crash-loops and adoption gaps, which is how
// the 2026-07-06 rolling update deleted node0's live dir (#42).
func (o *OperationsService) cleanNodeMediaDriver(log *slog.Logger, nodeId int) {
	trackedRunning := false
	nodeRunning := false
	if o.procMgr != nil {
		if info := o.procMgr.Get(o.cluster.DriverName(nodeId)); info != nil && info.Running {
			trackedRunning = true
		}
		// Embedded-mode fallback (ag#68): the owning node's liveness blocks the
		// delete when there is no external driver pid file to trust.
		if info := o.procMgr.Get(o.cluster.NodeName(nodeId)); info != nil && info.Running {
			nodeRunning = true
		}
	}
	driverDir := o.cluster.DriverAeronDir(nodeId)
	ok, reason := canDeleteDriverDir(driverDir, trackedRunning, nodeRunning)
	if !ok {
		if !trackedRunning {
			log.Error("refusing to delete media driver dir (#42 guard)", "dir", driverDir, "reason", reason)
		}
		return
	}
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
	deadline := time.Now().Add(timeout)
	var lastFollowerPos int64 = -1
	for time.Now().Before(deadline) {
		time.Sleep(500 * time.Millisecond)
		// Per-node counter reads go through the agent; the cross-node
		// comparison stays control-plane-side (docs/AGENT-ARCHITECTURE.md).
		leader, lerr := o.procMgr.NodeCounters(leaderId)
		follower, ferr := o.procMgr.NodeCounters(followerId)
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
