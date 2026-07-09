// SPDX-License-Identifier: Apache-2.0
package services

import (
	"fmt"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/match/admin-gateway/config"
)

// Stock Aeron tooling main classes — identical for every Aeron cluster.
const (
	clusterToolMain = "io.aeron.cluster.ClusterTool"
	archiveToolMain = "io.aeron.archive.ArchiveTool"
)

// Cluster is the descriptor + tooling wrapper for ONE Aeron cluster. Every
// per-cluster identity (node count, ports, dirs, jar, driver topology, tooling)
// lives here, so a single set of management operations (rolling update, restart,
// snapshot, status, counters, cleanup) serves both the matching engine and the
// assets engine — the only difference is which descriptor they run against.
//
// The media driver is modeled as PART OF the cluster: external clusters own a
// driver process per node (coupled to the node in ServiceDefs), embedded clusters
// run the driver inside the node JVM. Either way the driver's aeron.dir / CnC path
// is derived here (CncPath), so counters and housekeeping resolve it uniformly.
type Cluster struct {
	cfg *config.Config

	Name    string // stable id: "match" | "assets"
	Display string // human label
	Kind    string // UI discriminator: "match" | "assets" (icon/money-slot branch)

	NodeCount int    // 3 (match) | 1 (assets)
	PortBase  int    // 9000 | 9300; ingress = PortBase + node*100 + 2
	StateDir  string // cluster+archive state root: /dev/shm/aeron-cluster | /dev/shm/aeron-assets

	NodePrefix   string // node service + state-dir name prefix: "node" -> node0, "ae" -> ae0
	DriverPrefix string // external driver service prefix: "driver" -> driver0; "" when embedded

	NodeDisplay   string // node card label prefix: "Cluster Node" -> "Cluster Node 0"
	DriverDisplay string // driver card label prefix: "Media Driver" -> "Media Driver 0"

	// Embedded = the media driver runs inside the node JVM (no separate driver
	// service). External = one driver process per node, coupled in ServiceDefs.
	Embedded bool

	// DriverDirInfix distinguishes the media driver's aeron.dir between clusters
	// so their CnC files never collide: "" -> aeron-<user>-<n>-driver (match),
	// "assets-" -> aeron-<user>-assets-<n>-driver (assets embedded driver).
	DriverDirInfix string

	Jar string // launch/tooling classpath: match-cluster.jar | assets-cluster.jar

	// Source build inputs for a rolling update's jar rebuild (rsync + mvn):
	ProjectDir string // repo checkout: cfg.ProjectDir | cfg.AssetsProjectDir
	Module     string // maven module: "match-cluster" | "assets-cluster"

	// HousekeepingMain is the cluster's live archive-reclaim main class; "" skips
	// housekeeping (e.g. the assets engine has none yet).
	HousekeepingMain string

	// BackupDir is the disk backup location; "" = the cluster has no backup service.
	BackupDir string

	// RichArchiveStats gates the expensive per-poll enrichment (DetectLeader,
	// recording-log, archive `du` — each spawns a JVM/du). True for the matching
	// engine (its status has always carried positions/archive sizes); false for a
	// lean cluster on the shared box, whose status is CnC-counter-only (no JVM),
	// so the ME's busy-spin cores are never starved by a second cluster's polling.
	RichArchiveStats bool
}

// Capabilities reports what management operations this cluster supports, derived
// from its descriptor. The UI gates buttons on these (no Housekeeping/Backup for a
// cluster that has neither), and the handlers reject incapable ops before touching
// the shared operation slot.
func (c *Cluster) Capabilities() map[string]interface{} {
	return map[string]interface{}{
		"rollingUpdate":  true, // every Aeron cluster: swap jar + roll nodes
		"snapshot":       true, // every Aeron cluster: ClusterTool snapshot
		"cleanup":        true, // every Aeron cluster: stale /dev/shm reclaim
		"housekeeping":   c.HousekeepingMain != "",
		"backup":         c.BackupDir != "",
		"separateDriver": !c.Embedded,
	}
}

