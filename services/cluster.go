// SPDX-License-Identifier: Apache-2.0
package services

import (
	"fmt"
	"os/exec"
	"os/user"
	"regexp"
	"strconv"
	"strings"

	"github.com/match/admin-gateway/config"
)

// Cluster wraps ClusterTool operations
type Cluster struct {
	cfg *config.Config
}

func NewCluster(cfg *config.Config) *Cluster {
	return &Cluster{cfg: cfg}
}

// clusterTool runs a ClusterTool command for a specific node
func (c *Cluster) clusterTool(nodeId int, command string) (string, error) {
	clusterDir := fmt.Sprintf("%s/node%d/cluster", c.cfg.ClusterDir, nodeId)
	cmd := exec.Command("java",
		"--add-opens", "java.base/jdk.internal.misc=ALL-UNNAMED",
		"-cp", c.cfg.JarPath,
		"io.aeron.cluster.ClusterTool",
		clusterDir, command)

	output, err := cmd.CombinedOutput()
	return string(output), err
}

// archiveTool runs an ArchiveTool command for a specific node
func (c *Cluster) archiveTool(nodeId int, command string) (string, error) {
	archiveDir := fmt.Sprintf("%s/node%d/archive", c.cfg.ClusterDir, nodeId)
	cmd := exec.Command("java",
		"--add-opens", "java.base/jdk.internal.misc=ALL-UNNAMED",
		"-cp", c.cfg.JarPath,
		"io.aeron.archive.ArchiveTool",
		archiveDir, command)

	output, err := cmd.CombinedOutput()
	return string(output), err
}

// DetectLeader finds the current cluster leader
func (c *Cluster) DetectLeader() int {
	for nodeId := 0; nodeId < 3; nodeId++ {
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
	nodeDir := fmt.Sprintf("%s/node%d", c.cfg.ClusterDir, nodeId)
	cmd := exec.Command("du", "-sb", "--apparent-size", nodeDir)
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
	nodeDir := fmt.Sprintf("%s/node%d", c.cfg.ClusterDir, nodeId)
	cmd := exec.Command("du", "-s", nodeDir)
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
func (c *Cluster) ArchiveHousekeeping(nodeId int) (string, error) {
	clusterDir := fmt.Sprintf("%s/node%d/cluster", c.cfg.ClusterDir, nodeId)
	aeronDir := driverDirPath(nodeId)
	cmd := exec.Command("java",
		"--add-opens", "java.base/jdk.internal.misc=ALL-UNNAMED",
		"-cp", c.cfg.JarPath,
		"com.match.infrastructure.persistence.ArchiveHousekeeping",
		clusterDir, aeronDir, "2")

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

// driverDirPath is the single source of truth for an external media driver's
// aeron.dir. Every reader and (guarded) deleter of these dirs must derive the
// path here so the #42 protections cannot diverge from the launch config.
func driverDirPath(nodeId int) string {
	return fmt.Sprintf("/dev/shm/aeron-%s-%d-driver", currentUsername(), nodeId)
}

// ArchiveToolDescribe describes the archive catalog for a node
func (c *Cluster) ArchiveToolDescribe(nodeId int) (string, error) {
	archiveDir := fmt.Sprintf("%s/node%d/archive", c.cfg.ClusterDir, nodeId)
	cmd := exec.Command("java",
		"--add-opens", "java.base/jdk.internal.misc=ALL-UNNAMED",
		"-cp", c.cfg.JarPath,
		"io.aeron.archive.ArchiveTool",
		archiveDir, "describe")
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
