// SPDX-License-Identifier: Apache-2.0
package main

import (
	"strings"
	"testing"

	"github.com/match/admin-gateway/config"
)

func TestStartAgentHubLoopbackNoTokenAllowed(t *testing.T) {
	srv, err := startAgentHub(&config.Config{AgentListen: "127.0.0.1:0"})
	if err != nil {
		t.Fatalf("loopback tokenless listener should start (dev mode), got %v", err)
	}
	srv.Stop()
}

func TestStartAgentHubNonLoopbackRefusedWithoutTokenAndTLS(t *testing.T) {
	cases := []config.Config{
		{AgentListen: "0.0.0.0:0"},                                          // neither
		{AgentListen: "0.0.0.0:0", AgentToken: "t"},                         // token only
		{AgentListen: "0.0.0.0:0", AgentTLSCert: "/x.pem", AgentTLSKey: ""}, // cert only
	}
	for _, cfg := range cases {
		if srv, err := startAgentHub(&cfg); err == nil {
			srv.Stop()
			t.Fatalf("non-loopback listener must refuse without token+TLS: %+v", cfg)
		} else if !strings.Contains(err.Error(), "refusing") && !strings.Contains(err.Error(), "TLS") {
			t.Fatalf("unexpected refusal error: %v", err)
		}
	}
}

func TestStartAgentHubBadAddress(t *testing.T) {
	if srv, err := startAgentHub(&config.Config{AgentListen: "not-an-addr"}); err == nil {
		srv.Stop()
		t.Fatal("malformed listen address must error")
	}
}
