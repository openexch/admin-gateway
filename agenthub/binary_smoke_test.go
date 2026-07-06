// SPDX-License-Identifier: Apache-2.0

//go:build loopback

// End-to-end binary smoke (make loopback / CI loopback step): the REAL
// cmd/agentd binary over a REAL 127.0.0.1 TCP listener — no bufconn, no
// in-process shortcuts. The in-process loopback tests prove the protocol;
// this proves the shipped binary wires it all together.
package agenthub_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/grpc"

	"github.com/match/admin-gateway/agent"
	"github.com/match/admin-gateway/agenthub"
	"github.com/match/admin-gateway/agentwire"
)

func TestAgentdBinarySmoke(t *testing.T) {
	dir := t.TempDir()

	// Build the real binary.
	bin := filepath.Join(dir, "agentd")
	build := exec.Command("go", "build", "-o", bin, "../cmd/agentd")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build agentd: %v: %s", err, out)
	}

	// Real TCP hub on an ephemeral loopback port.
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	hub := agenthub.NewHub("smoke-token")
	srv := grpc.NewServer(hub.ServerOptions()...)
	agentwire.RegisterControlPlaneServer(srv, hub)
	go srv.Serve(lis)
	defer srv.Stop()

	tokenFile := filepath.Join(dir, "token")
	if err := os.WriteFile(tokenFile, []byte("smoke-token\n"), 0600); err != nil {
		t.Fatal(err)
	}
	logDir := filepath.Join(dir, "logs")
	scratch := filepath.Join(dir, "scratch")
	os.MkdirAll(scratch, 0755)

	cmd := exec.Command(bin,
		"-control", lis.Addr().String(),
		"-host-id", "smoke-host",
		"-token-file", tokenFile,
		"-insecure",
		"-log-dir", logDir,
		"-pid-dir", filepath.Join(dir, "pids"),
	)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		cmd.Process.Signal(os.Interrupt)
		cmd.Wait()
	}()

	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) && !hub.Connected("smoke-host") {
		time.Sleep(50 * time.Millisecond)
	}
	if !hub.Connected("smoke-host") {
		t.Fatal("binary did not register with the hub")
	}
	ra := hub.Agent("smoke-host")

	// Empty catalog: List is empty, unknown services error.
	if infos := ra.List(); len(infos) != 0 {
		t.Fatalf("M3 agentd must manage nothing, got %v", infos)
	}
	if err := ra.Start("anything"); err == nil {
		t.Fatal("unknown service must error")
	}

	// Catalog-independent verbs work end to end.
	if err := os.WriteFile(filepath.Join(logDir, "probe.log"), []byte("a\nb\nc\n"), 0644); err != nil {
		t.Fatal(err)
	}
	lines, err := ra.TailLog("probe", 2)
	if err != nil || len(lines) != 2 || lines[1] != "c" {
		t.Fatalf("taillog through the binary: %v %v", lines, err)
	}

	content := []byte("smoke-artifact")
	sum := sha256.Sum256(content)
	dest := filepath.Join(scratch, "smoke.jar")
	if err := ra.InstallArtifact(agent.ArtifactSpec{
		DestPath: dest, Sha256: hex.EncodeToString(sum[:]),
	}, bytes.NewReader(content)); err != nil {
		t.Fatalf("install through the binary: %v", err)
	}
	if got, _ := os.ReadFile(dest); !bytes.Equal(got, content) {
		t.Fatalf("artifact content wrong: %q", got)
	}
}
