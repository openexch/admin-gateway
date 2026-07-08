// SPDX-License-Identifier: Apache-2.0
package main

import (
	"net"
	"strings"
	"testing"

	"github.com/match/admin-gateway/config"
)

func TestStartAgentHubLoopbackNoTokenAllowed(t *testing.T) {
	cfg := &config.Config{AgentListen: "127.0.0.1:0"}
	if err := validateAgentListen(cfg); err != nil {
		t.Fatalf("loopback tokenless config should validate (dev mode), got %v", err)
	}
	srv, err := startAgentHub(cfg)
	if err != nil {
		t.Fatalf("loopback tokenless listener should start, got %v", err)
	}
	srv.Stop()
}

func TestValidateNonLoopbackRefusedWithoutTokenAndTLS(t *testing.T) {
	// Pointers, not values: config.Config carries a sync.RWMutex (the live
	// profile guard) and must never be copied.
	cases := []*config.Config{
		{AgentListen: "0.0.0.0:0"},                                          // neither
		{AgentListen: "0.0.0.0:0", AgentToken: "t"},                         // token only
		{AgentListen: "0.0.0.0:0", AgentTLSCert: "/x.pem", AgentTLSKey: ""}, // cert only
	}
	for _, cfg := range cases {
		if err := validateAgentListen(cfg); err == nil {
			t.Fatalf("non-loopback listener must refuse without token+TLS: %s", cfg.AgentListen)
		} else if !strings.Contains(err.Error(), "refusing") && !strings.Contains(err.Error(), "TLS") {
			t.Fatalf("unexpected refusal error: %v", err)
		}
	}
}

func TestValidateBadAddress(t *testing.T) {
	if err := validateAgentListen(&config.Config{AgentListen: "not-an-addr"}); err == nil {
		t.Fatal("malformed listen address must error")
	}
}

// The first live drill's lesson: a taken port must surface as a returnable
// operational error (main degrades without the hub), never a crash-loop of
// the process manager.
func TestStartAgentHubPortConflictIsReturnedNotFatal(t *testing.T) {
	taken, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer taken.Close()

	_, err = startAgentHub(&config.Config{AgentListen: taken.Addr().String()})
	if err == nil || !strings.Contains(err.Error(), "address already in use") {
		t.Fatalf("expected a bind-conflict error, got %v", err)
	}
}
