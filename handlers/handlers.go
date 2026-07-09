// SPDX-License-Identifier: Apache-2.0
package handlers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/match/admin-gateway/agent"
	"github.com/match/admin-gateway/config"
	"github.com/match/admin-gateway/logging"
	"github.com/match/admin-gateway/services"
)

type Handlers struct {
	statusSvc    *services.StatusService
	opsSvc       *services.OperationsService
	cluster      *services.Cluster
	progress     *services.Progress
	status       *services.ClusterStatus
	autoSnapshot *services.AutoSnapshot
	logSvc       *services.LogService
	procMgr      agent.ProcessAgent
	metrics      *services.MetricsService
	preflight    *services.Preflight
	cfg          *config.Config
	// clusterOps routes cluster-scoped ops (rolling-update, snapshot) to the
	// right cluster's OperationsService via the ?cluster= selector; the default
	// (empty/"match") is opsSvc, so existing callers are unchanged.
	clusterOps map[string]*services.OperationsService
}

func New(
	statusSvc *services.StatusService,
	opsSvc *services.OperationsService,
	clusterOps map[string]*services.OperationsService,
	cluster *services.Cluster,
	progress *services.Progress,
	status *services.ClusterStatus,
	autoSnapshot *services.AutoSnapshot,
	logSvc *services.LogService,
	procMgr agent.ProcessAgent,
	metrics *services.MetricsService,
	preflight *services.Preflight,
	cfg *config.Config,
) *Handlers {
	return &Handlers{
		statusSvc:    statusSvc,
		opsSvc:       opsSvc,
		clusterOps:   clusterOps,
		cluster:      cluster,
		progress:     progress,
		status:       status,
		autoSnapshot: autoSnapshot,
		logSvc:       logSvc,
		procMgr:      procMgr,
		metrics:      metrics,
		preflight:    preflight,
		cfg:          cfg,
	}
}

func (h *Handlers) RegisterRoutes(r chi.Router) {
	r.Use(corsMiddleware)

	// Status
	r.Get("/api/admin/status", h.handleStatus)
	r.Get("/api/admin/progress", h.handleProgress)
	r.Get("/api/admin/preflight", h.handlePreflight)
	r.Get("/api/admin/profile", h.handleGetProfile)  // active runtime profile + available set
	r.Post("/api/admin/profile", h.handleSetProfile) // switch the stack profile (apply-via-roll)
	r.Get("/api/admin/events", h.handleEvents)       // SSE: agent events + progress

	// Node operations
	r.Post("/api/admin/restart-node", h.handleRestartNode)
	r.Post("/api/admin/stop-node", h.handleStopNode)
	r.Post("/api/admin/start-node", h.handleStartNode)
	r.Post("/api/admin/stop-all-nodes", h.handleStopAllNodes)
	r.Post("/api/admin/start-all-nodes", h.handleStartAllNodes)

	// Complex operations
	r.Post("/api/admin/rolling-update", h.handleRollingUpdate)
	r.Post("/api/admin/snapshot", h.handleSnapshot)

	// Build operations (multi-module safe)
	r.Post("/api/admin/rebuild-gateway", h.handleRebuildGateway)
	r.Post("/api/admin/rebuild-cluster", h.handleRebuildCluster)
	r.Post("/api/admin/rebuild-oms", h.handleRebuildOms)

	// Live archive reclamation: purge log segments below latest snapshot.
	// (Aeron offline ArchiveTool compaction was removed — running it against a
	// live node corrupts snapshot recordings and breaks recover-from-snapshot.)
	r.Post("/api/admin/housekeeping", h.handleHousekeeping)

	// Auto-snapshot (GET/POST/DELETE)
	r.Get("/api/admin/auto-snapshot", h.handleAutoSnapshotGet)
	r.Post("/api/admin/auto-snapshot", h.handleAutoSnapshotPost)
	r.Delete("/api/admin/auto-snapshot", h.handleAutoSnapshotDelete)

	// Logs
	r.Get("/api/admin/logs", h.handleLogs)

	// Self-update (admin gateway) + post-restart verification handshake
	r.Post("/api/admin/rebuild-admin", h.handleRebuildAdmin)
	r.Get("/api/admin/rebuild-status", h.handleRebuildStatus)

	// Process manager
	r.Get("/api/admin/processes", h.handleProcessList)
	r.Get("/api/admin/processes/summary", h.handleProcessSummary)
	r.Get("/api/admin/processes/{name}", h.handleProcessGet)
	r.Post("/api/admin/processes/{name}/start", h.handleProcessStart)
	r.Post("/api/admin/processes/{name}/stop", h.handleProcessStop)
	r.Post("/api/admin/processes/{name}/restart", h.handleProcessRestart)
	r.Post("/api/admin/processes/{name}/force-stop", h.handleProcessForceStop)
	r.Post("/api/admin/processes/start-all", h.handleProcessStartAll)
	r.Post("/api/admin/processes/stop-all", h.handleProcessStopAll)
	r.Post("/api/admin/processes/restart-all", h.handleProcessRestartAll)

	// Cleanup and recovery
	r.Post("/api/admin/cleanup", h.handleCleanup)
	r.Post("/api/admin/cleanup-node", h.handleCleanupNode)
	r.Get("/api/admin/backup-info", h.handleBackupInfo)
	r.Post("/api/admin/recover-from-backup", h.handleRecoverFromBackup)
	r.Post("/api/admin/reseed-node", h.handleReseedNode)

	// Health check
	r.Get("/health", h.handleHealth)

	// Prometheus metrics (auth-exempt, like /health — local scraper)
	r.Get("/metrics", h.metrics.Handler)
}

