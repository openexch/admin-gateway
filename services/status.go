// SPDX-License-Identifier: Apache-2.0
package services

import (
	"net/http"
	"path/filepath"
	"sync"
	"time"

	"github.com/match/admin-gateway/agent"
	"github.com/match/admin-gateway/config"
)

// ClusterStatus tracks the state of cluster nodes
type ClusterStatus struct {
	mu               sync.RWMutex
	nodeStatus       [3]string
	nodeRunning      [3]bool
	leaderId         int
	leadershipTermId int64
}

func NewClusterStatus() *ClusterStatus {
	return &ClusterStatus{
		leaderId: -1,
	}
}

func (cs *ClusterStatus) SetNodeStatus(nodeId int, status string, running bool) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	if nodeId >= 0 && nodeId < 3 {
		cs.nodeStatus[nodeId] = status
		cs.nodeRunning[nodeId] = running
	}
}

func (cs *ClusterStatus) GetNodeStatus(nodeId int) string {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	if nodeId >= 0 && nodeId < 3 {
		return cs.nodeStatus[nodeId]
	}
	return ""
}

func (cs *ClusterStatus) UpdateLeader(leaderId int, termId int64) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	cs.leaderId = leaderId
	cs.leadershipTermId = termId
}

func (cs *ClusterStatus) GetLeaderId() int {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	return cs.leaderId
}

// StatusService provides cluster status information with background caching
type StatusService struct {
	cfg                 *config.Config
	cluster             *Cluster
	assetsCluster       *Cluster       // optional second cluster, surfaced additively under result["assetsCluster"]
	clusterStatus       *ClusterStatus // matching-engine transitional-state tracker
	assetsClusterStatus *ClusterStatus // assets-engine transitional-state tracker (nil until registered)
	pm            agent.ProcessAgent
	// counters stays service-owned (not routed via the agent): the initial
	// refreshStatus runs before SetProcessManager, and CnC reads are a
	// flagged Horizon B seam (docs/AGENT-ARCHITECTURE.md).
	counters     *AeronCounters
	autoSnapshot *AutoSnapshot
	preflight    *Preflight

	// Cached status
	cacheMu      sync.RWMutex
	cachedStatus map[string]interface{}
	lastUpdate   time.Time

	// Background poller
	pollInterval time.Duration
	stopChan     chan struct{}

	// Counter-freshness state across polls (issue #13: CnC counters survive
	// process death, so health needs liveness + advancement, not raw values).
	// Per cluster (keyed by Cluster.Name), each sized to that cluster's NodeCount.
	// Guarded by freshMu: fetchStatus can run from the poller and from the
	// GetStatus cache-miss fallback concurrently.
	freshMu    sync.Mutex
	freshState map[string]*clusterFresh
}

// clusterFresh holds one cluster's cross-poll counter-freshness state.
type clusterFresh struct {
	prevCommit  []int64
	frozenPolls []int
}

func NewStatusService(cfg *config.Config, cluster *Cluster, status *ClusterStatus) *StatusService {
	s := &StatusService{
		cfg:           cfg,
		cluster:       cluster,
		clusterStatus: status,
		counters:      NewAeronCounters(),
		freshState:    make(map[string]*clusterFresh),
		pollInterval:  2 * time.Second, // Poll every 2 seconds
		stopChan:      make(chan struct{}),
	}

	// Initial fetch
	s.refreshStatus()

	// Start background poller
	go s.backgroundPoller()

	return s
}

func (s *StatusService) SetProcessManager(pm agent.ProcessAgent) {
	s.pm = pm
}

// SetAssetsCluster registers a second cluster (the assets engine) whose status is
// surfaced additively in /api/admin/status under result["clusters"] and (as a
// back-compat alias) result["assetsCluster"]. Its transitional-state tracker is the
// same instance the assets OperationsService writes to, so node stops/starts show
// STOPPING/STARTING in status.
func (s *StatusService) SetAssetsCluster(c *Cluster, tracker *ClusterStatus) {
	s.assetsCluster = c
	s.assetsClusterStatus = tracker
}

// freshFor returns the cross-poll freshness state for a cluster, lazily created
// and sized to its NodeCount. Callers must hold freshMu.
func (s *StatusService) freshFor(name string, n int) *clusterFresh {
	f := s.freshState[name]
	if f == nil || len(f.prevCommit) != n {
		f = &clusterFresh{prevCommit: make([]int64, n), frozenPolls: make([]int, n)}
		s.freshState[name] = f
	}
	return f
}

