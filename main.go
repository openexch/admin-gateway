// SPDX-License-Identifier: Apache-2.0
package main

import (
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/match/admin-gateway/agent"
	"github.com/match/admin-gateway/config"
	"github.com/match/admin-gateway/handlers"
	"github.com/match/admin-gateway/logging"
	"github.com/match/admin-gateway/services"
)

func main() {
	// Load configuration
	cfg := config.Load()
	logging.Setup(cfg.LogFormat)

	// Active runtime profile (config/profiles.go): drives the service catalog
	// heaps/idle/pinning/etc. and the preflight mem gate. Apply the live OS knobs
	// (governor/THP) best-effort now; the catalog knobs take effect as services
	// (re)start.
	slog.Info("active runtime profile",
		"profile", cfg.ProfileName, "nodeHeapMB", cfg.Profile.NodeHeapMB,
		"idle", cfg.Profile.IdleMode, "driver", cfg.Profile.DriverProfile,
		"pinning", cfg.Profile.Pinning, "minMemMB", cfg.Profile.MinMemMB)
	services.ApplyProfileOSKnobs(cfg.Profile)

	// Initialize services
	systemd := services.NewSystemd() // only used by OperationsService for admin gateway self-restart
	// Cluster descriptors: the matching engine (existing) + the assets engine. Every
	// management op runs against one of these via the same code path. The existing
	// singletons stay bound to the matching engine so its behavior is unchanged;
	// the assets descriptor is registered for cluster-scoped management.
	matchCluster := services.NewMatchCluster(cfg)
	assetsCluster := services.NewAssetsCluster(cfg)
	cluster := matchCluster
	progress := services.NewProgress()
	clusterStatus := services.NewClusterStatus()
	// The in-process LocalAgent. Everything downstream depends only on the
	// agent contract (docs/AGENT-ARCHITECTURE.md), so a remote agentd client
	// can slot in per host later.
	var procMgr agent.ProcessAgent = services.NewProcessManager(cfg)

	statusSvc := services.NewStatusService(cfg, cluster, clusterStatus)
	statusSvc.SetProcessManager(procMgr)
	opsSvc := services.NewOperationsService(cfg, systemd, cluster, progress, clusterStatus)
	opsSvc.SetProcessManager(procMgr)
	opsSvc.SetStatusService(statusSvc)

	// Assets Engine ops: the SAME OperationsService code, bound to the assets
	// descriptor + its own status tracker. Intentionally no preflight/statusSvc —
	// a single-node cluster has no quorum to gate, so its "rolling update"
	// degenerates to a swap-and-restart (see doRollingUpdate's NodeCount==1 path).
	assetsClusterStatus := services.NewClusterStatus()
	assetsOps := services.NewOperationsService(cfg, systemd, assetsCluster, progress, assetsClusterStatus)
	assetsOps.SetProcessManager(procMgr)
	// Surface the AE as a first-class cluster in /status, sharing the assets ops'
	// transitional-state tracker so node stops/starts show STOPPING/STARTING.
	statusSvc.SetAssetsCluster(assetsCluster, assetsClusterStatus)
	clusterOps := map[string]*services.OperationsService{
		matchCluster.Name:  opsSvc,
		assetsCluster.Name: assetsOps,
	}
	preflight := services.NewPreflight(cfg)
	preflight.SetProcessManager(procMgr)
	preflight.SetStatusService(statusSvc)
	statusSvc.SetPreflight(preflight)
	opsSvc.SetPreflight(preflight)
	autoSnapshot := services.NewAutoSnapshot(opsSvc)
	statusSvc.SetAutoSnapshot(autoSnapshot)
	autoSnapshot.Start(5) // Auto-snapshot every 5 minutes to prevent unbounded log growth
	logSvc := services.NewLogService(cfg)
	metricsSvc := services.NewMetricsService(statusSvc, opsSvc, procMgr, progress, preflight)

	// Initialize handlers
	h := handlers.New(statusSvc, opsSvc, clusterOps, cluster, progress, clusterStatus, autoSnapshot, logSvc, procMgr, metricsSvc, preflight, cfg)

	// Setup router
	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(logging.RequestLogger)
	r.Use(metricsSvc.Middleware)
	r.Use(middleware.Recoverer)
	r.Use(middleware.RealIP)
	r.Use(handlers.AuthMiddleware(cfg.AuthToken))

	h.RegisterRoutes(r)

	// Secure-by-default (admin-gateway#11): loopback bind unless overridden;
	// a non-loopback bind without a token would expose every destructive op,
	// so refuse to start in that combination.
	if cfg.AuthToken == "" {
		if ip := net.ParseIP(cfg.BindAddr); ip == nil || !ip.IsLoopback() {
			slog.Error("refusing to bind without an auth token: set ADMIN_AUTH_TOKEN(_FILE) or bind loopback (ADMIN_BIND=127.0.0.1)",
				"bind", cfg.BindAddr)
			os.Exit(1)
		}
		slog.Warn("no admin token configured, loopback-only dev mode")
	}

	// Opt-in agent hub (docs/AGENTD.md): with ADMIN_AGENT_LISTEN unset the
	// hub is never constructed and the gateway is byte-identical to
	// pre-agentd builds. Security misconfiguration (non-loopback without
	// token+TLS) fails fast like the HTTP bind rule; OPERATIONAL failures
	// (port conflicts) only disable the hub — the gateway's primary job is
	// managing the cluster and must not crash-loop over an optional listener.
	if cfg.AgentListen != "" {
		if err := validateAgentListen(cfg); err != nil {
			slog.Error("agent hub misconfigured", "err", err)
			os.Exit(1)
		}
		if agentSrv, err := startAgentHub(cfg); err != nil {
			slog.Error("agent hub disabled (operational failure)", "err", err)
		} else {
			defer agentSrv.Stop()
		}
	}

	// Start server
	addr := cfg.BindAddr + ":" + cfg.Port
	slog.Info("admin gateway starting",
		"addr", addr, "project", cfg.ProjectDir, "jar", cfg.JarPath)

	// Graceful shutdown
	server := &http.Server{
		Addr:    addr,
		Handler: r,
	}

	// If this start is the back half of a rebuild-admin, report the handshake
	// once we are actually serving (self-probe /health, then promote
	// rebuild-pending.json to rebuild-result.json).
	go func() {
		probe := "http://127.0.0.1:" + cfg.Port + "/health"
		client := &http.Client{Timeout: 1 * time.Second}
		for i := 0; i < 30; i++ {
			if resp, err := client.Get(probe); err == nil {
				resp.Body.Close()
				if resp.StatusCode == http.StatusOK {
					opsSvc.FinalizeRebuildVerification()
					return
				}
			}
			time.Sleep(500 * time.Millisecond)
		}
	}()

	go func() {
		sigChan := make(chan os.Signal, 1)
		signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
		<-sigChan
		slog.Info("shutting down")
		autoSnapshot.Stop()
		procMgr.Close()
		statusSvc.Stop()
		server.Close()
	}()

	if err := server.ListenAndServe(); err != http.ErrServerClosed {
		slog.Error("server error", "err", err)
		os.Exit(1)
	}
}
