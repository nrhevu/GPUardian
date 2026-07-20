package config

import (
	"os"
	"strconv"
)

type Config struct {
	SocketPath            string
	StatePath             string
	RootKeyPath           string
	AuditLog              string
	CgroupRoot            string
	ProcRoot              string
	DryRun                bool
	GPUCount              int
	NodeAddr              string
	NodeTLSCert           string
	NodeTLSKey            string
	NodeAllowInsecure     bool
	NodeIDPath            string
	TelemetryDir          string
	WebAddr               string
	WebTLSCert            string
	WebTLSKey             string
	WebAllowInsecure      bool
	WebAllowInsecureNodes bool
	WebAllowRegistration  bool
	WebSecureCookies      bool
	WebTrustProxy         bool
	WebSessionKey         string
	WebRegistry           string
	WebUsers              string
	WebUIDir              string
	WebUser               string
	WebPassword           string
	WebDB                 string
}

func Default() Config {
	return Config{
		SocketPath:            env("ROCGUARD_SOCKET", "/run/rocguard.sock"),
		StatePath:             env("ROCGUARD_STATE", "/var/lib/rocguard/state.json"),
		RootKeyPath:           env("ROCGUARD_ROOT_KEY", "/var/lib/rocguard/root.key"),
		AuditLog:              env("ROCGUARD_AUDIT_LOG", "/var/log/rocguard/audit.log"),
		CgroupRoot:            env("ROCGUARD_CGROUP_ROOT", "/sys/fs/cgroup/rocguard"),
		ProcRoot:              env("ROCGUARD_PROC_ROOT", "/proc"),
		DryRun:                envBool("ROCGUARD_DRY_RUN"),
		GPUCount:              envInt("ROCGUARD_GPU_COUNT", 0),
		NodeAddr:              env("ROCGUARD_NODE_ADDR", ""),
		NodeTLSCert:           env("ROCGUARD_NODE_TLS_CERT", ""),
		NodeTLSKey:            env("ROCGUARD_NODE_TLS_KEY", ""),
		NodeAllowInsecure:     envBool("ROCGUARD_NODE_ALLOW_INSECURE"),
		NodeIDPath:            env("ROCGUARD_NODE_ID", "/var/lib/rocguard/node.id"),
		TelemetryDir:          env("ROCGUARD_TELEMETRY_DIR", "/var/lib/rocguard/telemetry"),
		WebAddr:               env("ROCGUARD_WEB_ADDR", "127.0.0.1:8080"),
		WebTLSCert:            env("ROCGUARD_WEB_TLS_CERT", ""),
		WebTLSKey:             env("ROCGUARD_WEB_TLS_KEY", ""),
		WebAllowInsecure:      envBool("ROCGUARD_WEB_ALLOW_INSECURE"),
		WebAllowInsecureNodes: envBool("ROCGUARD_WEB_ALLOW_INSECURE_NODES"),
		WebAllowRegistration:  envBool("ROCGUARD_WEB_ALLOW_REGISTRATION"),
		WebSecureCookies:      envBool("ROCGUARD_WEB_SECURE_COOKIES"),
		WebTrustProxy:         envBool("ROCGUARD_WEB_TRUST_PROXY"),
		WebSessionKey:         env("ROCGUARD_WEB_SESSION_KEY", "/var/lib/rocguard/web-session.key"),
		WebRegistry:           env("ROCGUARD_WEB_REGISTRY", "/var/lib/rocguard/web-servers.json"),
		WebUsers:              env("ROCGUARD_WEB_USERS", "/var/lib/rocguard/web-users.json"),
		WebUIDir:              env("ROCGUARD_WEB_UI_DIR", "web/ui/dist"),
		WebUser:               env("ROCGUARD_WEB_USER", "admin"),
		WebPassword:           env("ROCGUARD_WEB_PASSWORD", ""),
		WebDB:                 env("ROCGUARD_WEB_DB", "/var/lib/rocguard-web/history.db"),
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
	if err != nil || parsed < 0 || parsed > 1024 {
		return fallback
	}
	return parsed
}

func envBool(key string) bool {
	value, err := strconv.ParseBool(os.Getenv(key))
	return err == nil && value
}
