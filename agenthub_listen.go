// SPDX-License-Identifier: Apache-2.0
package main

import (
	"crypto/tls"
	"fmt"
	"log/slog"
	"net"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	"github.com/match/admin-gateway/agenthub"
	"github.com/match/admin-gateway/agentwire"
	"github.com/match/admin-gateway/config"
)

// startAgentHub serves the agent hub on cfg.AgentListen. Nothing in the
// gateway consumes remote agents yet (handlers keep the in-process
// LocalAgent until topology, milestone 4) — this listener exists for the
// loopback pair and for early multi-host experiments.
func startAgentHub(cfg *config.Config) (*grpc.Server, error) {
	host, _, err := net.SplitHostPort(cfg.AgentListen)
	if err != nil {
		return nil, fmt.Errorf("ADMIN_AGENT_LISTEN %q: %w", cfg.AgentListen, err)
	}
	ip := net.ParseIP(host)
	loopback := ip != nil && ip.IsLoopback()
	if !loopback && (cfg.AgentToken == "" || cfg.AgentTLSCert == "") {
		return nil, fmt.Errorf("refusing a non-loopback agent listener without both ADMIN_AGENT_TOKEN(_FILE) and ADMIN_AGENT_TLS_CERT/KEY")
	}

	hub := agenthub.NewHub(cfg.AgentToken)
	serverOpts := hub.ServerOptions()
	if cfg.AgentTLSCert != "" {
		cert, err := tls.LoadX509KeyPair(cfg.AgentTLSCert, cfg.AgentTLSKey)
		if err != nil {
			return nil, fmt.Errorf("agent TLS keypair: %w", err)
		}
		serverOpts = append(serverOpts, grpc.Creds(credentials.NewTLS(&tls.Config{
			Certificates: []tls.Certificate{cert},
			MinVersion:   tls.VersionTLS13,
		})))
	}

	lis, err := net.Listen("tcp", cfg.AgentListen)
	if err != nil {
		return nil, fmt.Errorf("agent listener: %w", err)
	}
	srv := grpc.NewServer(serverOpts...)
	agentwire.RegisterControlPlaneServer(srv, hub)
	go srv.Serve(lis)
	slog.Info("agent hub listening", "addr", cfg.AgentListen,
		"tls", cfg.AgentTLSCert != "", "token", cfg.AgentToken != "")
	return srv, nil
}