// NewMatchCluster is the matching-engine descriptor — exactly today's values, so
// migrating the ops onto it is behavior-preserving.
func NewMatchCluster(cfg *config.Config) *Cluster {
	return &Cluster{
		cfg:              cfg,
		Name:             "match",
		Display:          "Matching Engine",
		Kind:             "match",
		NodeCount:        3,
		PortBase:         9000,
		StateDir:         cfg.ClusterDir, // /dev/shm/aeron-cluster
		NodePrefix:       "node",
		DriverPrefix:     "driver",
		NodeDisplay:      "Cluster Node",
		DriverDisplay:    "Media Driver",
		Embedded:         false,
		DriverDirInfix:   "",
		Jar:              cfg.JarPath,
		ProjectDir:       cfg.ProjectDir,
		Module:           "match-cluster",
		HousekeepingMain: "com.match.infrastructure.persistence.ArchiveHousekeeping",
		BackupDir:        filepath.Join(cfg.ProjectDir, "backup"),
		RichArchiveStats: true, // the ME status has always carried positions/archive sizes
	}
}

// NewAssetsCluster is the money-ledger descriptor: a single embedded-driver node
// on disjoint ports/dirs. Node count and driver mode are just fields — flip to a
// 3-node external-driver cluster (full matching-engine parity) with no code change.
func NewAssetsCluster(cfg *config.Config) *Cluster {
	return &Cluster{
		cfg:              cfg,
		Name:             "assets",
		Display:          "Assets Engine",
		Kind:             "assets",
		NodeCount:        1,
		PortBase:         9300,
		StateDir:         "/dev/shm/aeron-assets",
		NodePrefix:       "ae",
		DriverPrefix:     "", // embedded: no external driver service
		NodeDisplay:      "Assets Engine",
		DriverDisplay:    "",
		Embedded:         true,
		DriverDirInfix:   "assets-",
		Jar:              cfg.AssetsJar,
		ProjectDir:       cfg.AssetsProjectDir,
		Module:           "assets-cluster",
		HousekeepingMain: "",    // no housekeeping yet
		BackupDir:        "",    // no backup yet
		RichArchiveStats: false, // CnC-counter-only status: no JVM/du on the shared box
	}
}

// NewCluster preserves the original single-cluster constructor (the matching
// engine) for callers not yet cluster-aware.
func NewCluster(cfg *config.Config) *Cluster {
	return NewMatchCluster(cfg)
}

// ---- per-cluster naming / paths (the seam every operation reads from) ----

// NodeName is the process-manager service name for node i (node0 / ae0).
func (c *Cluster) NodeName(i int) string { return fmt.Sprintf("%s%d", c.NodePrefix, i) }

// DriverName is the external driver service name for node i (driver0); empty when embedded.
func (c *Cluster) DriverName(i int) string {
	if c.Embedded {
		return ""
	}
	return fmt.Sprintf("%s%d", c.DriverPrefix, i)
}

// NodeStateDir is the cluster+archive state root for node i.
func (c *Cluster) NodeStateDir(i int) string {
	return fmt.Sprintf("%s/%s", c.StateDir, c.NodeName(i))
}

func (c *Cluster) clusterSubDir(i int) string { return c.NodeStateDir(i) + "/cluster" }
func (c *Cluster) archiveSubDir(i int) string { return c.NodeStateDir(i) + "/archive" }

// IngressPort is the client-facing port for node i (PortBase + node*100 + 2).
func (c *Cluster) IngressPort(i int) int { return c.PortBase + i*100 + 2 }

// DriverAeronDir is the aeron.dir of node i's media driver (external process or
// embedded) — the single source of truth for its CnC location.
func (c *Cluster) DriverAeronDir(i int) string {
	return fmt.Sprintf("/dev/shm/aeron-%s-%s%d-driver", currentUsername(), c.DriverDirInfix, i)
}

