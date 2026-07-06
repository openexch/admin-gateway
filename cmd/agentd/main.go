// SPDX-License-Identifier: Apache-2.0

// agentd is the per-host process agent (docs/AGENT-ARCHITECTURE.md Horizon
// B): it dials the control plane and executes process-lifecycle commands
// against its embedded ProcessManager. In milestone 3 its catalog is EMPTY —
// the real catalog arrives with the topology milestone — which makes it
// provably harmless next to a gateway running the in-process LocalAgent on
// the same box (no shared PID files, no adoption split-brain). TailLog,
// NodeCounters and InstallArtifact are catalog-independent and fully live.
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/match/admin-gateway/agentd"
	"github.com/match/admin-gateway/logging"
	"github.com/match/admin-gateway/services"
)

var version = "dev" // set via -ldflags at release time

func main() {
	var (
		control   = flag.String("control", "", "control-plane address (host:port); required")
		hostID    = flag.String("host-id", "", "stable host identity; required")
		tokenFile = flag.String("token-file", "", "file containing the agent token (or AGENTD_TOKEN)")
		tlsCA     = flag.String("tls-ca", "", "PEM file to pin the control plane's TLS CA")
		insecure  = flag.Bool("insecure", false, "dial without TLS (loopback only)")
		logDir    = flag.String("log-dir", "", "managed-process log dir (default ~/.local/log/cluster)")
		pidDir    = flag.String("pid-dir", "", "managed-process pid dir (default ~/.local/run/match)")
	)
	flag.Parse()
	logging.Setup(envOr("AGENTD_LOG_FORMAT", "json"))

	if *control == "" || *hostID == "" {
		fmt.Fprintln(os.Stderr, "agentd: -control and -host-id are required")
		os.Exit(2)
	}

	token := os.Getenv("AGENTD_TOKEN")
	if *tokenFile != "" {
		data, err := os.ReadFile(*tokenFile)
		if err != nil {
			slog.Error("cannot read token file", "file", *tokenFile, "err", err)
			os.Exit(1)
		}
		token = strings.TrimSpace(string(data))
	}

	var tlsCfg *tls.Config
	if *tlsCA != "" {
		pem, err := os.ReadFile(*tlsCA)
		if err != nil {
			slog.Error("cannot read CA file", "file", *tlsCA, "err", err)
			os.Exit(1)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			slog.Error("no certificates in CA file", "file", *tlsCA)
			os.Exit(1)
		}
		tlsCfg = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS13}
	}
	if tlsCfg == nil && !*insecure {
		slog.Error("refusing to dial without TLS: pass -tls-ca, or -insecure for loopback")
		os.Exit(1)
	}

	// Milestone 3: empty catalog (see the package comment).
	pm := services.NewProcessManagerWith(services.ProcessManagerOptions{
		LogDir: *logDir,
		PidDir: *pidDir,
	})

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		sigs := make(chan os.Signal, 1)
		signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
		<-sigs
		slog.Info("shutting down")
		cancel()
		// KillMode=process semantics: never touch managed processes on exit.
		pm.Close()
	}()

	slog.Info("agentd starting", "version", version, "control", *control, "host", *hostID)
	if err := agentd.Run(ctx, agentd.Config{
		Control:      *control,
		HostID:       *hostID,
		Token:        token,
		TLS:          tlsCfg,
		Insecure:     *insecure,
		AgentVersion: version,
	}, pm); err != nil && ctx.Err() == nil {
		slog.Error("agentd exited", "err", err)
		os.Exit(1)
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
