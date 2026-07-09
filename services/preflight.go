// SPDX-License-Identifier: Apache-2.0
package services

import (
	"fmt"
	"log/slog"
	"os"
	"strings"
	"syscall"

	"github.com/match/admin-gateway/agent"
	"github.com/match/admin-gateway/config"
	"github.com/match/admin-gateway/logging"
)

// Pre-flight invariant engine (admin-gateway#42/#43/#45): named checks over the
// conditions that let an admin operation take the cluster down on a
// resource-constrained box. Consumed three ways:
//
//  1. destructive operations call Gate(op, force) before doing work — a
//     blocking failure refuses the operation unless force overrides it;
//  2. the status poller runs RunCheap every cycle and surfaces failures in
//     /api/admin/status and the admin_invariant_ok metric;
//  3. GET /api/admin/preflight runs everything on demand (a report, never a gate).
//
// Checks here observe; they never mutate. Enforcement that must survive a
// control-plane outage (the pre-start artifact check, the live-driver-dir
// guards) lives agent-side in the ProcessManager, not here — the agent cannot
// depend on control-plane services (docs/AGENT-ARCHITECTURE.md).

// Severity of a failed invariant: block refuses gated operations, warn only logs.
const (
	SeverityBlock = "block"
	SeverityWarn  = "warn"
)

// InvariantResult is one check's verdict. Severity is meaningful when !OK and
// names the consequence for gated operations.
type InvariantResult struct {
	Name     string `json:"name"`
	OK       bool   `json:"ok"`
	Severity string `json:"severity,omitempty"`
	Detail   string `json:"detail,omitempty"`
}

// opGates maps a gated operation to the checks that must pass before it runs.
// Single-service /processes/* ops are deliberately absent: those are the
// recovery tools reached for exactly when invariants are failing, and gating
// them would create lockout loops. Snapshot/housekeeping keep their dedicated
// archive-lag guard (archiveOpBlockReason, match#35).
var opGates = map[string][]string{
	"rolling-update":  {"mem-available", "cluster-quorum", "driver-dirs", "disk-space"},
	"rebuild-cluster": {"mem-available", "disk-space"},
	"rebuild-gateway": {"mem-available", "disk-space"},
	"rebuild-oms":     {"mem-available", "disk-space"},
	// A profile switch rolls the nodes one at a time exactly like a rolling
	// update, so it carries the same quorum hazard: starting from a degraded
	// cluster can drop it below quorum when the first (healthy) node is stopped.
	// No jar is built, so disk-space is irrelevant. force overrides.
	"apply-profile": {"mem-available", "cluster-quorum", "driver-dirs"},
}

// statusReader is the slice of StatusService the quorum check needs.
type statusReader interface {
	GetStatus() map[string]interface{}
}

type Preflight struct {
	cfg    *config.Config
	pm     agent.ProcessAgent
	status statusReader
	log    *slog.Logger

	// Injectable for tests (the driver-dir derivation especially: tests must
	// never stat, create, or judge the box's real /dev/shm driver dirs).
	meminfoPath string
	rootPath    string
	shmPath     string
	driverDir   func(nodeId int) string
}

func NewPreflight(cfg *config.Config) *Preflight {
	return &Preflight{
		cfg:         cfg,
		log:         logging.Component("preflight"),
		meminfoPath: "/proc/meminfo",
		rootPath:    "/",
		shmPath:     "/dev/shm",
		driverDir:   driverDirPath,
	}
}

// SetProcessManager injects the process agent (avoids circular init).
func (p *Preflight) SetProcessManager(pm agent.ProcessAgent) {
	p.pm = pm
}

// SetStatusService injects the status service for the cluster-quorum check.
func (p *Preflight) SetStatusService(s *StatusService) {
	p.status = s
}

// RunAll runs every check, including ones that read the cluster status cache.
func (p *Preflight) RunAll() []InvariantResult {
	return append(p.RunCheap(), p.checkQuorum())
}

// RunCheap runs only the checks that touch local files and the in-process
// agent — no JVM spawns, no HTTP probes, no status-cache reads — so the 2s
// status poller can call it every cycle (and so it never recurses into the
// status cache it is embedded in).
func (p *Preflight) RunCheap() []InvariantResult {
	return []InvariantResult{
		p.checkMemAvailable(),
		p.checkDiskSpace(),
		p.checkDriverDirs(),
		p.checkArtifacts(),
	}
}

