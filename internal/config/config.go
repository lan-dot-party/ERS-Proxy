// Package config loads and validates the ERS-Proxy YAML configuration.
//
// Only process-level settings live here. Devices, groups and users are stored
// in the database and managed through the web UI or the command line.
package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// DefaultPath is used when neither --config nor ERSPROXY_CONFIG is set.
const DefaultPath = "/etc/ers-proxy/config.yaml"

// Open modes for devices.
const (
	OpenEmbedded = "embedded"
	OpenTab      = "tab"
	OpenWindow   = "window"
)

// ValidOpenMode reports whether m is a known open mode.
func ValidOpenMode(m string) bool {
	return m == OpenEmbedded || m == OpenTab || m == OpenWindow
}

// Config is the root of config.yaml.
type Config struct {
	// Listen is the HTTP listen address, e.g. "127.0.0.1:9050".
	Listen string `yaml:"listen"`
	// PublicURL is the externally visible base URL (scheme, host and port, no
	// path). If it starts with https://, session cookies are marked Secure.
	// Requests whose Origin header differs from it are rejected.
	PublicURL string `yaml:"public_url"`
	// TrustedProxies are the proxy IPs or networks whose X-Real-IP /
	// X-Forwarded-For headers are trusted for client IPs (lockout, audit log).
	TrustedProxies []string `yaml:"trusted_proxies"`
	// SecretKeyFile is the file with the AES-256 key that encrypts switch
	// passwords in the database (64 hex characters). It is created if missing.
	SecretKeyFile string `yaml:"secret_key_file"`

	// Storage configures the SQLite database.
	Storage Storage `yaml:"storage"`
	// Log configures logging.
	Log Log `yaml:"log"`
	// TLS enables the built-in TLS listener; usually a reverse proxy does this.
	TLS TLS `yaml:"tls"`
	// Proxy configures the connection to the devices.
	Proxy Proxy `yaml:"proxy"`
	// Health configures the reachability checks.
	Health Health `yaml:"health"`
	// UI holds defaults for the web interface.
	UI UI `yaml:"ui"`
	// Monitor configures port monitoring (history retention).
	Monitor Monitor `yaml:"monitor"`
	// Configure controls changing switch settings from the dashboard.
	Configure Configure `yaml:"configure"`

	// dir is the directory of the loaded file; relative paths resolve against it.
	dir string
}

// Storage configures the SQLite database.
type Storage struct {
	// Path is the SQLite database file. Its directory is created if missing.
	Path string `yaml:"path"`
}

// Log configures log/slog output.
type Log struct {
	// Level is the minimum log level: debug, info, warn or error.
	Level string `yaml:"level"`
	// Format is the log output format: text or json.
	Format string `yaml:"format"`
}

// TLS enables the built-in TLS listener. Usually a reverse proxy does this.
type TLS struct {
	// CertFile is the PEM certificate (chain). Set together with tls.key_file.
	CertFile string `yaml:"cert_file"`
	// KeyFile is the PEM private key.
	KeyFile string `yaml:"key_file"`
}

// Proxy configures the connections from ERS-Proxy to the devices.
type Proxy struct {
	// RequestTimeout is the maximum time for one request to a switch,
	// including reading the response.
	RequestTimeout time.Duration `yaml:"request_timeout"`
	// IdleTimeout ends a proxy session (an opened device) after this time
	// without requests. The device is simply opened again from the portal.
	IdleTimeout time.Duration `yaml:"idle_timeout"`
	// MaxConnsPerDevice limits parallel connections to one switch. Old switch
	// web servers handle only a few connections at a time.
	MaxConnsPerDevice int `yaml:"max_conns_per_device"`
	// MaxBodyBytes limits request and response bodies passing the proxy.
	MaxBodyBytes int64 `yaml:"max_body_bytes"`
}

// Health configures the reachability checks of devices.
type Health struct {
	// Enabled turns the checks on. Devices can opt out individually.
	Enabled bool `yaml:"enabled"`
	// Interval is the time between two checks of a device.
	Interval time.Duration `yaml:"interval"`
	// Timeout is the timeout of one check.
	Timeout time.Duration `yaml:"timeout"`
}

// UI holds defaults for the web interface.
type UI struct {
	// DefaultOpenMode is used for users and devices that do not choose one:
	// embedded (tab inside the portal), tab (new browser tab) or window (popup).
	DefaultOpenMode string `yaml:"default_open_mode"`
}

// Configure controls writing to switches.
type Configure struct {
	// Enabled allows changing switch settings from the dashboard. Off by default; only config.yaml can turn it on (no environment variable).
	Enabled bool `yaml:"enabled"`
	// ConfirmTimeout is how long a change waits for confirmation before it is reverted.
	ConfirmTimeout time.Duration `yaml:"confirm_timeout"`
}

// Monitor configures port monitoring.
type Monitor struct {
	// Retention says how long history is kept per resolution.
	Retention Retention `yaml:"retention"`
}

