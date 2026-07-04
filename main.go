package main

import (
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/match/admin-gateway/config"
	"github.com/match/admin-gateway/handlers"
	"github.com/match/admin-gateway/services"
)

func main() {
	// Load configuration
	cfg := config.Load()

	// Initialize services
	systemd := services.NewSystemd()   // only used by OperationsService for admin gateway self-restart
	cluster := services.NewCluster(cfg)
	progress := services.NewProgress()
	clusterStatus := services.NewClusterStatus()
	procMgr := services.NewProcessManager(cfg)

	statusSvc := services.NewStatusService(cfg, cluster, clusterStatus)
	statusSvc.SetProcessManager(procMgr)
	opsSvc := services.NewOperationsService(cfg, systemd, cluster, progress, clusterStatus)
	opsSvc.SetProcessManager(procMgr)
	opsSvc.SetStatusService(statusSvc)
	autoSnapshot := services.NewAutoSnapshot(opsSvc)
	statusSvc.SetAutoSnapshot(autoSnapshot)
	autoSnapshot.Start(5) // Auto-snapshot every 5 minutes to prevent unbounded log growth
	logSvc := services.NewLogService(cfg)

	// Initialize handlers
	h := handlers.New(statusSvc, opsSvc, cluster, progress, clusterStatus, autoSnapshot, logSvc, procMgr)

	// Setup router
	r := chi.NewRouter()
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)
	r.Use(middleware.RealIP)
	r.Use(handlers.AuthMiddleware(cfg.AuthToken))

	h.RegisterRoutes(r)

	// Secure-by-default (admin-gateway#11): loopback bind unless overridden;
	// a non-loopback bind without a token would expose every destructive op,
	// so refuse to start in that combination.
	if cfg.AuthToken == "" {
		if ip := net.ParseIP(cfg.BindAddr); ip == nil || !ip.IsLoopback() {
			log.Fatalf("Refusing to bind %s without an auth token: set ADMIN_AUTH_TOKEN(_FILE) "+
				"or bind loopback (ADMIN_BIND=127.0.0.1)", cfg.BindAddr)
		}
		log.Printf("⚠️  AUTH: no admin token configured — loopback-only dev mode")
	}

	// Start server
	addr := cfg.BindAddr + ":" + cfg.Port
	log.Printf("🚀 Admin Gateway starting on %s", addr)
	log.Printf("   Project: %s", cfg.ProjectDir)
	log.Printf("   JAR: %s", cfg.JarPath)

	// Graceful shutdown
	server := &http.Server{
		Addr:    addr,
		Handler: r,
	}

	go func() {
		sigChan := make(chan os.Signal, 1)
		signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
		<-sigChan
		log.Println("Shutting down...")
		autoSnapshot.Stop()
		procMgr.Shutdown()
		statusSvc.Stop()
		server.Close()
	}()

	if err := server.ListenAndServe(); err != http.ErrServerClosed {
		fmt.Fprintf(os.Stderr, "Server error: %v\n", err)
		os.Exit(1)
	}
}
