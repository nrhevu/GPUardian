package config

import (
	"os"
	"strconv"
)

type Config struct {
	SocketPath  string
	StatePath   string
	RootKeyPath string
	AuditLog    string
	CgroupRoot  string
	ProcRoot    string
	DryRun      bool
	GPUCount    int
	NodeAddr    string
	NodeTLSCert string
	NodeTLSKey  string
	WebAddr     string
	WebRegistry string
	WebUIDir    string
	WebUser     string
	WebPassword string
}

func Default() Config {
	return Config{
		SocketPath:  env("ROCGUARD_SOCKET", "/run/rocguard.sock"),
		StatePath:   env("ROCGUARD_STATE", "/var/lib/rocguard/state.json"),
		RootKeyPath: env("ROCGUARD_ROOT_KEY", "/var/lib/rocguard/root.key"),
		AuditLog:    env("ROCGUARD_AUDIT_LOG", "/var/log/rocguard/audit.log"),
		CgroupRoot:  env("ROCGUARD_CGROUP_ROOT", "/sys/fs/cgroup/rocguard"),
		ProcRoot:    env("ROCGUARD_PROC_ROOT", "/proc"),
		DryRun:      env("ROCGUARD_DRY_RUN", "") == "1",
		GPUCount:    envInt("ROCGUARD_GPU_COUNT", 0),
		NodeAddr:    env("ROCGUARD_NODE_ADDR", ""),
		NodeTLSCert: env("ROCGUARD_NODE_TLS_CERT", ""),
		NodeTLSKey:  env("ROCGUARD_NODE_TLS_KEY", ""),
		WebAddr:     env("ROCGUARD_WEB_ADDR", "127.0.0.1:8080"),
		WebRegistry: env("ROCGUARD_WEB_REGISTRY", "/var/lib/rocguard/web-servers.json"),
		WebUIDir:    env("ROCGUARD_WEB_UI_DIR", "web/ui/dist"),
		WebUser:     env("ROCGUARD_WEB_USER", "admin"),
		WebPassword: env("ROCGUARD_WEB_PASSWORD", ""),
	}
}

func env(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func envInt(key string, fallback int) int {
	value := os.Getenv(key)
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return fallback
	}
	return parsed
}
