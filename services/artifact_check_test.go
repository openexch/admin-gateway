// SPDX-License-Identifier: Apache-2.0
package services

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/match/admin-gateway/config"
	"github.com/match/admin-gateway/logging"
)

func TestStartRefusedWhenArtifactMissing(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "oms-app.jar")
	def := ServiceDef{Name: "oms", Artifact: missing, Command: []string{"/bin/true"}}
	proc := &managedProcess{status: "stopped"}
	pm := &ProcessManager{
		log:      logging.Component("pm"),
		procs:    map[string]*managedProcess{"oms": proc},
		services: []ServiceDef{def},
	}

	err := pm.startProcessInner(def, true)
	if err == nil {
		t.Fatal("expected refusal with the artifact missing")
	}
	if !strings.Contains(err.Error(), "artifact missing") || !strings.Contains(err.Error(), missing) {
		t.Fatalf("refusal should name the missing artifact, got: %v", err)
	}
	proc.mu.Lock()
	defer proc.mu.Unlock()
	if proc.status != "failed" || !strings.Contains(proc.lastError, "artifact missing") {
		t.Fatalf("refusal must land in status/lastError, got status=%q lastError=%q", proc.status, proc.lastError)
	}
	// The whole point: a refused start is not a crash, so it must not burn
	// the crash-loop cap toward disarm.
	if len(proc.crashTimes) != 0 {
		t.Fatal("a refused start must not count toward the crash-loop cap")
	}
}

func TestStartProceedsWhenArtifactPresent(t *testing.T) {
	dir := t.TempDir()
	artifact := filepath.Join(dir, "present.jar")
	if err := os.WriteFile(artifact, []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	def := ServiceDef{Name: "oms", Artifact: artifact, Command: []string{"/bin/true"}}
	proc := &managedProcess{status: "stopped"}
	pm := &ProcessManager{
		log:      logging.Component("pm"),
		logDir:   dir,
		pidDir:   dir,
		stopChan: make(chan struct{}),
		procs:    map[string]*managedProcess{"oms": proc},
		services: []ServiceDef{def},
	}

	if err := pm.startProcessInner(def, true); err != nil {
		t.Fatalf("start should proceed past the artifact check, got %v", err)
	}
	// /bin/true exits immediately; stop bookkeeping so the monitor goroutine
	// treats it as intentional.
	pm.stopProcess("oms", true)
}

func TestBuildCmdNice(t *testing.T) {
	// Enabled: argv is prefixed with nice (and ionice where available) and
	// preserves the original command tail + dir.
	o := &OperationsService{cfg: &config.Config{BuildNice: 10}, log: logging.Component("ops")}
	cmd := o.buildCmd("/tmp", "mvn", "package", "-q")
	joined := strings.Join(cmd.Args, " ")
	if !strings.Contains(joined, "nice -n 10") {
		t.Fatalf("expected nice prefix, got %v", cmd.Args)
	}
	if !strings.HasSuffix(joined, "mvn package -q") {
		t.Fatalf("original argv must be preserved, got %v", cmd.Args)
	}
	if cmd.Dir != "/tmp" {
		t.Fatalf("dir not applied, got %q", cmd.Dir)
	}

	// Disabled (0): the command runs unwrapped.
	o.cfg.BuildNice = 0
	cmd = o.buildCmd("", "go", "build", ".")
	if cmd.Args[0] != "go" {
		t.Fatalf("BuildNice=0 must not wrap, got %v", cmd.Args)
	}
}
