package config

import "testing"

func TestRegistrationDefaultsDisabled(t *testing.T) {
	t.Setenv("GPUARDIAN_WEB_ALLOW_REGISTRATION", "")
	if cfg := Default(); cfg.WebAllowRegistration {
		t.Fatal("public registration was enabled by default")
	}
}

func TestDefaultLoadsSecuritySettings(t *testing.T) {
	t.Setenv("GPUARDIAN_NODE_ALLOW_INSECURE", "true")
	t.Setenv("GPUARDIAN_WEB_ALLOW_INSECURE", "1")
	t.Setenv("GPUARDIAN_WEB_ALLOW_INSECURE_NODES", "true")
	t.Setenv("GPUARDIAN_WEB_ALLOW_REGISTRATION", "true")
	t.Setenv("GPUARDIAN_WEB_SECURE_COOKIES", "TRUE")
	t.Setenv("GPUARDIAN_WEB_TRUST_PROXY", "true")
	t.Setenv("GPUARDIAN_WEB_TLS_CERT", "/tls/web.crt")
	t.Setenv("GPUARDIAN_WEB_TLS_KEY", "/tls/web.key")
	t.Setenv("GPUARDIAN_WEB_SESSION_KEY", "/state/session.key")
	t.Setenv("GPUARDIAN_WEB_USER_KEY", "/state/user-key.key")
	t.Setenv("GPUARDIAN_GPU_COUNT", "8")

	cfg := Default()
	if !cfg.NodeAllowInsecure || !cfg.WebAllowInsecure || !cfg.WebAllowInsecureNodes || !cfg.WebAllowRegistration || !cfg.WebSecureCookies || !cfg.WebTrustProxy {
		t.Fatalf("boolean security settings were not loaded: %+v", cfg)
	}
	if cfg.WebTLSCert != "/tls/web.crt" || cfg.WebTLSKey != "/tls/web.key" || cfg.WebSessionKey != "/state/session.key" || cfg.WebUserKey != "/state/user-key.key" {
		t.Fatalf("web security paths were not loaded: %+v", cfg)
	}
	if cfg.GPUCount != 8 {
		t.Fatalf("GPUCount = %d, want 8", cfg.GPUCount)
	}
}

func TestInvalidEnvironmentValuesFailClosed(t *testing.T) {
	t.Setenv("GPUARDIAN_WEB_ALLOW_INSECURE", "definitely")
	t.Setenv("GPUARDIAN_WEB_ALLOW_INSECURE_NODES", "definitely")
	t.Setenv("GPUARDIAN_WEB_ALLOW_REGISTRATION", "definitely")
	t.Setenv("GPUARDIAN_GPU_COUNT", "1000000")
	cfg := Default()
	if cfg.WebAllowInsecure {
		t.Fatal("invalid boolean enabled insecure web mode")
	}
	if cfg.WebAllowInsecureNodes {
		t.Fatal("invalid boolean enabled insecure node transport")
	}
	if cfg.WebAllowRegistration {
		t.Fatal("invalid boolean enabled public registration")
	}
	if cfg.GPUCount != 0 {
		t.Fatalf("unbounded GPU count accepted: %d", cfg.GPUCount)
	}
}

func TestOtherInsecureFlagsDoNotEnableInsecureNodes(t *testing.T) {
	t.Setenv("GPUARDIAN_NODE_ALLOW_INSECURE", "true")
	t.Setenv("GPUARDIAN_WEB_ALLOW_INSECURE", "true")
	t.Setenv("GPUARDIAN_WEB_ALLOW_INSECURE_NODES", "")
	if cfg := Default(); cfg.WebAllowInsecureNodes {
		t.Fatal("listener security overrides enabled gateway-to-node HTTP")
	}
}
