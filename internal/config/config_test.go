package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/klhq/oai-plan-proxy/internal/config"
)

func env(vars map[string]string) func(string) string {
	return func(key string) string { return vars[key] }
}

func TestLoadUsesDefaultsWhenNothingIsSet(t *testing.T) {
	userConfigDir := t.TempDir()

	cfg, err := config.Load(env(nil), func() (string, error) { return userConfigDir, nil })
	if err != nil {
		t.Fatal(err)
	}

	if cfg.Listen != "127.0.0.1:18080" {
		t.Errorf("Listen = %q, want 127.0.0.1:18080", cfg.Listen)
	}
	if want := filepath.Join(userConfigDir, "oai-plan-proxy"); cfg.DataDir != want {
		t.Errorf("DataDir = %q, want %q", cfg.DataDir, want)
	}
	if cfg.APIKey != "" {
		t.Errorf("APIKey = %q, want empty", cfg.APIKey)
	}
}

func TestLoadReadsEnvironmentOverrides(t *testing.T) {
	failIfCalled := func() (string, error) {
		t.Fatal("user config dir must not be consulted when OAI_PLAN_PROXY_DATA_DIR is set")
		return "", nil
	}

	cfg, err := config.Load(env(map[string]string{
		"OAI_PLAN_PROXY_LISTEN":   "0.0.0.0:9000",
		"OAI_PLAN_PROXY_DATA_DIR": "/srv/proxy",
		"OAI_PLAN_PROXY_API_KEY":  "incoming-key",
	}), failIfCalled)
	if err != nil {
		t.Fatal(err)
	}

	want := config.Config{Listen: "0.0.0.0:9000", DataDir: "/srv/proxy", APIKey: "incoming-key"}
	if cfg != want {
		t.Fatalf("cfg = %+v, want %+v", cfg, want)
	}
}

func TestOnlyLoopbackListenAddressesCountAsLocal(t *testing.T) {
	for listen, want := range map[string]bool{
		"127.0.0.1:18080":   true,
		"127.0.0.2:18080":   true,
		"localhost:18080":   true,
		"[::1]:18080":       true,
		"0.0.0.0:18080":     false,
		":18080":            false,
		"[::]:18080":        false,
		"192.168.1.5:18080": false,
		"proxy.lan:18080":   false,
		"not an address":    false,
	} {
		if got := config.Loopback(listen); got != want {
			t.Errorf("Loopback(%q) = %v, want %v", listen, got, want)
		}
	}
}

func TestAllowNoKeyAcceptsOnlyOne(t *testing.T) {
	for value, want := range map[string]bool{"": false, "1": true} {
		cfg, err := config.Load(env(map[string]string{config.EnvDataDir: t.TempDir(), config.EnvAllowNoKey: value}), nil)
		if err != nil || cfg.AllowNoKey != want {
			t.Errorf("%q: AllowNoKey = %v, err = %v; want %v and no error", value, cfg.AllowNoKey, err, want)
		}
	}
	for _, value := range []string{"0", "true", "yes", "false"} {
		_, err := config.Load(env(map[string]string{config.EnvDataDir: t.TempDir(), config.EnvAllowNoKey: value}), nil)
		if err == nil || !strings.Contains(err.Error(), config.EnvAllowNoKey) {
			t.Errorf("%q: err = %v, want an error naming %s", value, err, config.EnvAllowNoKey)
		}
	}
}

// keyFile writes content to a new file and returns its path.
func keyFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "api-key")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestTheAPIKeyCanComeFromAFile(t *testing.T) {
	path := keyFile(t, "  file-key-123\n")

	cfg, err := config.Load(env(map[string]string{config.EnvDataDir: t.TempDir(), config.EnvAPIKeyFile: path}), nil)

	if err != nil || cfg.APIKey != "file-key-123" {
		t.Fatalf("APIKey = %q, err = %v; want file-key-123", cfg.APIKey, err)
	}
}

func TestABadAPIKeyFileIsAConfigErrorThatHidesTheKey(t *testing.T) {
	secret := "file-key-123"
	for name, vars := range map[string]map[string]string{
		"both set":    {config.EnvAPIKey: "env-key", config.EnvAPIKeyFile: keyFile(t, secret)},
		"missing":     {config.EnvAPIKeyFile: filepath.Join(t.TempDir(), "absent")},
		"blank":       {config.EnvAPIKeyFile: keyFile(t, " \n\t")},
		"a directory": {config.EnvAPIKeyFile: t.TempDir()},
	} {
		vars[config.EnvDataDir] = t.TempDir()

		_, err := config.Load(env(vars), nil)

		if err == nil || !strings.Contains(err.Error(), config.EnvAPIKeyFile) || strings.Contains(err.Error(), secret) {
			t.Errorf("%s: err = %v, want an error naming %s without the key", name, err, config.EnvAPIKeyFile)
		}
	}
}

func TestMaxConcurrentIsAWholeNumber(t *testing.T) {
	for value, want := range map[string]int{"": 0, "0": 0, "4": 4} {
		cfg, err := config.Load(env(map[string]string{config.EnvDataDir: t.TempDir(), config.EnvMaxConcurrent: value}), nil)
		if err != nil || cfg.MaxConcurrent != want {
			t.Errorf("%q: MaxConcurrent = %d, err = %v; want %d", value, cfg.MaxConcurrent, err, want)
		}
	}
	for _, value := range []string{"-1", "four", "1.5", " 4"} {
		_, err := config.Load(env(map[string]string{config.EnvDataDir: t.TempDir(), config.EnvMaxConcurrent: value}), nil)
		if err == nil || !strings.Contains(err.Error(), config.EnvMaxConcurrent) {
			t.Errorf("%q: err = %v, want an error naming %s", value, err, config.EnvMaxConcurrent)
		}
	}
}
