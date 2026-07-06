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

// validateAgentListen enforces the security matrix for the agent listener.
// A violation here is a misconfiguration the gateway must refuse to start
// with (fail fast, same stance as the HTTP bind rule).
func validateAgentListen(cfg *config.Config) error {
	host, _, err := net.SplitHostPort(cfg.AgentListen)
	if err != nil {
		return fmt.Errorf("ADMIN_AGENT_LISTEN %q: %w", cfg.AgentListen, err)
	}
	ip := net.ParseIP(host)
	if loopback := ip != nil && ip.IsLoopback(); !loopback && (cfg.AgentToken == "" || cfg.AgentTLSCert == "") {
		return fmt.Errorf("refusing a non-loopback agent listener without both ADMIN_AGENT_TOKEN(_FILE) and ADMIN_AGENT_TLS_CERT/KEY")
	}
	return nil
}

// startAgentHub serves the agent hub on cfg.AgentListen (validate first).
// Nothing in the gateway consumes remote agents yet (handlers keep the
// in-process LocalAgent until topology, milestone 4) — this listener exists
// for the loopback pair and for early multi-host experiments. Errors here
// are OPERATIONAL (port conflicts): callers degrade without the hub instead
// of dying — the gateway's primary job is managing the cluster, and a taken
// port crash-looped the whole process manager in the first live drill.
func startAgentHub(cfg *config.Config) (*grpc.Server, error) {
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
