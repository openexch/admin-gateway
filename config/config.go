// SPDX-License-Identifier: Apache-2.0
package config

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

type Config struct {
	Port          string
	BindAddr      string // listen address; loopback by default (admin-gateway#11)
	AuthToken     string // bearer token for the admin API; empty = loopback-only dev mode
	ProjectDir    string
	AdminDir      string // this repo's checkout (admin-gateway source + live binary)
	OmsProjectDir string
	JarPath       string // Cluster JAR (for ClusterTool operations)
	GatewayJar    string // Gateway JAR
	OmsJar        string // OMS uber JAR
	LogDir        string
	ClusterDir    string
	LogFormat     string // "json" (default) or "text" (ADMIN_LOG_FORMAT)
	GoBin         string // go binary for rebuild-admin (ADMIN_GO_BIN; see resolveGoBin)
	SimBinary     string // market simulator binary (SIM_BINARY; openexch/tools market-sim)

	// Pre-flight invariant thresholds (services/preflight.go, #42/#43/#45).
	// Defaults sized for the 31G demo box: steady-state MemAvailable there is
	// ~7-8GB with the full stack up, and the #43 OOM hit during a node
	// restart's catchup transient.
	MinMemMB      int // block gated ops below this MemAvailable (ADMIN_MIN_MEM_MB)
	MinRootDiskGB int // block gated ops below this free space on / (ADMIN_MIN_ROOT_DISK_GB)
	MaxShmUsedPct int // block gated ops above this /dev/shm usage (ADMIN_MAX_SHM_USED_PCT)
	BuildNice     int // niceness for rebuild mvn/go/rsync (ADMIN_BUILD_NICE; 0 disables)
}

func Load() *Config {
	projectDir := os.Getenv("MATCH_PROJECT_DIR")
	if projectDir == "" {
		// Default to parent of admin-gateway
		exe, _ := os.Executable()
		projectDir = filepath.Dir(filepath.Dir(exe))
	}

	omsProjectDir := os.Getenv("OMS_PROJECT_DIR")
	if omsProjectDir == "" {
		// Default to sibling order-management directory
		omsProjectDir = filepath.Join(filepath.Dir(projectDir), "order-management")
	}

	// admin-gateway lives in its OWN repo since the split; it is NOT under
	// MATCH_PROJECT_DIR anymore. Default to the running binary's directory,
	// which is the repo checkout in every deploy mode we have.
	adminDir := os.Getenv("ADMIN_PROJECT_DIR")
	if adminDir == "" {
		exe, _ := os.Executable()
		adminDir = filepath.Dir(exe)
	}

	homeDir, _ := os.UserHomeDir()

	return &Config{
		Port:          getEnvOrDefault("ADMIN_PORT", "8082"),
		BindAddr:      getEnvOrDefault("ADMIN_BIND", "127.0.0.1"),
		AuthToken:     loadAuthToken(),
		ProjectDir:    projectDir,
		AdminDir:      adminDir,
		OmsProjectDir: omsProjectDir,
		JarPath:       filepath.Join(projectDir, "match-cluster/target/match-cluster.jar"),
		GatewayJar:    filepath.Join(projectDir, "match-gateway/target/match-gateway.jar"),
		OmsJar:        filepath.Join(omsProjectDir, "oms-app/target/oms-app.jar"),
		LogDir:        filepath.Join(homeDir, ".local/log/cluster"),
		ClusterDir:    "/dev/shm/aeron-cluster",
		LogFormat:     getEnvOrDefault("ADMIN_LOG_FORMAT", "json"),
		GoBin:         resolveGoBin(),
		// tools is a sibling repo of match/order-management; build with
		// `cd tools/market-sim && go build -o market-sim .`
		SimBinary: getEnvOrDefault("SIM_BINARY",
			filepath.Join(filepath.Dir(projectDir), "tools/market-sim/market-sim")),
		MinMemMB:      getEnvIntOrDefault("ADMIN_MIN_MEM_MB", 4096),
		MinRootDiskGB: getEnvIntOrDefault("ADMIN_MIN_ROOT_DISK_GB", 5),
		MaxShmUsedPct: getEnvIntOrDefault("ADMIN_MAX_SHM_USED_PCT", 90),
		BuildNice:     getEnvIntOrDefault("ADMIN_BUILD_NICE", 10),
	}
}

// resolveGoBin picks the Go toolchain rebuild-admin builds with (#36). The
// systemd user environment can resolve a different (older) go than an
// interactive shell — on Ubuntu, /usr/bin/go is the apt toolchain with
// GOTOOLCHAIN downloads disabled, which fails go.mod's minimum with
// "toolchain not available" while `go build` works fine in a terminal.
// Order: ADMIN_GO_BIN > the conventional upstream install > PATH.
func resolveGoBin() string {
	if bin := os.Getenv("ADMIN_GO_BIN"); bin != "" {
		return bin
	}
	if _, err := os.Stat("/usr/local/go/bin/go"); err == nil {
		return "/usr/local/go/bin/go"
	}
	if bin, err := exec.LookPath("go"); err == nil {
		return bin
	}
	return "go"
}

func getEnvOrDefault(key, defaultVal string) string {
	if val := os.Getenv(key); val != "" {
		return val
	}
	return defaultVal
}

func getEnvIntOrDefault(key string, defaultVal int) int {
	if val := os.Getenv(key); val != "" {
		if n, err := strconv.Atoi(val); err == nil {
			return n
		}
	}
	return defaultVal
}

// loadAuthToken reads the admin API bearer token from ADMIN_AUTH_TOKEN, or
// from the file named by ADMIN_AUTH_TOKEN_FILE (surrounding whitespace trimmed).
func loadAuthToken() string {
	if tok := os.Getenv("ADMIN_AUTH_TOKEN"); tok != "" {
		return tok
	}
	if file := os.Getenv("ADMIN_AUTH_TOKEN_FILE"); file != "" {
		data, err := os.ReadFile(file)
		if err != nil {
			// Fail closed: main refuses non-loopback binds without a token.
			return ""
		}
		return strings.TrimSpace(string(data))
	}
	return ""
}