// Gate refuses a gated operation when any of its checks fails at block
// severity. force overrides blocking failures (they are still logged), the
// same escape hatch as the match#35 archive-lag guard.
func (p *Preflight) Gate(op string, force bool) error {
	names, ok := opGates[op]
	if !ok {
		return nil
	}
	byName := make(map[string]InvariantResult)
	for _, r := range p.RunAll() {
		byName[r.Name] = r
	}
	var blocked []string
	for _, name := range names {
		r, ok := byName[name]
		if !ok || r.OK {
			continue
		}
		if r.Severity == SeverityBlock {
			blocked = append(blocked, fmt.Sprintf("%s: %s", r.Name, r.Detail))
		} else {
			p.log.Warn("preflight warning", "op", op, "check", r.Name, "detail", r.Detail)
		}
	}
	if len(blocked) == 0 {
		return nil
	}
	if force {
		p.log.Warn("preflight blocking failures overridden by force",
			"op", op, "failures", strings.Join(blocked, "; "))
		return nil
	}
	return fmt.Errorf("preflight refused %s — %s (inspect GET /api/admin/preflight; override with {\"force\":true})",
		op, strings.Join(blocked, "; "))
}

// InvariantsOK is true when every result passed.
func InvariantsOK(rs []InvariantResult) bool {
	for _, r := range rs {
		if !r.OK {
			return false
		}
	}
	return true
}

// ---- mem-available (#43) ----

// checkMemAvailable guards against the OOM class of incident: a node
// restart's catchup transient on a box with no headroom stalls page reclaim,
// freezes Aeron conductors past their timeout, and cascades into a
// full-cluster outage (issue #43, 2026-07-06).
func (p *Preflight) checkMemAvailable() InvariantResult {
	const name = "mem-available"
	data, err := os.ReadFile(p.meminfoPath)
	if err != nil {
		return InvariantResult{Name: name, OK: false, Severity: SeverityWarn,
			Detail: "cannot read " + p.meminfoPath + ": " + err.Error()}
	}
	avail, err := memAvailableBytes(data)
	if err != nil {
		return InvariantResult{Name: name, OK: false, Severity: SeverityWarn, Detail: err.Error()}
	}
	minMemMB := p.cfg.MinMem() // live: a profile switch moves this floor atomically
	blockBytes := int64(minMemMB) * 1024 * 1024
	warnBytes := blockBytes + blockBytes/2 // 1.5x
	detail := fmt.Sprintf("MemAvailable %dMB (block <%dMB, warn <%dMB; tune ADMIN_MIN_MEM_MB)",
		avail/(1024*1024), minMemMB, warnBytes/(1024*1024))
	switch {
	case avail < blockBytes:
		return InvariantResult{Name: name, OK: false, Severity: SeverityBlock, Detail: detail}
	case avail < warnBytes:
		return InvariantResult{Name: name, OK: false, Severity: SeverityWarn, Detail: detail}
	default:
		return InvariantResult{Name: name, OK: true, Detail: detail}
	}
}

// memAvailableBytes parses MemAvailable out of /proc/meminfo content.
func memAvailableBytes(meminfo []byte) (int64, error) {
	for _, line := range strings.Split(string(meminfo), "\n") {
		if !strings.HasPrefix(line, "MemAvailable:") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			break
		}
		var kb int64
		if _, err := fmt.Sscanf(fields[1], "%d", &kb); err != nil {
			return 0, fmt.Errorf("unparseable MemAvailable line: %q", line)
		}
		return kb * 1024, nil
	}
	return 0, fmt.Errorf("no MemAvailable in meminfo")
}

// MemAvailableBytes reports the current MemAvailable for the metrics endpoint
// (-1 when unreadable). Reading /proc/meminfo is memory-backed and safe at
// scrape time.
func (p *Preflight) MemAvailableBytes() int64 {
	data, err := os.ReadFile(p.meminfoPath)
	if err != nil {
		return -1
	}
	avail, err := memAvailableBytes(data)
	if err != nil {
		return -1
	}
	return avail
}

// ---- disk-space ----

// checkDiskSpace guards the two filesystems an operation can fill: the root
// disk (builds, logs, backups) and /dev/shm (archives + driver dirs — a full
// tmpfs wedges followers, RUNBOOKS §6).
func (p *Preflight) checkDiskSpace() InvariantResult {
	const name = "disk-space"
	var details []string
	ok := true

	if freeGB, totalGB, err := statfsGB(p.rootPath); err == nil {
		if freeGB < int64(p.cfg.MinRootDiskGB) {
			ok = false
			details = append(details, fmt.Sprintf("%s has %dGB free (block <%dGB; tune ADMIN_MIN_ROOT_DISK_GB)",
				p.rootPath, freeGB, p.cfg.MinRootDiskGB))
		} else {
			details = append(details, fmt.Sprintf("%s %d/%dGB free", p.rootPath, freeGB, totalGB))
		}
	}
	if usedPct, err := statfsUsedPct(p.shmPath); err == nil {
		if usedPct > p.cfg.MaxShmUsedPct {
			ok = false
			details = append(details, fmt.Sprintf("%s %d%% used (block >%d%%; tune ADMIN_MAX_SHM_USED_PCT)",
				p.shmPath, usedPct, p.cfg.MaxShmUsedPct))
		} else {
			details = append(details, fmt.Sprintf("%s %d%% used", p.shmPath, usedPct))
		}
	}

	r := InvariantResult{Name: name, OK: ok, Detail: strings.Join(details, "; ")}
	if !ok {
		r.Severity = SeverityBlock
	}
	return r
}