// buildRichClusterStatus builds one self-describing cluster block for any cluster
// from its descriptor, via a single code path. Health is the matching engine's
// cross-node freshness quorum (a frozen commit only counts against a node while
// OTHERS advance — at zero load every position is legitimately frozen), reused for
// both clusters: for a 1-node cluster "others advanced" is always false, so an idle
// ledger never reads DEGRADED, while it still gains DEAD/DEGRADED detection.
//
// The per-node object is a SUPERSET: a cluster with RichArchiveStats populates the
// JVM/du-derived positions + archive sizes (matching-engine parity); a lean cluster
// populates only the cheap CnC counters and derives its leader from the CnC role
// counter — no JVM spawns competing with the ME's busy-spin cores.
func (s *StatusService) buildRichClusterStatus(c *Cluster, tracker *ClusterStatus) map[string]interface{} {
	n := c.NodeCount
	if tracker == nil {
		tracker = NewClusterStatus() // never nil: a fresh tracker reports no transitional state
	}

	// Leader: the matching engine spawns a JVM (authoritative across terms and
	// transitional states); a lean cluster derives it from the CnC role counter
	// below (no JVM — the RichArchiveStats cost guard).
	leader := -1
	if c.RichArchiveStats {
		leader = c.DetectLeader()
		if leader >= 0 {
			tracker.UpdateLeader(leader, 0)
		} else {
			leader = tracker.GetLeaderId()
		}
	}

	// Per-node evidence + cross-node freshness derived up front: freshness ("is MY
	// commit frozen while OTHERS advance?") needs the cluster-wide view before any
	// per-node health, and the freshness state must not stay locked across the
	// JVM/du calls below.
	cnc := make([]*CounterData, n)
	cncOK := make([]bool, n)
	advanced := make([]bool, n)
	isActiveArr := make([]bool, n)
	pidArr := make([]int, n)
	pidAliveArr := make([]bool, n)
	trackedArr := make([]string, n)
	transitionalArr := make([]bool, n)
	healthArr := make([]string, n)
	runningArr := make([]bool, n)

	s.freshMu.Lock()
	fresh := s.freshFor(c.Name, n)
	for i := 0; i < n; i++ {
		data, err := s.counters.GetNodeCountersAt(c.CncPath(i))
		if err == nil && data.CommitPosition >= 0 {
			cnc[i] = data
			cncOK[i] = true
			advanced[i] = data.CommitPosition != fresh.prevCommit[i]
			fresh.prevCommit[i] = data.CommitPosition
		}
		serviceName := c.NodeName(i)
		isActiveArr[i] = s.isServiceRunning(serviceName)
		pidArr[i] = s.getServicePID(serviceName)
		pidAliveArr[i] = isActiveArr[i] && isProcessAlive(pidArr[i])
		trackedArr[i] = tracker.GetNodeStatus(i)
		transitionalArr[i] = trackedArr[i] == "STOPPING" || trackedArr[i] == "STARTING" ||
			trackedArr[i] == "REJOINING" || trackedArr[i] == "ELECTION"
	}
	for i := 0; i < n; i++ {
		// "Others advanced" is generic over node count: for n==1 no j!=i exists, so
		// a lone idle node is never penalized (matches the old lean-cluster "idle =
		// healthy"); for n==3 this is exactly advanced[(i+1)%3]||advanced[(i+2)%3].
		othersAdvanced := false
		for j := 0; j < n; j++ {
			if j != i && advanced[j] {
				othersAdvanced = true
				break
			}
		}
		fresh.frozenPolls[i] = UpdateFrozenPolls(fresh.frozenPolls[i], advanced[i], othersAdvanced,
			transitionalArr[i] || !isActiveArr[i])
		healthArr[i], runningArr[i] = DeriveNodeHealth(NodeObservation{
			PmRunning:    isActiveArr[i],
			PidAlive:     pidAliveArr[i],
			CncOK:        cncOK[i],
			FrozenPolls:  fresh.frozenPolls[i],
			Transitional: transitionalArr[i],
		})
	}
	s.freshMu.Unlock()

	// Lean clusters derive the leader from the CnC role counter (2 == LEADER).
	if !c.RichArchiveStats {
		for i := 0; i < n; i++ {
			if cncOK[i] && cnc[i].NodeRole == 2 {
				leader = i
				break
			}
		}
	}

	nodes := make([]map[string]interface{}, n)
	healthyCount := 0
	for i := 0; i < n; i++ {
		// procName lets the UI join a node to its process-manager record (mem/cpu/
		// uptime) without hardcoding a per-cluster naming scheme (node0.. vs ae0).
		node := map[string]interface{}{"id": i, "procName": c.NodeName(i)}
		pid := pidArr[i]
		transitional := transitionalArr[i]
		tracked := trackedArr[i]
		health, running := healthArr[i], runningArr[i]

		node["health"] = health
		node["healthy"] = health == HealthHealthy
		node["pidAlive"] = pidAliveArr[i]
		node["commitAdvancing"] = advanced[i]
		if cncOK[i] && cnc[i].NodeRole >= 0 {
			node["cncRole"] = cncRoleString(cnc[i].NodeRole)
		}

		if running {
			node["running"] = true
			node["pid"] = pid
			if transitional {
				node["role"] = tracked
			} else if i == leader {
				node["role"] = "LEADER"
			} else {
				node["role"] = "FOLLOWER"
			}
		} else {
			node["running"] = false
			if health == HealthDead {
				node["role"] = "DEAD"
				node["pid"] = pid
			} else {
				node["role"] = "OFFLINE"
			}
		}

		if c.RichArchiveStats {
			// Matching-engine parity: JVM/du-derived positions + archive sizes.
			if archiveSize := c.GetArchiveSize(i); archiveSize >= 0 {
				node["archiveBytes"] = archiveSize
			}
			if diskSize := c.GetArchiveDiskUsage(i); diskSize >= 0 {
				node["archiveDiskBytes"] = diskSize
			}
			logPos, snapPos := c.GetLogAndSnapshotPositions(i)
			if logPos >= 0 {
				node["logPosition"] = logPos
			}
			if snapPos >= 0 {
				node["snapshotPosition"] = snapPos
			}
			if cncOK[i] {
				node["commitPosition"] = cnc[i].CommitPosition
				if snapPos >= 0 {
					node["logDelta"] = cnc[i].CommitPosition - snapPos
				} else {
					node["logDelta"] = cnc[i].CommitPosition
				}
				if cnc[i].SnapshotCount >= 0 {
					node["snapshotCount"] = cnc[i].SnapshotCount
				}
			}
		} else if cncOK[i] {
			// Lean cluster: cheap CnC counters only (no JVM/du).
			node["commitPosition"] = cnc[i].CommitPosition
			if cnc[i].SnapshotCount >= 0 {
				node["snapshotCount"] = cnc[i].SnapshotCount
			}
		}

		if health == HealthHealthy {
			healthyCount++
		}
		nodes[i] = node
	}

	return map[string]interface{}{
		"name":            c.Name,
		"display":         c.Display,
		"kind":            c.Kind,
		"nodeCount":       n,
		"leader":          leader,
		"allNodesHealthy": healthyCount == n && n > 0,
		"capabilities":    c.Capabilities(),
		"nodes":           nodes,
	}
}

