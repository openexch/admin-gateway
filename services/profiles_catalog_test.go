// SPDX-License-Identifier: Apache-2.0
package services

import (
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/match/admin-gateway/config"
)

// The catalog must reflect the active profile: heaps, pre-touch, pinning, idle
// strategy, book capacity, log term, driver mode, and sim load all trace from
// the Profile struct into each service's Command/Env.
func TestCatalogReflectsProfile(t *testing.T) {
	profiles, err := config.LoadProfiles()
	if err != nil {
		t.Fatalf("LoadProfiles: %v", err)
	}
	if len(profiles) < 5 {
		t.Fatalf("expected the 5 shipped profiles, got %d", len(profiles))
	}

	for name, prof := range profiles {
		t.Run(name, func(t *testing.T) {
			cfg := &config.Config{
				ProjectDir: "/tmp/proj", OmsJar: "/tmp/oms.jar",
				SimBinary: "/tmp/market-sim", JarPath: "/tmp/cluster.jar",
				ProfileName: name, Profile: prof, Profiles: profiles,
			}
			pm := NewProcessManager(cfg)

			node := pm.findDef("node0")
			if node == nil {
				t.Fatal("node0 missing from catalog")
			}
			// Heap + pre-touch.
			mustContain(t, node.Command, fmt.Sprintf("-Xmx%dm", prof.NodeHeapMB))
			mustContain(t, node.Command, fmt.Sprintf("-Xms%dm", prof.NodeHeapMB))
			if got := hasArg(node.Command, "-XX:+AlwaysPreTouch"); got != prof.PreTouch {
				t.Errorf("AlwaysPreTouch = %v, want %v", got, prof.PreTouch)
			}
			// Pinning.
			switch prof.Pinning {
			case "dedicated":
				if node.Command[0] != "/usr/bin/taskset" {
					t.Errorf("dedicated pinning: command[0] = %q, want taskset", node.Command[0])
				}
			case "none":
				if node.Command[0] != "/usr/bin/java" {
					t.Errorf("none pinning: command[0] = %q, want java (no taskset)", node.Command[0])
				}
			}
			// Engine/transport env.
			if node.Env["TRANSPORT_IDLE_MODE"] != prof.IdleMode {
				t.Errorf("TRANSPORT_IDLE_MODE = %q, want %q", node.Env["TRANSPORT_IDLE_MODE"], prof.IdleMode)
			}
			if node.Env["TRANSPORT_LOG_TERM_LENGTH"] != prof.LogTermLength {
				t.Errorf("TRANSPORT_LOG_TERM_LENGTH = %q, want %q", node.Env["TRANSPORT_LOG_TERM_LENGTH"], prof.LogTermLength)
			}
			if node.Env["MATCH_ENGINE_BOOK_CAPACITY"] != strconv.Itoa(prof.BookCapacity) {
				t.Errorf("MATCH_ENGINE_BOOK_CAPACITY = %q, want %d", node.Env["MATCH_ENGINE_BOOK_CAPACITY"], prof.BookCapacity)
			}

			// Driver mode: external => driver0 present + node uses external transport.
			driver := pm.findDef("driver0")
			if prof.DriverMode == "external" {
				if driver == nil {
					t.Error("external driver mode: driver0 missing")
				}
				if node.Env["TRANSPORT_DRIVER_MODE"] != "external" {
					t.Errorf("external: node TRANSPORT_DRIVER_MODE = %q, want external", node.Env["TRANSPORT_DRIVER_MODE"])
				}
			} else if driver != nil {
				t.Error("embedded driver mode: driver0 should not exist")
			}

			// Sim load.
			sim := pm.findDef("sim")
			if sim == nil {
				t.Fatal("sim missing from catalog")
			}
			mustContain(t, sim.Command, fmt.Sprintf("-global-ops=%d", prof.SimGlobalOps))

			// OMS/market heaps.
			mustContain(t, pm.findDef("oms").Command, fmt.Sprintf("-Xmx%dm", prof.OmsHeapMB))
			mustContain(t, pm.findDef("market").Command, fmt.Sprintf("-Xmx%dm", prof.MarketHeapMB))
		})
	}
}

func hasArg(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}

func mustContain(t *testing.T, args []string, want string) {
	t.Helper()
	if !hasArg(args, want) {
		t.Errorf("command missing %q; got: %s", want, strings.Join(args, " "))
	}
}