// CncPath is node i's Aeron CnC file (the counters source).
func (c *Cluster) CncPath(i int) string { return c.DriverAeronDir(i) + "/cnc.dat" }

// NodeServiceDefs generates this cluster's process-manager service definitions:
// its nodes and, in external-driver mode, one coupled media-driver process per
// node. The media driver is thus PART of the cluster — generated here with the
// driver↔node linkage (DependsOn / GatedBy / WaitForFile / RestartCascades /
// DriverDir) DERIVED from the descriptor, never hand-wired. The per-node launch
// specifics (which are profile-driven for the matching engine and fixed for the
// assets engine) are supplied by the caller's closures.
//
// external is passed explicitly because a cluster's driver topology can be
// profile-dependent (the matching engine runs external drivers on the demo/perf
// profiles but an embedded driver on the light profile), independent of the
// descriptor's static Embedded flag.
func (c *Cluster) NodeServiceDefs(
	external bool,
	nodeCmd func(i int) []string,
	driverCmd func(i int) []string,
	nodeEnv func(i int) map[string]string,
	preStart func(i int) [][]string,
) []ServiceDef {
	defs := make([]ServiceDef, 0, c.NodeCount*2)

	// Driver services first (boot order): each coupled to its node.
	if external {
		for i := 0; i < c.NodeCount; i++ {
			defs = append(defs, ServiceDef{
				Name:            c.DriverName(i),
				Display:         fmt.Sprintf("%s %d", c.DriverDisplay, i),
				Role:            RoleInfra,
				Command:         driverCmd(i),
				WorkDir:         c.ProjectDir,
				AutoRestart:     true,
				RestartSec:      3,
				StopTimeout:     5,
				RestartCascades: []string{c.NodeName(i)}, // a node cannot outlive its driver's shm files
				DriverDir:       c.DriverAeronDir(i),
			})
		}
	}

	// Node services, gated on their driver in external mode.
	for i := 0; i < c.NodeCount; i++ {
		def := ServiceDef{
			Name:        c.NodeName(i),
			Display:     fmt.Sprintf("%s %d", c.NodeDisplay, i),
			Role:        RoleClusterNode,
			Command:     nodeCmd(i),
			Env:         nodeEnv(i),
			WorkDir:     c.ProjectDir,
			PreStart:    preStart(i),
			AutoRestart: true,
			RestartSec:  10,
			StopTimeout: 5,
			Artifact:    c.Jar,
		}
		if external {
			def.DependsOn = []string{c.DriverName(i)}
			def.WaitForFile = filepath.Join(c.DriverAeronDir(i), "cnc.dat")
			def.GatedBy = c.DriverName(i)
		}
		defs = append(defs, def)
	}
	return defs
}

// ---- ClusterTool / ArchiveTool (stock Aeron; only clusterDir + jar differ) ----

// clusterTool runs a ClusterTool command for a specific node
func (c *Cluster) clusterTool(nodeId int, command string) (string, error) {
	cmd := exec.Command("java",
		"--add-opens", "java.base/jdk.internal.misc=ALL-UNNAMED",
		"-cp", c.Jar,
		clusterToolMain,
		c.clusterSubDir(nodeId), command)

	output, err := cmd.CombinedOutput()
	return string(output), err
}

// archiveTool runs an ArchiveTool command for a specific node
func (c *Cluster) archiveTool(nodeId int, command string) (string, error) {
	cmd := exec.Command("java",
		"--add-opens", "java.base/jdk.internal.misc=ALL-UNNAMED",
		"-cp", c.Jar,
		archiveToolMain,
		c.archiveSubDir(nodeId), command)

	output, err := cmd.CombinedOutput()
	return string(output), err
}

// DetectLeader finds the current cluster leader
func (c *Cluster) DetectLeader() int {
	for nodeId := 0; nodeId < c.NodeCount; nodeId++ {
		output, err := c.clusterTool(nodeId, "list-members")
		if err != nil {
			continue
		}
		// Parse leaderMemberId=N from output
		re := regexp.MustCompile(`leaderMemberId=(\d+)`)
		matches := re.FindStringSubmatch(output)
		if len(matches) > 1 {
			leader, _ := strconv.Atoi(matches[1])
			return leader
		}
	}
	return -1
}