func (s *StatusService) SetAutoSnapshot(as *AutoSnapshot) {
	s.autoSnapshot = as
}

// SetPreflight injects the invariant engine; every status poll then surfaces
// the cheap invariant checks (#42's driver-dir lie in particular).
func (s *StatusService) SetPreflight(p *Preflight) {
	s.preflight = p
}

func (s *StatusService) Stop() {
	close(s.stopChan)
}

// isServiceRunning checks if a service is running via ProcessManager
func (s *StatusService) isServiceRunning(name string) bool {
	if s.pm == nil {
		return false
	}
	info := s.pm.Get(name)
	return info != nil && info.Running
}

// getServicePID gets PID from ProcessManager
func (s *StatusService) getServicePID(name string) int {
	if s.pm == nil {
		return 0
	}
	info := s.pm.Get(name)
	if info != nil {
		return info.PID
	}
	return 0
}

func (s *StatusService) backgroundPoller() {
	ticker := time.NewTicker(s.pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			s.refreshStatus()
		case <-s.stopChan:
			return
		}
	}
}

func (s *StatusService) refreshStatus() {
	status := s.fetchStatus()

	s.cacheMu.Lock()
	s.cachedStatus = status
	s.lastUpdate = time.Now()
	s.cacheMu.Unlock()
}

// GetStatus returns cached status (instant response)
func (s *StatusService) GetStatus() map[string]interface{} {
	s.cacheMu.RLock()
	defer s.cacheMu.RUnlock()

	if s.cachedStatus == nil {
		// Fallback if cache not ready
		return s.fetchStatus()
	}

	// Add cache age info
	result := make(map[string]interface{})
	for k, v := range s.cachedStatus {
		result[k] = v
	}
	result["cacheAgeMs"] = time.Since(s.lastUpdate).Milliseconds()

	return result
}

