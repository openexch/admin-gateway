// SPDX-License-Identifier: Apache-2.0
package services

import (
	"fmt"
	"path/filepath"
	"time"
)

// RebuildOms builds the OMS uber-jar in an isolated copy of the OMS repo and
// installs it over the live jar. Until now the live oms-app.jar was produced
// by a MANUAL copy after a hand-run mvn build — the one flow rebuild-gateway
// pretended to cover by restarting oms without ever building its code.
func (o *OperationsService) RebuildOms(restart, force bool) error {
	totalSteps := 2
	if restart {
		totalSteps = 3
	}
	if !o.progress.TryStart("rebuild-oms", totalSteps) {
		return fmt.Errorf("another operation in progress")
	}
	if err := o.gate("rebuild-oms", force); err != nil {
		return err
	}

	go o.doRebuildOms(restart)
	return nil
}

func (o *OperationsService) doRebuildOms(restart bool) {
	stagingJar := filepath.Join(o.cfg.OmsProjectDir, "oms-app/target/staging/oms-app.jar")

	// Step 1: isolated-tree build (#45 pattern — mvn never runs in the live
	// tree). The shaded runnable jar is oms-app-1.0-SNAPSHOT.jar; the live
	// launch path is the version-free oms-app.jar. NOTE: the build resolves
	// match-common from ~/.m2 (order-management/pom.xml) — a box that never
	// built the match repo must `mvn install` there once (runbook 8).
	o.progress.Update(1, "Building oms-app in isolated directory...")
	tempBuildDir := fmt.Sprintf("/tmp/oms-build-%d", time.Now().UnixMilli())
	if err := o.stageModuleJar(o.cfg.OmsProjectDir, "oms-app",
		"oms-app/target/oms-app-1.0-SNAPSHOT.jar", tempBuildDir, stagingJar); err != nil {
		o.progress.Finish(false, "OMS build failed: "+err.Error())
		return
	}

	// Step 2: install over the live jar (sha-verified, atomic).
	o.progress.Update(2, "Installing OMS JAR...")
	if err := o.installStagedJar(stagingJar, o.cfg.OmsJar); err != nil {
		o.progress.Finish(false, "OMS JAR install failed: "+err.Error())
		return
	}

	if !restart {
		o.progress.Finish(true, "OMS JAR rebuilt and installed")
		return
	}

	// Step 3: restart onto the new code.
	o.progress.Update(3, "Restarting OMS...")
	o.restartService("oms")
	time.Sleep(3 * time.Second)

	o.progress.Finish(true, "OMS rebuilt, installed and restarted")
}
