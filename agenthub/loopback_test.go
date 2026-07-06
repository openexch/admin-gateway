// SPDX-License-Identifier: Apache-2.0

// Loopback parity: the RemoteAgent↔agentd pair must behave exactly like the
// in-process LocalAgent. The proof is running the SAME conformance suite the
// LocalAgent passes, with every verb crossing the wire, plus the remote-only
// behaviors (auth, reconnect, displacement, chunked artifacts).
package agenthub_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/test/bufconn"

	"github.com/match/admin-gateway/agent"
	"github.com/match/admin-gateway/agent/agenttest"
	"github.com/match/admin-gateway/agentd"
	"github.com/match/admin-gateway/agenthub"
	"github.com/match/admin-gateway/agentwire"
	"github.com/match/admin-gateway/services"
)

const testToken = "loopback-test-token"

// startHub serves a Hub over bufconn and returns it plus dial options that
// reach it.
func startHub(t *testing.T, token string) (*agenthub.Hub, []grpc.DialOption) {
	t.Helper()
	lis := bufconn.Listen(1 << 20)
	hub := agenthub.NewHub(token)
	srv := grpc.NewServer(hub.ServerOptions()...)
	agentwire.RegisterControlPlaneServer(srv, hub)
	go srv.Serve(lis)
	t.Cleanup(srv.Stop)
	dialer := grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
		return lis.DialContext(ctx)
	})
	return hub, []grpc.DialOption{dialer}
}

// newLocalPM builds a real ProcessManager with a fake catalog in temp dirs.
func newLocalPM(t *testing.T) (agent.ProcessAgent, agenttest.Fixture) {
	t.Helper()
	dir := t.TempDir()
	logDir := filepath.Join(dir, "logs")
	scratch := filepath.Join(dir, "scratch")
	if err := os.MkdirAll(scratch, 0755); err != nil {
		t.Fatal(err)
	}
	pm := services.NewProcessManagerWith(services.ProcessManagerOptions{
		Services: []services.ServiceDef{
			{Name: "conf-a", Display: "Conformance A", Role: services.RoleInfra,
				Command: []string{"/bin/sh", "-c", "sleep 60"}, StopTimeout: 1},
			{Name: "conf-b", Display: "Conformance B", Role: services.RoleInfra,
				Command:     []string{"/bin/sh", "-c", "sleep 60"},
				AutoRestart: true, RestartSec: 1, StopTimeout: 1},
		},
		LogDir: logDir,
		PidDir: filepath.Join(dir, "pids"),
	})
	t.Cleanup(func() {
		pm.StopAll()
		pm.Close()
	})
	return pm, agenttest.Fixture{
		Simple: "conf-a", AutoRestart: "conf-b", LogDir: logDir, Scratch: scratch,
	}
}

// startAgentd runs the agent session loop against the hub and waits for the
// session to establish. Returns a cancel that ends the agent.
func startAgentd(t *testing.T, hub *agenthub.Hub, dial []grpc.DialOption, hostID, token string, pa agent.ProcessAgent) context.CancelFunc {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	go agentd.Run(ctx, agentd.Config{
		Control: "passthrough:///bufnet", HostID: hostID, Token: token,
		Insecure: true, AgentVersion: "test",
		BackoffMin: 50 * time.Millisecond, BackoffMax: 250 * time.Millisecond,
		ExtraDialOptions: dial,
	}, pa)
	waitConnected(t, hub, hostID, 10*time.Second)
	t.Cleanup(cancel)
	return cancel
}

func waitConnected(t *testing.T, hub *agenthub.Hub, hostID string, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if hub.Connected(hostID) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("agent %q did not connect within %s", hostID, d)
}

// TestRemoteAgentConformance is loopback parity itself: the identical suite
// the LocalAgent passes, with every verb crossing the bufconn wire.
func TestRemoteAgentConformance(t *testing.T) {
	agenttest.RunConformance(t, func(t *testing.T) (agent.ProcessAgent, agenttest.Fixture) {
		pm, fx := newLocalPM(t)
		hub, dial := startHub(t, testToken)
		startAgentd(t, hub, dial, "loopback-host", testToken, pm)
		return hub.Agent("loopback-host"), fx
	})
}

func TestRemoteAgentNotConnectedFailsFast(t *testing.T) {
	hub, _ := startHub(t, testToken)
	ra := hub.Agent("ghost-host")

	start := time.Now()
	err := ra.Start("anything")
	if err == nil || !strings.Contains(err.Error(), "not connected") {
		t.Fatalf("expected a not-connected error, got %v", err)
	}
	if time.Since(start) > time.Second {
		t.Fatalf("disconnected host must fail fast, took %s", time.Since(start))
	}
	if got := ra.Get("anything"); got != nil {
		t.Fatalf("Get on a disconnected host must be nil, got %+v", got)
	}
}

