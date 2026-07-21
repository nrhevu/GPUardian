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
	GPUVendor             string
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
	WebUserKey            string
	WebRegistry           string
	WebUsers              string
	WebUIDir              string
	WebUser               string
	WebPassword           string
	WebDB                 string
}

func Default() Config {
	return Config{
		SocketPath:            env("GPUARDIAN_SOCKET", "/run/gpuardian.sock"),
		StatePath:             env("GPUARDIAN_STATE", "/var/lib/gpuardian/state.json"),
		RootKeyPath:           env("GPUARDIAN_ROOT_KEY", "/var/lib/gpuardian/root.key"),
		AuditLog:              env("GPUARDIAN_AUDIT_LOG", "/var/log/gpuardian/audit.log"),
		CgroupRoot:            env("GPUARDIAN_CGROUP_ROOT", "/sys/fs/cgroup/gpuardian"),
		ProcRoot:              env("GPUARDIAN_PROC_ROOT", "/proc"),
		DryRun:                envBool("GPUARDIAN_DRY_RUN"),
		GPUCount:              envInt("GPUARDIAN_GPU_COUNT", 0),
		GPUVendor:             env("GPUARDIAN_GPU_VENDOR", "auto"),
		NodeAddr:              env("GPUARDIAN_NODE_ADDR", ""),
		NodeTLSCert:           env("GPUARDIAN_NODE_TLS_CERT", ""),
		NodeTLSKey:            env("GPUARDIAN_NODE_TLS_KEY", ""),
		NodeAllowInsecure:     envBool("GPUARDIAN_NODE_ALLOW_INSECURE"),
		NodeIDPath:            env("GPUARDIAN_NODE_ID", "/var/lib/gpuardian/node.id"),
		TelemetryDir:          env("GPUARDIAN_TELEMETRY_DIR", "/var/lib/gpuardian/telemetry"),
		WebAddr:               env("GPUARDIAN_WEB_ADDR", "127.0.0.1:8080"),
		WebTLSCert:            env("GPUARDIAN_WEB_TLS_CERT", ""),
		WebTLSKey:             env("GPUARDIAN_WEB_TLS_KEY", ""),
		WebAllowInsecure:      envBool("GPUARDIAN_WEB_ALLOW_INSECURE"),
		WebAllowInsecureNodes: envBool("GPUARDIAN_WEB_ALLOW_INSECURE_NODES"),
		WebAllowRegistration:  envBool("GPUARDIAN_WEB_ALLOW_REGISTRATION"),
		WebSecureCookies:      envBool("GPUARDIAN_WEB_SECURE_COOKIES"),
		WebTrustProxy:         envBool("GPUARDIAN_WEB_TRUST_PROXY"),
		WebSessionKey:         env("GPUARDIAN_WEB_SESSION_KEY", "/var/lib/gpuardian/web-session.key"),
		WebUserKey:            env("GPUARDIAN_WEB_USER_KEY", "/var/lib/gpuardian-web/user-key.key"),
		WebRegistry:           env("GPUARDIAN_WEB_REGISTRY", "/var/lib/gpuardian/web-servers.json"),
		WebUsers:              env("GPUARDIAN_WEB_USERS", "/var/lib/gpuardian/web-users.json"),
		WebUIDir:              env("GPUARDIAN_WEB_UI_DIR", "web/ui/dist"),
		WebUser:               env("GPUARDIAN_WEB_USER", "admin"),
		WebPassword:           env("GPUARDIAN_WEB_PASSWORD", ""),
		WebDB:                 env("GPUARDIAN_WEB_DB", "/var/lib/gpuardian-web/history.db"),
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
