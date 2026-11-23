// Package config reads the proxy's settings from the environment.
package config

import (
	"fmt"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
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
	// AllowNoKey lets serve run without an API key on an open address.
	AllowNoKey bool
	// MaxConcurrent limits the model requests in flight; 0 means no limit.
	MaxConcurrent int
	// UsageLimitPause is how long serve answers the usage limit at once after
	// upstream reports it with no reset time; 0 turns the pause off.
	UsageLimitPause time.Duration
}

// defaultUsageLimitPause is how long serve holds model requests back after
// upstream reports the usage limit with no reset time.
const defaultUsageLimitPause = 30 * time.Second

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
	// EnvMaxConcurrent limits the model requests in flight.
	EnvMaxConcurrent = "OAI_PLAN_PROXY_MAX_CONCURRENT"
	// EnvUsageLimitPause sets how long to hold back model requests after the
	// usage limit.
	EnvUsageLimitPause = "OAI_PLAN_PROXY_USAGE_LIMIT_PAUSE"
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
	if path := getenv(EnvAPIKeyFile); path != "" {
		key, err := readKeyFile(path, cfg.APIKey != "")
		if err != nil {
			return Config{}, err
		}
		cfg.APIKey = key
	}
	if v := getenv(EnvMaxConcurrent); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return Config{}, fmt.Errorf("%s must be a whole number of 0 or more, not %q", EnvMaxConcurrent, v)
		}
		cfg.MaxConcurrent = n
	}
	cfg.UsageLimitPause = defaultUsageLimitPause
	if v := getenv(EnvUsageLimitPause); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d < 0 || d > time.Hour {
			return Config{}, fmt.Errorf("%s must be a duration from 0 to 1h such as 30s, not %q", EnvUsageLimitPause, v)
		}
		cfg.UsageLimitPause = d
	}
	switch v := getenv(EnvAllowNoKey); v {
	case "":
	case "1":
		cfg.AllowNoKey = true
	default:
		return Config{}, fmt.Errorf("%s must be 1 or unset, not %q", EnvAllowNoKey, v)
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

// readKeyFile reads the API key from path. Its errors name the variable
// and never quote the file's content.
func readKeyFile(path string, keySet bool) (string, error) {
	if keySet {
		return "", fmt.Errorf("set %s or %s, not both", EnvAPIKey, EnvAPIKeyFile)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("%s: %w", EnvAPIKeyFile, err)
	}
	key := strings.TrimSpace(string(raw))
	if key == "" {
		return "", fmt.Errorf("%s: %s is empty", EnvAPIKeyFile, path)
	}
	return key, nil
}