// GetRecordingLog returns the recording log for a node
func (c *Cluster) GetRecordingLog(nodeId int) (string, error) {
	return c.clusterTool(nodeId, "recording-log")
}

// TakeSnapshot triggers a snapshot on the leader
func (c *Cluster) TakeSnapshot(leaderNode int) (string, error) {
	return c.clusterTool(leaderNode, "snapshot")
}

// GetLogAndSnapshotPositions returns both log position and snapshot position
// with a single JVM invocation (recording-log command).
func (c *Cluster) GetLogAndSnapshotPositions(nodeId int) (logPos int64, snapPos int64) {
	output, err := c.clusterTool(nodeId, "recording-log")
	if err != nil {
		return -1, -1
	}

	// Parse max log position across all entries
	logPos = -1
	reLog := regexp.MustCompile(`logPosition=(\d+)`)
	matches := reLog.FindAllStringSubmatch(output, -1)
	for _, match := range matches {
		if len(match) > 1 {
			pos, _ := strconv.ParseInt(match[1], 10, 64)
			if pos > logPos {
				logPos = pos
			}
		}
	}

	// Parse snapshot position from SNAPSHOT entries
	snapPos = -1
	entries := strings.Split(output, "Entry{")
	for _, entry := range entries {
		if strings.Contains(entry, "type=SNAPSHOT") {
			snapMatches := reLog.FindStringSubmatch(entry)
			if len(snapMatches) > 1 {
				pos, _ := strconv.ParseInt(snapMatches[1], 10, 64)
				if pos > snapPos {
					snapPos = pos
				}
			}
		}
	}

	return logPos, snapPos
}

func (c *Cluster) GetLogPosition(nodeId int) int64 {
	output, err := c.clusterTool(nodeId, "recording-log")
	if err != nil {
		return -1
	}

	re := regexp.MustCompile(`logPosition=(\d+)`)
	matches := re.FindAllStringSubmatch(output, -1)

	var maxPos int64 = -1
	for _, match := range matches {
		if len(match) > 1 {
			pos, _ := strconv.ParseInt(match[1], 10, 64)
			if pos > maxPos {
				maxPos = pos
			}
		}
	}
	return maxPos
}

// GetSnapshotPosition extracts the latest snapshot position
func (c *Cluster) GetSnapshotPosition(nodeId int) int64 {
	output, err := c.clusterTool(nodeId, "recording-log")
	if err != nil {
		return -1
	}

	// Find SNAPSHOT entries and get their logPosition
	var latestPos int64 = -1
	entries := strings.Split(output, "Entry{")
	for _, entry := range entries {
		if strings.Contains(entry, "type=SNAPSHOT") {
			re := regexp.MustCompile(`logPosition=(\d+)`)
			matches := re.FindStringSubmatch(entry)
			if len(matches) > 1 {
				pos, _ := strconv.ParseInt(matches[1], 10, 64)
				if pos > latestPos {
					latestPos = pos
				}
			}
		}
	}
	return latestPos
}

// GetArchiveSize returns the archive size in bytes for a node
func (c *Cluster) GetArchiveSize(nodeId int) int64 {
	cmd := exec.Command("du", "-sb", "--apparent-size", c.NodeStateDir(nodeId))
	output, err := cmd.Output()
	if err != nil {
		return -1
	}
	parts := strings.Fields(string(output))
	if len(parts) > 0 {
		size, _ := strconv.ParseInt(parts[0], 10, 64)
		return size
	}
	return -1
}