// Retention is the storage time of port history. Samples are taken per poll,
// aggregated to minutes, 15 minutes and hours, and deleted after these times.
type Retention struct {
	// Minute is the retention of the 1-minute resolution.
	Minute time.Duration `yaml:"minute"`
	// Quarter is the retention of the 15-minute resolution.
	Quarter time.Duration `yaml:"quarter"`
	// Hour is the retention of the 1-hour resolution.
	Hour time.Duration `yaml:"hour"`
	// Events is the retention of port events (link changes etc.).
	Events time.Duration `yaml:"events"`
}

// Default returns a configuration with all defaults applied.
func Default() *Config {
	c := base()
	c.applyDefaults()
	return c
}

// base holds defaults that cannot be told apart from an unset value after
// unmarshalling (booleans that default to true).
func base() *Config {
	return &Config{Health: Health{Enabled: true}}
}

func (c *Config) applyDefaults() {
	if c.Listen == "" {
		c.Listen = "127.0.0.1:9050"
	}
	if c.PublicURL == "" {
		c.PublicURL = "http://" + c.Listen
	}
	c.PublicURL = strings.TrimRight(c.PublicURL, "/")
	if c.TrustedProxies == nil {
		c.TrustedProxies = []string{"127.0.0.1", "::1"}
	}
	if c.SecretKeyFile == "" {
		c.SecretKeyFile = "secret.key"
	}
	if c.Storage.Path == "" {
		c.Storage.Path = "data/ers-proxy.db"
	}
	if c.Log.Level == "" {
		c.Log.Level = "info"
	}
	if c.Log.Format == "" {
		c.Log.Format = "text"
	}
	if c.Proxy.RequestTimeout == 0 {
		c.Proxy.RequestTimeout = 60 * time.Second
	}
	if c.Proxy.IdleTimeout == 0 {
		c.Proxy.IdleTimeout = 2 * time.Hour
	}
	if c.Proxy.MaxConnsPerDevice == 0 {
		c.Proxy.MaxConnsPerDevice = 4
	}
	if c.Proxy.MaxBodyBytes == 0 {
		c.Proxy.MaxBodyBytes = 16 << 20
	}
	if c.Health.Interval == 0 {
		c.Health.Interval = 60 * time.Second
	}
	if c.Health.Timeout == 0 {
		c.Health.Timeout = 5 * time.Second
	}
	if c.UI.DefaultOpenMode == "" {
		c.UI.DefaultOpenMode = OpenEmbedded
	}
	if c.Configure.ConfirmTimeout == 0 {
		c.Configure.ConfirmTimeout = 2 * time.Minute
	}
	if c.Monitor.Retention.Minute == 0 {
		c.Monitor.Retention.Minute = 48 * time.Hour
	}
	if c.Monitor.Retention.Quarter == 0 {
		c.Monitor.Retention.Quarter = 720 * time.Hour
	}
	if c.Monitor.Retention.Hour == 0 {
		c.Monitor.Retention.Hour = 8760 * time.Hour
	}
	if c.Monitor.Retention.Events == 0 {
		c.Monitor.Retention.Events = 8760 * time.Hour
	}
}

// Load reads and validates the file at path. Environment variables override
// a few values (see ApplyEnv).
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	c := base()
	if err := yaml.Unmarshal(data, c); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	c.dir = filepath.Dir(abs)
	c.ApplyEnv(os.Getenv)
	c.applyDefaults()
	if err := c.Validate(); err != nil {
		return nil, fmt.Errorf("invalid config %s: %w", path, err)
	}
	return c, nil
}

// ApplyEnv overrides values from ERSPROXY_* environment variables. It is
// useful for containers: ERSPROXY_LISTEN, ERSPROXY_PUBLIC_URL,
// ERSPROXY_LOG_LEVEL.
func (c *Config) ApplyEnv(getenv func(string) string) {
	if v := getenv("ERSPROXY_LISTEN"); v != "" {
		c.Listen = v
	}
	if v := getenv("ERSPROXY_PUBLIC_URL"); v != "" {
		c.PublicURL = v
	}
	if v := getenv("ERSPROXY_LOG_LEVEL"); v != "" {
		c.Log.Level = v
	}
}