func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		w.Header().Set("Access-Control-Allow-Private-Network", "true")

		if r.Method == "OPTIONS" {
			w.WriteHeader(http.StatusNoContent)
			return
		}

		next.ServeHTTP(w, r)
	})
}

func (h *Handlers) handleStatus(w http.ResponseWriter, r *http.Request) {
	jsonResponse(w, http.StatusOK, h.statusSvc.GetStatus())
}

func (h *Handlers) handleProgress(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("reset") == "true" && h.progress.ToMap()["complete"] == true {
		h.progress.Reset()
	}
	jsonResponse(w, http.StatusOK, h.progress.ToMap())
}

func (h *Handlers) handleHealth(w http.ResponseWriter, r *http.Request) {
	jsonResponse(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handlePreflight runs every invariant check on demand. Always 200: this is a
// report, never a gate — gated operations enforce blocking failures themselves.
func (h *Handlers) handlePreflight(w http.ResponseWriter, r *http.Request) {
	checks := h.preflight.RunAll()
	jsonResponse(w, http.StatusOK, map[string]interface{}{
		"ok":     services.InvariantsOK(checks),
		"checks": checks,
	})
}

// handleGetProfile reports the active runtime profile and the full available
// set (config/profiles.go). The active fields read the LIVE profile (cfg.Active)
// so a switch is reflected immediately; the available set is immutable.
func (h *Handlers) handleGetProfile(w http.ResponseWriter, r *http.Request) {
	available := make([]map[string]interface{}, 0, len(h.cfg.Profiles))
	for _, name := range config.ProfileNames(h.cfg.Profiles) {
		p := h.cfg.Profiles[name]
		available = append(available, map[string]interface{}{
			"name":         name,
			"description":  p.Description,
			"nodeHeapMB":   p.NodeHeapMB,
			"idleMode":     p.IdleMode,
			"driverMode":   p.DriverMode,
			"pinning":      p.Pinning,
			"bookCapacity": p.BookCapacity,
			"minMemMB":     p.MinMemMB,
			"simGlobalOps": p.SimGlobalOps,
			"governor":     p.Governor,
		})
	}
	activeName, activeProfile := h.cfg.Active()
	jsonResponse(w, http.StatusOK, map[string]interface{}{
		"active":    activeName,
		"profile":   activeProfile,
		"available": available,
	})
}

// handleSetProfile switches the whole stack onto a named profile, applying it
// via an in-process catalog rebuild + a quorum-safe roll (no admin restart, no
// jar build). Async: 202 + poll /api/admin/progress. {"force":true} overrides
// the switch-up memory guard and permits re-applying the already-active profile
// (a full re-roll to converge stragglers). Membership-changing switches
// (to/from the embedded-driver "light" profile) are refused with guidance.
func (h *Handlers) handleSetProfile(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name    string `json:"name"`
		Profile string `json:"profile"` // accepted alias for name
		Force   bool   `json:"force"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]string{"error": "body must be {\"name\": \"<profile>\", \"force\": <bool>}"})
		return
	}
	name := req.Name
	if name == "" {
		name = req.Profile
	}
	if name == "" {
		jsonResponse(w, http.StatusBadRequest, map[string]string{"error": "name is required"})
		return
	}
	if err := h.opsSvc.ApplyProfile(name, req.Force); err != nil {
		jsonResponse(w, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	}
	jsonResponse(w, http.StatusAccepted, map[string]string{
		"message": "Applying profile " + name + " — rolling services onto it. Poll GET /api/admin/progress.",
	})
}

// Node operations — cluster-scoped via ?cluster= (default = matching engine, so
// existing callers are unchanged). The descriptor supplies the node name + count
// and the ops service supplies the transitional-state tracker, so one code path
// drives both the matching engine and the assets engine.
func (h *Handlers) handleRestartNode(w http.ResponseWriter, r *http.Request) {
	ops := h.opsFor(r)
	c, tracker := ops.Cluster(), ops.Status()
	nodeId, err := h.getNodeId(r, c.NodeCount)
	if err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	name := c.NodeName(nodeId)
	log := logging.FromRequest(r)
	go func() {
		tracker.SetNodeStatus(nodeId, "STOPPING", false)
		if err := h.procMgr.Restart(name); err != nil {
			log.Error("restart-node failed", "cluster", c.Name, "node", nodeId, "err", err)
			tracker.SetNodeStatus(nodeId, "OFFLINE", false)
			return
		}
		tracker.SetNodeStatus(nodeId, "FOLLOWER", true)
	}()

	jsonResponse(w, http.StatusAccepted, map[string]string{
		"message": "Node " + strconv.Itoa(nodeId) + " restart initiated",
	})
}

func (h *Handlers) handleStopNode(w http.ResponseWriter, r *http.Request) {
	ops := h.opsFor(r)
	c, tracker := ops.Cluster(), ops.Status()
	nodeId, err := h.getNodeId(r, c.NodeCount)
	if err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	name := c.NodeName(nodeId)
	log := logging.FromRequest(r)
	go func() {
		tracker.SetNodeStatus(nodeId, "STOPPING", false)
		if err := h.procMgr.ForceStop(name); err != nil {
			log.Error("stop-node failed", "cluster", c.Name, "node", nodeId, "err", err)
		}
		tracker.SetNodeStatus(nodeId, "OFFLINE", false)
	}()

	jsonResponse(w, http.StatusAccepted, map[string]string{
		"message": "Node " + strconv.Itoa(nodeId) + " stop initiated",
	})
}

func (h *Handlers) handleStartNode(w http.ResponseWriter, r *http.Request) {
	ops := h.opsFor(r)
	c, tracker := ops.Cluster(), ops.Status()
	nodeId, err := h.getNodeId(r, c.NodeCount)
	if err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	name := c.NodeName(nodeId)
	log := logging.FromRequest(r)
	go func() {
		tracker.SetNodeStatus(nodeId, "STARTING", false)
		if err := h.procMgr.Start(name); err != nil {
			log.Error("start-node failed", "cluster", c.Name, "node", nodeId, "err", err)
			tracker.SetNodeStatus(nodeId, "OFFLINE", false)
			return
		}
		tracker.SetNodeStatus(nodeId, "FOLLOWER", true)
	}()

	jsonResponse(w, http.StatusAccepted, map[string]string{
		"message": "Node " + strconv.Itoa(nodeId) + " start initiated",
	})
}

func (h *Handlers) handleStopAllNodes(w http.ResponseWriter, r *http.Request) {
	ops := h.opsFor(r)
	c, tracker := ops.Cluster(), ops.Status()
	log := logging.FromRequest(r)
	go func() {
		for i := 0; i < c.NodeCount; i++ {
			name := c.NodeName(i)
			tracker.SetNodeStatus(i, "STOPPING", false)
			if err := h.procMgr.ForceStop(name); err != nil {
				log.Error("stop-all-nodes: node stop failed", "cluster", c.Name, "node", i, "err", err)
			}
			tracker.SetNodeStatus(i, "OFFLINE", false)
		}
	}()

	jsonResponse(w, http.StatusAccepted, map[string]string{
		"message": "All nodes stop initiated",
	})
}

func (h *Handlers) handleStartAllNodes(w http.ResponseWriter, r *http.Request) {
	ops := h.opsFor(r)
	c, tracker := ops.Cluster(), ops.Status()
	log := logging.FromRequest(r)
	go func() {
		for i := 0; i < c.NodeCount; i++ {
			name := c.NodeName(i)
			tracker.SetNodeStatus(i, "STARTING", false)
			if err := h.procMgr.Start(name); err != nil {
				log.Error("start-all-nodes: node start failed", "cluster", c.Name, "node", i, "err", err)
			}
		}
		// Wait and detect leader
		leader := c.DetectLeader()
		for i := 0; i < c.NodeCount; i++ {
			if i == leader {
				tracker.SetNodeStatus(i, "LEADER", true)
			} else {
				tracker.SetNodeStatus(i, "FOLLOWER", true)
			}
		}
	}()

	jsonResponse(w, http.StatusAccepted, map[string]string{
		"message": "All nodes start initiated",
	})
}

// Complex operations
// opsFor selects the OperationsService for the ?cluster= query param (default =
// the matching engine, so existing callers are unchanged). The same code path
// serves every registered cluster.
func (h *Handlers) opsFor(r *http.Request) *services.OperationsService {
	name := r.URL.Query().Get("cluster")
	if name != "" && h.clusterOps != nil {
		if ops, ok := h.clusterOps[name]; ok {
			return ops
		}
	}
	return h.opsSvc
}

func (h *Handlers) handleRollingUpdate(w http.ResponseWriter, r *http.Request) {
	// {"force": true} overrides pre-flight blocking failures (#43), the same
	// escape hatch as the snapshot/housekeeping lag guard.
	if err := h.opsFor(r).RollingUpdate(parseForce(r)); err != nil {
		jsonResponse(w, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	}
	jsonResponse(w, http.StatusAccepted, map[string]string{
		"message": "Rolling update initiated",
	})
}

func (h *Handlers) handleSnapshot(w http.ResponseWriter, r *http.Request) {
	if err := h.opsFor(r).Snapshot(parseForce(r)); err != nil {
		jsonResponse(w, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	}
	jsonResponse(w, http.StatusAccepted, map[string]string{
		"message": "Snapshot initiated",
	})
}

func (h *Handlers) handleRebuildGateway(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Restart bool `json:"restart"`
		Force   bool `json:"force"` // overrides pre-flight blocking failures
	}
	json.NewDecoder(r.Body).Decode(&req) // ignore error - defaults to false

	if err := h.opsSvc.RebuildGateway(req.Restart, req.Force); err != nil {
		jsonResponse(w, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	}
	msg := "Gateway rebuild initiated (isolated-tree build, staged install)"
	if req.Restart {
		msg += " (will restart the market gateway after install)"
	}
	jsonResponse(w, http.StatusAccepted, map[string]string{
		"message": msg,
	})
}

func (h *Handlers) handleRebuildOms(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Restart bool `json:"restart"`
		Force   bool `json:"force"` // overrides pre-flight blocking failures
	}
	json.NewDecoder(r.Body).Decode(&req) // ignore error - defaults to false

	if err := h.opsSvc.RebuildOms(req.Restart, req.Force); err != nil {
		jsonResponse(w, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	}
	msg := "OMS rebuild initiated (isolated-tree build, staged install)"
	if req.Restart {
		msg += " (will restart oms after install)"
	}
	jsonResponse(w, http.StatusAccepted, map[string]string{
		"message": msg,
	})
}

func (h *Handlers) handleRebuildCluster(w http.ResponseWriter, r *http.Request) {
	if err := h.opsFor(r).RebuildCluster(parseForce(r)); err != nil {
		jsonResponse(w, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	}
	jsonResponse(w, http.StatusAccepted, map[string]string{
		"message": "Cluster rebuild initiated (builds to staging, use rolling-update to deploy)",
	})
}

// parseForce reads an optional JSON body {"force": true} (match#35 lag-guard override).
func parseForce(r *http.Request) bool {
	var body struct {
		Force bool `json:"force"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	return body.Force
}

func (h *Handlers) handleHousekeeping(w http.ResponseWriter, r *http.Request) {
	ops := h.opsFor(r)
	// Capability refusal BEFORE the shared operation slot is claimed: a cluster with
	// no housekeeping tool must not wedge the global Progress (the #26 lesson).
	if ops.Cluster().HousekeepingMain == "" {
		jsonResponse(w, http.StatusBadRequest, map[string]string{
			"error": "cluster '" + ops.Cluster().Name + "' has no archive housekeeping",
		})
		return
	}
	if err := ops.Housekeeping(parseForce(r)); err != nil {
		jsonResponse(w, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	}
	jsonResponse(w, http.StatusAccepted, map[string]string{
		"message": "Archive housekeeping initiated",
	})
}

// Auto-snapshot handlers
func (h *Handlers) handleAutoSnapshotGet(w http.ResponseWriter, r *http.Request) {
	jsonResponse(w, http.StatusOK, h.autoSnapshot.ToMap())
}

func (h *Handlers) handleAutoSnapshotPost(w http.ResponseWriter, r *http.Request) {
	var req struct {
		IntervalMinutes int64 `json:"intervalMinutes"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.IntervalMinutes <= 0 {
		jsonResponse(w, http.StatusBadRequest, map[string]string{
			"error": "intervalMinutes must be a positive number",
		})
		return
	}

	h.autoSnapshot.Start(req.IntervalMinutes)
	jsonResponse(w, http.StatusOK, map[string]interface{}{
		"status":          "started",
		"intervalMinutes": req.IntervalMinutes,
		"message":         "Auto-snapshot enabled: every " + strconv.FormatInt(req.IntervalMinutes, 10) + " minutes",
	})
}

func (h *Handlers) handleAutoSnapshotDelete(w http.ResponseWriter, r *http.Request) {
	h.autoSnapshot.Stop()
	jsonResponse(w, http.StatusOK, map[string]interface{}{
		"status":  "stopped",
		"message": "Auto-snapshot disabled",
	})
}

// Logs handler
func (h *Handlers) handleLogs(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()

	lines := 50
	if l := query.Get("lines"); l != "" {
		if parsed, err := strconv.Atoi(l); err == nil {
			lines = parsed
			if lines > 500 {
				lines = 500
			}
		}
	}

	if service := query.Get("service"); service != "" {
		jsonResponse(w, http.StatusOK, h.logSvc.GetServiceLogs(service, lines))
		return
	}

	nodeId := 0
	if n := query.Get("node"); n != "" {
		if parsed, err := strconv.Atoi(n); err == nil {
			nodeId = parsed
		}
	}

	// Cluster-aware node log file: node<id> for the matching engine (default),
	// ae<id> for the assets engine, resolved from the ?cluster= descriptor.
	name := h.opsFor(r).Cluster().NodeName(nodeId)
	jsonResponse(w, http.StatusOK, h.logSvc.GetNodeLogsNamed(name, nodeId, lines))
}

// Cleanup handler
func (h *Handlers) handleCleanup(w http.ResponseWriter, r *http.Request) {
	var opts services.CleanupOptions
	json.NewDecoder(r.Body).Decode(&opts) // ignore error - defaults to false values
	result := h.opsFor(r).Cleanup(opts)
	status := http.StatusOK
	if success, ok := result["success"].(bool); ok && !success {
		status = http.StatusBadRequest
	}
	jsonResponse(w, status, result)
}

// CleanupNode handler for per-node cleanup
func (h *Handlers) handleCleanupNode(w http.ResponseWriter, r *http.Request) {
	var req struct {
		NodeId int  `json:"nodeId"`
		Force  bool `json:"force"`
		DryRun bool `json:"dryRun"`
	}
	json.NewDecoder(r.Body).Decode(&req)
	result := h.opsFor(r).CleanupNode(req.NodeId, req.Force, req.DryRun)
	status := http.StatusOK
	if success, ok := result["success"].(bool); ok && !success {
		status = http.StatusBadRequest
	}
	jsonResponse(w, status, result)
}

// BackupInfo handler
func (h *Handlers) handleBackupInfo(w http.ResponseWriter, r *http.Request) {
	ops := h.opsFor(r)
	if ops.Cluster().BackupDir == "" {
		jsonResponse(w, http.StatusBadRequest, map[string]string{
			"error": "cluster '" + ops.Cluster().Name + "' has no backup",
		})
		return
	}
	jsonResponse(w, http.StatusOK, ops.GetBackupInfo())
}

// RecoverFromBackup handler
func (h *Handlers) handleRecoverFromBackup(w http.ResponseWriter, r *http.Request) {
	ops := h.opsFor(r)
	if ops.Cluster().BackupDir == "" {
		jsonResponse(w, http.StatusBadRequest, map[string]string{
			"error": "cluster '" + ops.Cluster().Name + "' has no backup to recover from",
		})
		return
	}
	var req struct {
		NodeId int  `json:"nodeId"`
		Force  bool `json:"force"`
		DryRun bool `json:"dryRun"`
	}
	json.NewDecoder(r.Body).Decode(&req)
	result := ops.RecoverFromBackup(req.NodeId, req.Force, req.DryRun)
	status := http.StatusOK
	if success, ok := result["success"].(bool); ok && !success {
		status = http.StatusBadRequest
	}
	jsonResponse(w, status, result)
}

// --- Process Manager Handlers ---

func (h *Handlers) handleProcessList(w http.ResponseWriter, r *http.Request) {
	jsonResponse(w, http.StatusOK, h.procMgr.List())
}

func (h *Handlers) handleProcessSummary(w http.ResponseWriter, r *http.Request) {
	jsonResponse(w, http.StatusOK, h.procMgr.Summary())
}

func (h *Handlers) handleProcessGet(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	info := h.procMgr.Get(name)
	if info == nil {
		jsonResponse(w, http.StatusNotFound, map[string]string{"error": "unknown service: " + name})
		return
	}
	jsonResponse(w, http.StatusOK, info)
}

func (h *Handlers) handleProcessStart(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	if err := h.procMgr.Start(name); err != nil {
		jsonResponse(w, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	}
	jsonResponse(w, http.StatusAccepted, map[string]string{
		"message": name + " start initiated",
	})
}

func (h *Handlers) handleProcessStop(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	if err := h.procMgr.Stop(name); err != nil {
		jsonResponse(w, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	}
	jsonResponse(w, http.StatusAccepted, map[string]string{
		"message": name + " stop initiated",
	})
}

func (h *Handlers) handleProcessRestart(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	if err := h.procMgr.Restart(name); err != nil {
		jsonResponse(w, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	}
	jsonResponse(w, http.StatusAccepted, map[string]string{
		"message": name + " restart initiated",
	})
}

func (h *Handlers) handleProcessForceStop(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	if err := h.procMgr.ForceStop(name); err != nil {
		jsonResponse(w, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	}
	jsonResponse(w, http.StatusAccepted, map[string]string{
		"message": name + " force-stop initiated",
	})
}

func (h *Handlers) handleProcessStartAll(w http.ResponseWriter, r *http.Request) {
	go func() {
		// Runs in background — dependency-ordered start takes time
		h.procMgr.StartAll()
	}()
	jsonResponse(w, http.StatusAccepted, map[string]string{
		"message": "Start-all initiated (dependency-ordered)",
	})
}

func (h *Handlers) handleProcessStopAll(w http.ResponseWriter, r *http.Request) {
	go func() {
		h.procMgr.StopAll()
	}()
	jsonResponse(w, http.StatusAccepted, map[string]string{
		"message": "Stop-all initiated (reverse dependency order)",
	})
}

func (h *Handlers) handleProcessRestartAll(w http.ResponseWriter, r *http.Request) {
	go func() {
		h.procMgr.RestartAll()
	}()
	jsonResponse(w, http.StatusAccepted, map[string]string{
		"message": "Restart-all initiated (stop reverse → start forward)",
	})
}

// Self-update: rebuild admin gateway binary and restart via systemd
func (h *Handlers) handleRebuildAdmin(w http.ResponseWriter, r *http.Request) {
	if err := h.opsSvc.RebuildAdmin(); err != nil {
		jsonResponse(w, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	}
	jsonResponse(w, http.StatusAccepted, map[string]string{
		"message": "Admin gateway self-update initiated. Service will restart momentarily. " +
			"Poll GET /api/admin/rebuild-status for post-restart verification.",
	})
}

// handleReseedNode launches the stranded-member reseed: copy a healthy
// follower's state over a corrupt member's (match#35 procedure, automated).
func (h *Handlers) handleReseedNode(w http.ResponseWriter, r *http.Request) {
	ops := h.opsFor(r)
	// Reseed copies a healthy follower's state over a stranded member: it needs a
	// distinct source, so a single-node cluster has nothing to reseed from. Refuse
	// before the shared operation slot is claimed.
	if ops.Cluster().NodeCount < 2 {
		jsonResponse(w, http.StatusBadRequest, map[string]string{
			"error": "cluster '" + ops.Cluster().Name + "' is single-node; reseed needs a source follower",
		})
		return
	}
	var req struct {
		NodeId       *int `json:"nodeId"`
		SourceNodeId *int `json:"sourceNodeId"`
		Force        bool `json:"force"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.NodeId == nil || req.SourceNodeId == nil {
		jsonResponse(w, http.StatusBadRequest, map[string]string{
			"error": "body must be {\"nodeId\": <stranded>, \"sourceNodeId\": <healthy follower>, \"force\": true}",
		})
		return
	}
	if err := ops.ReseedNode(*req.NodeId, *req.SourceNodeId, req.Force); err != nil {
		jsonResponse(w, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	}
	jsonResponse(w, http.StatusAccepted, map[string]string{
		"message": fmt.Sprintf("Reseeding node%d from node%d — the source follower stops during the copy "+
			"(brief quorum outage). Poll /api/admin/progress.", *req.NodeId, *req.SourceNodeId),
	})
}

// handleRebuildStatus reports the rebuild-admin verification handshake:
// pending (restart in flight), verified (the new process came up and checked
// its own binary), or none.
func (h *Handlers) handleRebuildStatus(w http.ResponseWriter, r *http.Request) {
	jsonResponse(w, http.StatusOK, h.opsSvc.RebuildStatus())
}

// Helpers
func (h *Handlers) getNodeId(r *http.Request, nodeCount int) (int, error) {
	var req struct {
		NodeId int `json:"nodeId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		return 0, err
	}
	if req.NodeId < 0 || req.NodeId >= nodeCount {
		return 0, &InvalidNodeError{max: nodeCount - 1}
	}
	return req.NodeId, nil
}

type InvalidNodeError struct{ max int }

func (e *InvalidNodeError) Error() string {
	return fmt.Sprintf("Invalid nodeId. Must be 0..%d", e.max)
}

func jsonResponse(w http.ResponseWriter, status int, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(data)
}