// fetchStatus does the actual work (expensive - DetectLeader/recording-log/du
// spawn JVMs for the matching engine; a lean second cluster adds only CnC reads).
func (s *StatusService) fetchStatus() map[string]interface{} {
	// The matching engine is the primary cluster: its block becomes clusters[0] and
	// its nodes/leader/allNodesHealthy are ALSO aliased to the top level (by
	// reference) so in-process consumers (metrics/operations/preflight) and the
	// pre-clusters[] frontend keep working unchanged.
	matchBlock := s.buildRichClusterStatus(s.cluster, s.clusterStatus)
	clusters := []map[string]interface{}{matchBlock}

	// Build gateways status (PID + HTTP health probe)
	gateways := map[string]interface{}{
		"market": map[string]interface{}{
			"running": s.isServiceRunning("market"),
			"port":    8081,
			"healthy": probeHealth("http://localhost:8081/health"),
		},
		"admin": map[string]interface{}{
			"running": true, // We're always running
			"port":    8082,
			"healthy": true,
		},
		"oms": map[string]interface{}{
			"running": s.isServiceRunning("oms"),
			"port":    8080,
			"healthy": probeHealth("http://localhost:8080/api/v1/health"),
		},
	}

	// Check backup: "running" alone proves nothing (match#36 — the backup agent
	// wedged silently for days); freshness comes from the app's heartbeat file.
	backupFresh, backupReason, _ := BackupFreshness(filepath.Join(s.cfg.ProjectDir, "backup"))
	backup := map[string]interface{}{
		"running": s.isServiceRunning("backup"),
		"fresh":   backupFresh,
		"reason":  backupReason,
	}
	// The backup belongs to the matching engine (it owns BackupDir); surface it on
	// its cluster block too, kept at the root as an alias.
	matchBlock["backup"] = backup

	// Demo end-to-end health from the market simulator's canary
	// (:8090/health returns 200 only when every critical check passes:
	// order round-trip, fills, market data, CORS incl. the public edge).
	// This is the signal that catches "the demo is silently broken".
	simRunning := s.isServiceRunning("sim")
	demoHealthy := simRunning && probeHealth("http://localhost:8090/health")
	demo := map[string]interface{}{
		"running": simRunning,
		"healthy": demoHealthy,
		"port":    8090,
	}

	result := map[string]interface{}{
		"clusters":        clusters,                      // generic multi-cluster array
		"leader":          matchBlock["leader"],          // alias (matching engine)
		"nodes":           matchBlock["nodes"],           // alias BY REFERENCE (the rich []map slice)
		"allNodesHealthy": matchBlock["allNodesHealthy"], // alias
		"gateways":        gateways,
		"backup":          backup,
		"demo":            demo,
		"demoHealthy":     demoHealthy,
		"activeProfile":   s.cfg.ActiveName(), // live runtime profile (Phase 2 switch)
		"gateway": map[string]interface{}{ // Legacy field
			"running": s.isServiceRunning("oms"),
			"port":    8080,
		},
		"autoSnapshot": s.getAutoSnapshotStatus(),
	}
	// The assets engine as a second first-class cluster: appended to clusters[] and
	// kept under the existing "assetsCluster" key as an alias. Absent when unregistered.
	if s.assetsCluster != nil {
		assetsBlock := s.buildRichClusterStatus(s.assetsCluster, s.assetsClusterStatus)
		clusters = append(clusters, assetsBlock)
		result["clusters"] = clusters
		result["assetsCluster"] = assetsBlock
	}
	if s.preflight != nil {
		inv := s.preflight.RunCheap()
		result["invariants"] = inv
		result["invariantsOk"] = InvariantsOK(inv)
	}
	// Surface an in-flight or completed rebuild-admin handshake (omitted when
	// no rebuild has happened on this checkout).
	if rb := ReadRebuildStatus(s.cfg.AdminDir); rb["state"] != "none" {
		result["lastRebuild"] = rb
	}
	return result
}

// getAutoSnapshotStatus returns auto-snapshot status from the AutoSnapshot service
func (s *StatusService) getAutoSnapshotStatus() map[string]interface{} {
	if s.autoSnapshot != nil {
		return s.autoSnapshot.ToMap()
	}
	return map[string]interface{}{
		"enabled":         false,
		"intervalMinutes": int64(0),
	}
}

// probeHealth makes a quick HTTP GET to a health endpoint and returns true if it responds 200.
func probeHealth(url string) bool {
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}
