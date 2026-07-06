// SPDX-License-Identifier: Apache-2.0
package services

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/match/admin-gateway/config"
	"github.com/match/admin-gateway/logging"
)

// recordedCmd is one execBuild invocation captured by newBuildRecorder.
type recordedCmd struct {
	dir  string
	argv []string
}

// newBuildRecorder returns an execBuild stub that records every command and,
// when it sees the mvn step, fabricates the built jar inside the temp tree so
// the staging copy has something real to move.
func newBuildRecorder(rec *[]recordedCmd, builtJarRel string, jarContent string) func(string, ...string) ([]byte, error) {
	return func(dir string, argv ...string) ([]byte, error) {
		*rec = append(*rec, recordedCmd{dir: dir, argv: argv})
		if argv[0] == "mvn" {
			jar := filepath.Join(dir, builtJarRel)
			if err := os.MkdirAll(filepath.Dir(jar), 0o755); err != nil {
				return nil, err
			}
			if err := os.WriteFile(jar, []byte(jarContent), 0o644); err != nil {
				return nil, err
			}
		}
		return nil, nil
	}
}

func newRebuildFixture(t *testing.T) (*OperationsService, *fakeAgent, *[]recordedCmd) {
	t.Helper()
	liveTree := filepath.Join(t.TempDir(), "match")
	if err := os.MkdirAll(liveTree, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		ProjectDir: liveTree,
		GatewayJar: filepath.Join(liveTree, "match-gateway/target/match-gateway.jar"),
		JarPath:    filepath.Join(liveTree, "match-cluster/target/match-cluster.jar"),
	}
	fa := newFakeAgent()
	rec := &[]recordedCmd{}
	o := &OperationsService{
		cfg:      cfg,
		progress: NewProgress(),
		log:      logging.Component("ops"),
	}
	o.SetProcessManager(fa)
	o.execBuild = newBuildRecorder(rec, "match-gateway/target/match-gateway.jar", "new-gateway-jar")
	return o, fa, rec
}

// TestRebuildGatewayNeverBuildsInLiveTree is the #45 regression test: the mvn
// step (whose clean/-am phases delete upstream modules' target dirs) must only
// ever execute inside the isolated temp tree, and the live jar must be
// replaced through the sha-verified InstallArtifact path.
func TestRebuildGatewayNeverBuildsInLiveTree(t *testing.T) {
	o, fa, rec := newRebuildFixture(t)
	if !o.progress.TryStart("rebuild-gateway", 3) {
		t.Fatal("could not claim progress")
	}

	o.doRebuildGateway(true)

	pm := o.progress.ToMap()
	if pm["error"] != false || pm["complete"] != true {
		t.Fatalf("rebuild should finish clean, got %+v", pm)
	}

	var sawMvn bool
	for _, c := range *rec {
		if c.dir == o.cfg.ProjectDir {
			t.Fatalf("build step executed in the LIVE tree (%s): %v — the #45 outage vector", c.dir, c.argv)
		}
		if c.argv[0] == "mvn" {
			sawMvn = true
			if !strings.Contains(c.dir, "match-gateway-build-") {
				t.Fatalf("mvn ran outside the isolated build dir: %q", c.dir)
			}
			joined := strings.Join(c.argv, " ")
			if !strings.Contains(joined, "-pl match-gateway") {
				t.Fatalf("mvn must be module-scoped, got %q", joined)
			}
		}
		if c.argv[0] == "rsync" {
			joined := strings.Join(c.argv, " ")
			for _, ex := range []string{"--exclude=*/target", "--exclude=.git"} {
				if !strings.Contains(joined, ex) {
					t.Fatalf("rsync must exclude %q, got %q", ex, joined)
				}
			}
			if !strings.Contains(joined, o.cfg.ProjectDir+"/") {
				t.Fatalf("rsync should copy from the live tree, got %q", joined)
			}
		}
	}
	if !sawMvn {
		t.Fatal("expected an mvn build step")
	}

	// The live jar is replaced only via the sha-verified artifact primitive.
	if len(fa.installed) != 1 {
		t.Fatalf("expected exactly one InstallArtifact, got %d", len(fa.installed))
	}
	spec := fa.installed[0]
	if spec.DestPath != o.cfg.GatewayJar {
		t.Fatalf("installed to %q, want %q", spec.DestPath, o.cfg.GatewayJar)
	}
	if len(spec.Sha256) != 64 {
		t.Fatalf("install must carry a sha256, got %q", spec.Sha256)
	}

	// Restart list honesty: market only — oms runs a different repo's jar.
	if fmt.Sprintf("%v", fa.restarts) != "[market]" {
		t.Fatalf("restart list must be exactly [market], got %v", fa.restarts)
	}
}

func TestRebuildGatewayNoRestartWhenNotAsked(t *testing.T) {
	o, fa, _ := newRebuildFixture(t)
	if !o.progress.TryStart("rebuild-gateway", 2) {
		t.Fatal("could not claim progress")
	}

	o.doRebuildGateway(false)

	if pm := o.progress.ToMap(); pm["error"] != false || pm["complete"] != true {
		t.Fatalf("rebuild should finish clean, got %+v", pm)
	}
	if len(fa.restarts) != 0 {
		t.Fatalf("no restarts expected, got %v", fa.restarts)
	}
	if len(fa.installed) != 1 {
		t.Fatalf("jar should still be installed, got %d installs", len(fa.installed))
	}
}

func TestStageModuleJarCleansTempTree(t *testing.T) {
	o, _, _ := newRebuildFixture(t)
	temp := filepath.Join(t.TempDir(), "build")
	staging := filepath.Join(t.TempDir(), "staging/match-gateway.jar")

	err := o.stageModuleJar(o.cfg.ProjectDir, "match-gateway",
		"match-gateway/target/match-gateway.jar", temp, staging)
	if err != nil {
		t.Fatalf("stage failed: %v", err)
	}
	if _, err := os.Stat(staging); err != nil {
		t.Fatalf("staged jar missing: %v", err)
	}
	if b, err := os.ReadFile(staging); err != nil || string(b) != "new-gateway-jar" {
		t.Fatalf("staged jar content wrong: %q %v", b, err)
	}
	if _, err := os.Stat(temp); !os.IsNotExist(err) {
		t.Fatal("temp build tree should be removed on success")
	}
}

func TestStageModuleJarPropagatesBuildFailure(t *testing.T) {
	o, _, _ := newRebuildFixture(t)
	o.execBuild = func(dir string, argv ...string) ([]byte, error) {
		if argv[0] == "mvn" {
			return []byte("BUILD FAILURE"), fmt.Errorf("exit 1")
		}
		return nil, nil
	}
	temp := filepath.Join(t.TempDir(), "build")
	staging := filepath.Join(t.TempDir(), "staging/x.jar")

	err := o.stageModuleJar(o.cfg.ProjectDir, "match-gateway", "x.jar", temp, staging)
	if err == nil || !strings.Contains(err.Error(), "BUILD FAILURE") {
		t.Fatalf("expected mvn failure with output, got %v", err)
	}
	if _, err := os.Stat(staging); !os.IsNotExist(err) {
		t.Fatal("no staging jar should exist after a failed build")
	}
	if _, err := os.Stat(temp); !os.IsNotExist(err) {
		t.Fatal("temp build tree should be removed on failure too")
	}
}
