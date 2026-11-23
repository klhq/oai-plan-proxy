// Package config reads the proxy's settings from the environment.
package config

import (
	"fmt"
	"net"
	"net/netip"
	"path/filepath"
)

const (
	defaultListen = "127.0.0.1:18080"
	appDirName    = "oai-plan-proxy"
)

// Config holds the settings shared by every command.
type Config struct {
	Listen  string
	DataDir string
	APIKey  string
	// LogFormat selects the serve log handler: text or json.
	LogFormat string
}

// Environment variable names.
const (
	EnvListen    = "OAI_PLAN_PROXY_LISTEN"
	EnvDataDir   = "OAI_PLAN_PROXY_DATA_DIR"
	EnvAPIKey    = "OAI_PLAN_PROXY_API_KEY"
	EnvLogFormat = "OAI_PLAN_PROXY_LOG_FORMAT"
	// EnvAPIKeyFile names a file that holds the API key.
	EnvAPIKeyFile = "OAI_PLAN_PROXY_API_KEY_FILE"
	// EnvAllowNoKey set to 1 lets serve run without a key on any address.
	EnvAllowNoKey = "OAI_PLAN_PROXY_ALLOW_NO_KEY"
)

// Load reads settings through getenv. userConfigDir supplies the base
// directory for the default data directory. Load calls it only when
// OAI_PLAN_PROXY_DATA_DIR is unset, because a container without HOME has no
// user config directory.
func Load(getenv func(string) string, userConfigDir func() (string, error)) (Config, error) {
	cfg := Config{
		Listen:    getenv(EnvListen),
		DataDir:   getenv(EnvDataDir),
		APIKey:    getenv(EnvAPIKey),
		LogFormat: getenv(EnvLogFormat),
	}
	if cfg.Listen == "" {
		cfg.Listen = defaultListen
	}
	if cfg.DataDir == "" {
		base, err := userConfigDir()
		if err != nil {
			return Config{}, fmt.Errorf("locate user config directory (set %s instead): %w", EnvDataDir, err)
		}
		cfg.DataDir = filepath.Join(base, appDirName)
	}
	return cfg, nil
}

// Loopback reports whether the listen address only accepts connections
// from this host: localhost, an IPv4 address in 127.0.0.0/8, or ::1. An
// empty host, a wildcard, and any other name are not loopback.
func Loopback(listen string) bool {
	host, _, err := net.SplitHostPort(listen)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip, err := netip.ParseAddr(host)
	return err == nil && ip.IsLoopback()
}
