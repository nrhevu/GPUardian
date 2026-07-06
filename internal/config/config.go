package config

import "os"

type Config struct {
	SocketPath  string
	StatePath   string
	RootKeyPath string
	AuditLog    string
	CgroupRoot  string
	ProcRoot    string
}

func Default() Config {
	return Config{
		SocketPath:  env("ROCGUARD_SOCKET", "/run/rocguard.sock"),
		StatePath:   env("ROCGUARD_STATE", "/var/lib/rocguard/state.json"),
		RootKeyPath: env("ROCGUARD_ROOT_KEY", "/var/lib/rocguard/root.key"),
		AuditLog:    env("ROCGUARD_AUDIT_LOG", "/var/log/rocguard/audit.log"),
		CgroupRoot:  env("ROCGUARD_CGROUP_ROOT", "/sys/fs/cgroup/rocguard"),
		ProcRoot:    env("ROCGUARD_PROC_ROOT", "/proc"),
	}
}

func env(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
