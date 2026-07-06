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

	// Initialize services
	systemd := services.NewSystemd()   // only used by OperationsService for admin gateway self-restart
	cluster := services.NewCluster(cfg)
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
	h := handlers.New(statusSvc, opsSvc, cluster, progress, clusterStatus, autoSnapshot, logSvc, procMgr, metricsSvc, preflight)

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
	// pre-agentd builds. Same secure-by-default rule as the HTTP API, plus
	// TLS: agent sessions drive process lifecycle, so a non-loopback
	// listener refuses to start without BOTH a token and TLS.
	if cfg.AgentListen != "" {
		agentSrv, err := startAgentHub(cfg)
		if err != nil {
			slog.Error("agent hub failed to start", "err", err)
			os.Exit(1)
		}
		defer agentSrv.Stop()
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
