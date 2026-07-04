// SPDX-License-Identifier: Apache-2.0
package config

import (
	"os"
	"path/filepath"
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
	}
}

func getEnvOrDefault(key, defaultVal string) string {
	if val := os.Getenv(key); val != "" {
		return val
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