func statfsGB(path string) (freeGB, totalGB int64, err error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, 0, err
	}
	bs := int64(st.Bsize)
	return int64(st.Bavail) * bs / (1 << 30), int64(st.Blocks) * bs / (1 << 30), nil
}

func statfsUsedPct(path string) (int, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, err
	}
	if st.Blocks == 0 {
		return 0, nil
	}
	used := st.Blocks - st.Bavail
	return int(used * 100 / st.Blocks), nil
}

// ---- driver-dirs (#42) ----

// checkDriverDirs surfaces the exact #42 state: an external media driver's
// PID is alive but its /dev/shm dir (or cnc.dat) is gone — deleted under a
// live driver. The driver still shows "running" everywhere else while every
// node start against it fails the gate; this check makes the lie visible in
// /status and metrics instead.
func (p *Preflight) checkDriverDirs() InvariantResult {
	const name = "driver-dirs"
	if p.pm == nil {
		return InvariantResult{Name: name, OK: true, Detail: "process agent not wired"}
	}
	var failures []string
	for i := 0; i < 3; i++ {
		info := p.pm.Get(fmt.Sprintf("driver%d", i))
		if info == nil || !info.Running || !isProcessAlive(info.PID) {
			continue
		}
		cnc := p.driverDir(i) + "/cnc.dat"
		if _, err := os.Stat(cnc); err != nil {
			failures = append(failures, fmt.Sprintf(
				"driver%d (pid %d) alive but %s is missing — dir deleted under a live driver; restart driver%d then node%d (runbook 1)",
				i, info.PID, cnc, i, i))
		}
	}
	if len(failures) > 0 {
		return InvariantResult{Name: name, OK: false, Severity: SeverityBlock,
			Detail: strings.Join(failures, "; ")}
	}
	return InvariantResult{Name: name, OK: true}
}

// ---- artifacts (#45) ----

// checkArtifacts reports missing launch artifacts for the managed services.
// Warn severity: this is the observability twin of the ProcessManager's
// pre-start artifact check, which does the hard enforcement on every start
// path including auto-restart.
func (p *Preflight) checkArtifacts() InvariantResult {
	const name = "artifacts"
	artifacts := []struct{ path, dependents string }{
		{p.cfg.JarPath, "node0-2, backup"},
		{p.cfg.GatewayJar, "market"},
		{p.cfg.OmsJar, "oms"},
		{p.cfg.SimBinary, "sim"},
		{p.cfg.AssetsJar, "ae0"},
	}
	var missing []string
	for _, a := range artifacts {
		if _, err := os.Stat(a.path); err != nil {
			missing = append(missing, fmt.Sprintf("%s (%s)", a.path, a.dependents))
		}
	}
	if len(missing) > 0 {
		return InvariantResult{Name: name, OK: false, Severity: SeverityWarn,
			Detail: "missing: " + strings.Join(missing, ", ")}
	}
	return InvariantResult{Name: name, OK: true}
}

// ---- cluster-quorum (#43) ----

// checkQuorum requires all three nodes HEALTHY (per the derived health of
// health.go, the same source archiveOpBlockReason trusts). A rolling update
// started at 2/3 gambles the remaining follower on a clean catchup.
func (p *Preflight) checkQuorum() InvariantResult {
	const name = "cluster-quorum"
	if p.status == nil {
		return InvariantResult{Name: name, OK: true, Detail: "status service not wired"}
	}
	s := p.status.GetStatus()
	nodes, ok := s["nodes"].([]map[string]interface{})
	if !ok {
		return InvariantResult{Name: name, OK: false, Severity: SeverityBlock,
			Detail: "node health unavailable"}
	}
	healthy := 0
	var states []string
	for _, n := range nodes {
		h, _ := n["health"].(string)
		if h == HealthHealthy {
			healthy++
		}
		states = append(states, fmt.Sprintf("node%v=%s", n["id"], h))
	}
	detail := fmt.Sprintf("%d/%d healthy: %s", healthy, len(nodes), strings.Join(states, " "))
	if healthy < len(nodes) || len(nodes) == 0 {
		return InvariantResult{Name: name, OK: false, Severity: SeverityBlock, Detail: detail}
	}
	return InvariantResult{Name: name, OK: true, Detail: detail}
}
