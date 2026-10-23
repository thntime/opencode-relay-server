package config_test

import (
	"testing"

	"github.com/zero469/opencode-relay-server/internal/config"
)

// 前提：config.getEnv 把空字符串视同未设置，因此 t.Setenv(..., "") 等价于
// 未导出该变量；下面的断言都依赖这一行为。
func TestLoad_FrpsDefaultsAreEmpty(t *testing.T) {
	t.Setenv("FRPS_HOST", "")
	t.Setenv("FRPS_PORT", "")
	t.Setenv("FRPS_DOMAIN", "")
	t.Setenv("FRPS_TOKEN", "")

	cfg := config.Load()

	if cfg.FrpsHost != "" {
		t.Errorf("FrpsHost = %q, want empty when FRPS_HOST is unset", cfg.FrpsHost)
	}
	if cfg.FrpsPort != "" {
		t.Errorf("FrpsPort = %q, want empty when FRPS_PORT is unset", cfg.FrpsPort)
	}
	if cfg.Domain != "" {
		t.Errorf("Domain = %q, want empty when FRPS_DOMAIN is unset", cfg.Domain)
	}
	if cfg.FrpsToken != "" {
		t.Errorf("FrpsToken = %q, want empty when FRPS_TOKEN is unset", cfg.FrpsToken)
	}
}

func TestLoad_FrpsFromEnv(t *testing.T) {
	t.Setenv("FRPS_HOST", "frps.example.com")
	t.Setenv("FRPS_PORT", "7443")
	t.Setenv("FRPS_DOMAIN", "example.com")
	t.Setenv("FRPS_TOKEN", "secret-token")

	cfg := config.Load()

	if cfg.FrpsHost != "frps.example.com" {
		t.Errorf("FrpsHost = %q, want frps.example.com", cfg.FrpsHost)
	}
	if cfg.FrpsPort != "7443" {
		t.Errorf("FrpsPort = %q, want 7443", cfg.FrpsPort)
	}
	if cfg.Domain != "example.com" {
		t.Errorf("Domain = %q, want example.com", cfg.Domain)
	}
	if cfg.FrpsToken != "secret-token" {
		t.Errorf("FrpsToken = %q, want secret-token", cfg.FrpsToken)
	}
}
