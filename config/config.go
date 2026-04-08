package config

import (
	"os"
	"path/filepath"
)

type Config struct {
	Port          string
	ProjectDir    string
	OmsProjectDir string
	JarPath       string // Cluster JAR (for ClusterTool operations)
	GatewayJar    string // Gateway JAR
	OmsJar        string // OMS uber JAR
	LogDir        string
	ClusterDir    string
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

	homeDir, _ := os.UserHomeDir()

	return &Config{
		Port:          getEnvOrDefault("ADMIN_PORT", "8082"),
		ProjectDir:    projectDir,
		OmsProjectDir: omsProjectDir,
		JarPath:       filepath.Join(projectDir, "match-cluster/target/match-cluster.jar"),
		GatewayJar:    filepath.Join(projectDir, "match-gateway/target/match-gateway.jar"),
		OmsJar:        filepath.Join(omsProjectDir, "oms-app/target/oms-app.jar"),
		LogDir:        filepath.Join(homeDir, ".local/log/cluster"),
		ClusterDir:    "/dev/shm/aeron-cluster",
	}
}

func getEnvOrDefault(key, defaultVal string) string {
	if val := os.Getenv(key); val != "" {
		return val
	}
	return defaultVal
}
