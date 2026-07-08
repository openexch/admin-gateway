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
	cfg           *config.Config
	cluster       *Cluster
	clusterStatus *ClusterStatus
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
	// Guarded by freshMu: fetchStatus can run from the poller and from the
	// GetStatus cache-miss fallback concurrently.
	freshMu     sync.Mutex
	prevCommit  [3]int64
	frozenPolls [3]int
}

func NewStatusService(cfg *config.Config, cluster *Cluster, status *ClusterStatus) *StatusService {
	s := &StatusService{
		cfg:           cfg,
		cluster:       cluster,
		clusterStatus: status,
		counters:      NewAeronCounters(),
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

// fetchStatus does the actual work (expensive - calls ClusterTool)
func (s *StatusService) fetchStatus() map[string]interface{} {
	// Detect leader (expensive - spawns JVM)
	leader := s.cluster.DetectLeader()
	if leader >= 0 {
		s.clusterStatus.UpdateLeader(leader, 0)
	} else {
		leader = s.clusterStatus.GetLeaderId()
	}

	// Read all nodes' counters and derive health up front: freshness ("is MY
	// commit position frozen while OTHERS advance?") needs the cross-node view
	// before any per-node health can be derived, and the freshness state must
	// not stay locked across the expensive JVM-spawning calls below.
	var cnc [3]*CounterData
	var cncOK [3]bool
	var advanced [3]bool
	var isActiveArr [3]bool
	var pidArr [3]int
	var pidAliveArr [3]bool
	var trackedArr [3]string
	var transitionalArr [3]bool
	var healthArr [3]string
	var runningArr [3]bool

	s.freshMu.Lock()
	for i := 0; i < 3; i++ {
		data, err := s.counters.GetNodeCounters(i)
		if err == nil && data.CommitPosition >= 0 {
			cnc[i] = data
			cncOK[i] = true
			advanced[i] = data.CommitPosition != s.prevCommit[i]
			s.prevCommit[i] = data.CommitPosition
		}

		serviceName := "node" + string(rune('0'+i))
		isActiveArr[i] = s.isServiceRunning(serviceName)
		pidArr[i] = s.getServicePID(serviceName)
		pidAliveArr[i] = isActiveArr[i] && isProcessAlive(pidArr[i])
		trackedArr[i] = s.clusterStatus.GetNodeStatus(i)
		transitionalArr[i] = trackedArr[i] == "STOPPING" || trackedArr[i] == "STARTING" ||
			trackedArr[i] == "REJOINING" || trackedArr[i] == "ELECTION"
	}
	for i := 0; i < 3; i++ {
		othersAdvanced := advanced[(i+1)%3] || advanced[(i+2)%3]
		s.frozenPolls[i] = UpdateFrozenPolls(s.frozenPolls[i], advanced[i], othersAdvanced,
			transitionalArr[i] || !isActiveArr[i])
		healthArr[i], runningArr[i] = DeriveNodeHealth(NodeObservation{
			PmRunning:    isActiveArr[i],
			PidAlive:     pidAliveArr[i],
			CncOK:        cncOK[i],
			FrozenPolls:  s.frozenPolls[i],
			Transitional: transitionalArr[i],
		})
	}
	s.freshMu.Unlock()

	// Build nodes status
	nodes := make([]map[string]interface{}, 3)
	for i := 0; i < 3; i++ {
		node := map[string]interface{}{
			"id": i,
		}

		pid := pidArr[i]
		transitional := transitionalArr[i]
		tracked := trackedArr[i]
		health, running := healthArr[i], runningArr[i]

		node["health"] = health
		node["pidAlive"] = pidAliveArr[i]
		node["commitAdvancing"] = advanced[i]
		if cncOK[i] && cnc[i].NodeRole >= 0 {
			node["cncRole"] = cncRoleString(cnc[i].NodeRole)
		}

		if running {
			node["running"] = true
			node["pid"] = pid

			// Check tracked status for transitional states
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

		// Archive and log info
		if archiveSize := s.cluster.GetArchiveSize(i); archiveSize >= 0 {
			node["archiveBytes"] = archiveSize
		}
		if diskSize := s.cluster.GetArchiveDiskUsage(i); diskSize >= 0 {
			node["archiveDiskBytes"] = diskSize
		}
		// Get log and snapshot positions in one call (avoids double JVM spawn)
		logPos, snapPos := s.cluster.GetLogAndSnapshotPositions(i)
		if logPos >= 0 {
			node["logPosition"] = logPos
		}
		if snapPos >= 0 {
			node["snapshotPosition"] = snapPos
		}

		// Real-time counters from Aeron shared memory (pre-read above)
		if cncOK[i] {
			node["commitPosition"] = cnc[i].CommitPosition
			// Calculate delta from last snapshot
			if snapPos >= 0 {
				node["logDelta"] = cnc[i].CommitPosition - snapPos
			} else {
				node["logDelta"] = cnc[i].CommitPosition
			}
			if cnc[i].SnapshotCount >= 0 {
				node["snapshotCount"] = cnc[i].SnapshotCount
			}
		}

		nodes[i] = node
	}

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

	allNodesHealthy := healthArr[0] == HealthHealthy &&
		healthArr[1] == HealthHealthy && healthArr[2] == HealthHealthy

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
		"leader":          leader,
		"nodes":           nodes,
		"allNodesHealthy": allNodesHealthy,
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