// Validate checks value ranges and combinations.
func (c *Config) Validate() error {
	var errs []error
	u, err := url.Parse(c.PublicURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		errs = append(errs, fmt.Errorf("public_url %q: want http(s)://host[:port]", c.PublicURL))
	} else if u.Path != "" || u.RawQuery != "" {
		errs = append(errs, fmt.Errorf("public_url %q: ERS-Proxy must be served at the root of a host, without a path", c.PublicURL))
	}
	switch c.Log.Level {
	case "debug", "info", "warn", "error":
	default:
		errs = append(errs, fmt.Errorf("log.level %q: want debug, info, warn or error", c.Log.Level))
	}
	if c.Log.Format != "text" && c.Log.Format != "json" {
		errs = append(errs, fmt.Errorf("log.format %q: want text or json", c.Log.Format))
	}
	if (c.TLS.CertFile == "") != (c.TLS.KeyFile == "") {
		errs = append(errs, errors.New("tls.cert_file and tls.key_file must be set together"))
	}
	if c.Proxy.RequestTimeout < time.Second {
		errs = append(errs, errors.New("proxy.request_timeout must be at least 1s"))
	}
	if c.Proxy.IdleTimeout < time.Minute {
		errs = append(errs, errors.New("proxy.idle_timeout must be at least 1m"))
	}
	if c.Proxy.MaxConnsPerDevice < 1 || c.Proxy.MaxConnsPerDevice > 64 {
		errs = append(errs, errors.New("proxy.max_conns_per_device must be between 1 and 64"))
	}
	if c.Proxy.MaxBodyBytes < 1<<20 {
		errs = append(errs, errors.New("proxy.max_body_bytes must be at least 1048576"))
	}
	if c.Health.Interval < 10*time.Second {
		errs = append(errs, errors.New("health.interval must be at least 10s"))
	}
	if c.Health.Timeout < time.Second || c.Health.Timeout > c.Health.Interval {
		errs = append(errs, errors.New("health.timeout must be at least 1s and not longer than health.interval"))
	}
	if !ValidOpenMode(c.UI.DefaultOpenMode) {
		errs = append(errs, fmt.Errorf("ui.default_open_mode %q: want embedded, tab or window", c.UI.DefaultOpenMode))
	}
	if c.Configure.ConfirmTimeout < 30*time.Second || c.Configure.ConfirmTimeout > 30*time.Minute {
		errs = append(errs, errors.New("configure.confirm_timeout must be between 30s and 30m"))
	}
	r := c.Monitor.Retention
	if r.Minute < time.Hour {
		errs = append(errs, errors.New("monitor.retention.minute must be at least 1h"))
	}
	if r.Quarter < r.Minute {
		errs = append(errs, errors.New("monitor.retention.quarter must not be shorter than monitor.retention.minute"))
	}
	if r.Hour < r.Quarter {
		errs = append(errs, errors.New("monitor.retention.hour must not be shorter than monitor.retention.quarter"))
	}
	if r.Events < 24*time.Hour {
		errs = append(errs, errors.New("monitor.retention.events must be at least 24h"))
	}
	return errors.Join(errs...)
}

// SecureCookies reports whether session cookies get the Secure flag.
func (c *Config) SecureCookies() bool {
	return strings.HasPrefix(c.PublicURL, "https://")
}

// Origin returns the origin of the public URL ("https://host:port"), which
// state-changing requests must send in their Origin header.
func (c *Config) Origin() string {
	u, err := url.Parse(c.PublicURL)
	if err != nil {
		return c.PublicURL
	}
	return u.Scheme + "://" + u.Host
}

// ResolvePath makes p absolute relative to the config file's directory.
func (c *Config) ResolvePath(p string) string {
	if p == "" || filepath.IsAbs(p) || c.dir == "" {
		return p
	}
	return filepath.Join(c.dir, p)
}

// Example is written by "ers-proxy config init".
const Example = `# ERS-Proxy configuration
# Devices, groups and users live in the database and are managed
# in the web UI or with the command line.

# HTTP listen address. Behind a reverse proxy, bind to localhost only.
listen: 127.0.0.1:9050

# Public URL (scheme, host, port – no path). With https://, session
# cookies are marked Secure. Must match what browsers see.
public_url: http://127.0.0.1:9050

# Proxies whose X-Real-IP/X-Forwarded-For headers are trusted.
trusted_proxies: [127.0.0.1, "::1"]

# AES-256 key for switch passwords in the database (created if missing).
# Relative paths are relative to this file.
secret_key_file: secret.key

storage:
  path: data/ers-proxy.db     # SQLite database file

log:
  level: info                 # debug, info, warn, error
  format: text                # text or json

proxy:
  request_timeout: 60s        # one request to a switch
  idle_timeout: 2h            # an opened device expires after this idle time
  max_conns_per_device: 4     # old switch web servers handle few connections
  max_body_bytes: 16777216

health:
  enabled: true               # reachability checks (status dots)
  interval: 60s
  timeout: 5s

ui:
  default_open_mode: embedded # embedded, tab or window

# Port monitoring history. Longer retention needs more disk space
# (about 100 bytes per row, see docs/configuration.md).
monitor:
  retention:
    minute: 48h               # 1-minute resolution (at least 1h)
    quarter: 720h             # 15-minute resolution (30 days)
    hour: 8760h               # 1-hour resolution (1 year)
    events: 8760h             # link changes etc. (at least 24h)

# Changing switch settings from the dashboard (port VLANs etc.). Writing to
# switches is off and can only be enabled here, explicitly: no environment
# variable turns it on. Port monitoring stays read-only either way.
# configure:
#   enabled: false
#   confirm_timeout: 2m       # a change is reverted unless confirmed (30s to 30m)

# Built-in TLS (otherwise use a reverse proxy):
# tls:
#   cert_file: /etc/ers-proxy/tls.crt
#   key_file: /etc/ers-proxy/tls.key
`