// GetArchiveDiskUsage returns actual disk usage in bytes
func (c *Cluster) GetArchiveDiskUsage(nodeId int) int64 {
	cmd := exec.Command("du", "-s", c.NodeStateDir(nodeId))
	output, err := cmd.Output()
	if err != nil {
		return -1
	}
	parts := strings.Fields(string(output))
	if len(parts) > 0 {
		// du -s returns KB, convert to bytes
		sizeKB, _ := strconv.ParseInt(parts[0], 10, 64)
		return sizeKB * 1024
	}
	return -1
}

// SeedRecordingLogFromSnapshot resets a node's recording-log from its latest snapshot
func (c *Cluster) SeedRecordingLogFromSnapshot(nodeId int) (string, error) {
	return c.clusterTool(nodeId, "seed-recording-log-from-snapshot")
}

// ArchiveHousekeeping reclaims archive disk on a LIVE node (no downtime):
// it purges whole log segment files below the latest snapshot position. This
// is the ONLY safe in-place reclaimer — it never runs Aeron's offline
// ArchiveTool against a running node (doing so corrupts snapshot recordings
// and breaks recover-from-snapshot). Invoked automatically after every snapshot.
//
// Clusters without a housekeeping main class (HousekeepingMain == "") skip it.
func (c *Cluster) ArchiveHousekeeping(nodeId int) (string, error) {
	if c.HousekeepingMain == "" {
		return fmt.Sprintf("housekeeping skipped: %s has no housekeeping tool", c.Name), nil
	}
	cmd := exec.Command("java",
		"--add-opens", "java.base/jdk.internal.misc=ALL-UNNAMED",
		"-cp", c.Jar,
		c.HousekeepingMain,
		c.clusterSubDir(nodeId), c.DriverAeronDir(nodeId), "2")

	output, err := cmd.CombinedOutput()
	return string(output), err
}

// currentUsername returns the OS user, matching Aeron's default
// CommonContext directory naming (/dev/shm/aeron-<user>-<node>-driver).
func currentUsername() string {
	if u, err := user.Current(); err == nil {
		return u.Username
	}
	return "unknown"
}

// driverDirPath is the single source of truth for an EXTERNAL matching-engine
// media driver's aeron.dir. Every reader and (guarded) deleter of these dirs must
// derive the path here so the #42 protections cannot diverge from the launch
// config. (Equivalent to NewMatchCluster(cfg).DriverAeronDir(nodeId).)
func driverDirPath(nodeId int) string {
	return fmt.Sprintf("/dev/shm/aeron-%s-%d-driver", currentUsername(), nodeId)
}

// ArchiveToolDescribe describes the archive catalog for a node
func (c *Cluster) ArchiveToolDescribe(nodeId int) (string, error) {
	cmd := exec.Command("java",
		"--add-opens", "java.base/jdk.internal.misc=ALL-UNNAMED",
		"-cp", c.Jar,
		archiveToolMain,
		c.archiveSubDir(nodeId), "describe")
	output, err := cmd.CombinedOutput()
	return string(output), err
}

// GetRecordingLogRecordingIds parses recording IDs from the recording-log
func (c *Cluster) GetRecordingLogRecordingIds(nodeId int) []int64 {
	output, err := c.clusterTool(nodeId, "recording-log")
	if err != nil {
		return nil
	}
	return extractRecordingIds(output)
}

// GetArchiveCatalogRecordingIds parses recording IDs from the archive catalog
func (c *Cluster) GetArchiveCatalogRecordingIds(nodeId int) []int64 {
	output, err := c.ArchiveToolDescribe(nodeId)
	if err != nil {
		return nil
	}
	return extractRecordingIds(output)
}

// extractRecordingIds parses recordingId=N from tool output
func extractRecordingIds(output string) []int64 {
	re := regexp.MustCompile(`recordingId=(\d+)`)
	matches := re.FindAllStringSubmatch(output, -1)
	ids := make([]int64, 0, len(matches))
	for _, match := range matches {
		if len(match) > 1 {
			id, _ := strconv.ParseInt(match[1], 10, 64)
			ids = append(ids, id)
		}
	}
	return ids
}