func TestBadTokenRejected(t *testing.T) {
	hub, dial := startHub(t, testToken)
	pm, _ := newLocalPM(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go agentd.Run(ctx, agentd.Config{
		Control: "passthrough:///bufnet", HostID: "intruder", Token: "wrong",
		Insecure: true, BackoffMin: 20 * time.Millisecond, BackoffMax: 50 * time.Millisecond,
		ExtraDialOptions: dial,
	}, pm)

	time.Sleep(500 * time.Millisecond) // several retry rounds
	if hub.Connected("intruder") {
		t.Fatal("agent with a bad token must never register")
	}
}

func TestReconnectRestoresServiceAndEvents(t *testing.T) {
	pm, fx := newLocalPM(t)
	hub, dial := startHub(t, testToken)
	cancelFirst := startAgentd(t, hub, dial, "host-r", testToken, pm)
	ra := hub.Agent("host-r")

	// Subscribers attached before the drop must keep working after the
	// reconnect (the fan-out lives on the hub, not the session).
	events, unsub := ra.Subscribe(16)
	defer unsub()

	if err := ra.Start(fx.Simple); err != nil {
		t.Fatalf("pre-drop start: %v", err)
	}
	drainEvent(t, events, fx.Simple, agent.EventStarted)

	// Drop the agent; the same RemoteAgent must fail fast, then recover once
	// a new agent session (same host id) establishes.
	cancelFirst()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && hub.Connected("host-r") {
		time.Sleep(10 * time.Millisecond)
	}
	if hub.Connected("host-r") {
		t.Fatal("session did not end after agent cancel")
	}
	if err := ra.ForceStop(fx.Simple); err == nil {
		t.Fatal("expected not-connected error while the agent is down")
	}

	startAgentd(t, hub, dial, "host-r", testToken, pm)
	if err := ra.ForceStop(fx.Simple); err != nil {
		t.Fatalf("post-reconnect stop: %v", err)
	}
	drainEvent(t, events, fx.Simple, agent.EventStopped)
}

func TestDuplicateHostDisplacedNewestWins(t *testing.T) {
	pm, fx := newLocalPM(t)
	hub, dial := startHub(t, testToken)
	startAgentd(t, hub, dial, "host-d", testToken, pm)
	ra := hub.Agent("host-d")
	if in := ra.Get(fx.Simple); in == nil {
		t.Fatal("first session should serve")
	}

	// A second agent for the same host (restart scenario) displaces the
	// first; the host stays connected and serving throughout.
	startAgentd(t, hub, dial, "host-d", testToken, pm)
	waitConnected(t, hub, "host-d", 5*time.Second)
	deadline := time.Now().Add(5 * time.Second)
	var in *agent.ProcessInfo
	for time.Now().Before(deadline) {
		if in = ra.Get(fx.Simple); in != nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if in == nil {
		t.Fatal("displaced host must keep serving through the new session")
	}
}

func TestInstallArtifactMultiChunk(t *testing.T) {
	pm, fx := newLocalPM(t)
	hub, dial := startHub(t, testToken)
	startAgentd(t, hub, dial, "host-i", testToken, pm)
	ra := hub.Agent("host-i")

	// >1MB forces multiple 256KB chunks over FetchArtifact.
	content := make([]byte, 1<<20+12345)
	rand.Read(content)
	sum := sha256.Sum256(content)
	dest := filepath.Join(fx.Scratch, "big.jar")

	err := ra.InstallArtifact(agent.ArtifactSpec{
		DestPath: dest, Sha256: hex.EncodeToString(sum[:]), Mode: 0644,
	}, bytes.NewReader(content))
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	got, err := os.ReadFile(dest)
	if err != nil || !bytes.Equal(got, content) {
		t.Fatalf("artifact corrupted in transit: err=%v len=%d want=%d", err, len(got), len(content))
	}

	// Sha mismatch surfaces the agent-side verification failure.
	err = ra.InstallArtifact(agent.ArtifactSpec{
		DestPath: filepath.Join(fx.Scratch, "bad.jar"), Sha256: strings.Repeat("0", 64),
	}, bytes.NewReader(content))
	if err == nil || !strings.Contains(err.Error(), "sha256 mismatch") {
		t.Fatalf("expected agent-side sha failure, got %v", err)
	}
}

func TestNodeCountersRoundTripLatency(t *testing.T) {
	pm, _ := newLocalPM(t)
	hub, dial := startHub(t, testToken)
	startAgentd(t, hub, dial, "host-l", testToken, pm)
	ra := hub.Agent("host-l")

	// The rolling update polls counters at 500ms; a warm-stream round trip
	// must be far under that budget (doc risk #2). The call may succeed (a
	// dev box with a live cluster has a real CnC file) or fail (CI) — the
	// full round-trip latency is the assertion either way.
	start := time.Now()
	_, _ = ra.NodeCounters(0)
	if d := time.Since(start); d > 50*time.Millisecond {
		t.Fatalf("counters round trip took %s, budget 50ms", d)
	}
}

func drainEvent(t *testing.T, events <-chan agent.Event, service string, typ agent.EventType) {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		select {
		case ev, ok := <-events:
			if !ok {
				t.Fatal("event channel closed")
			}
			if ev.Service == service && ev.Type == typ {
				return
			}
		case <-deadline:
			t.Fatalf("no %s event for %s", typ, service)
		}
	}
}
