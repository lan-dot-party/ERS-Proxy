package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestExampleLoads(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(Example), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatalf("example config does not load: %v", err)
	}
	if c.Listen != "127.0.0.1:9050" || c.Proxy.IdleTimeout != 2*time.Hour || !c.Health.Enabled {
		t.Fatalf("unexpected values: %+v", c)
	}
	if got := c.ResolvePath(c.Storage.Path); got != filepath.Join(filepath.Dir(path), "data/ers-proxy.db") {
		t.Fatalf("storage path not resolved relative to the file: %s", got)
	}
}

func TestDefaultIsValid(t *testing.T) {
	if err := Default().Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestValidate(t *testing.T) {
	cases := map[string]func(*Config){
		"public_url with path":   func(c *Config) { c.PublicURL = "https://example.org/ers" },
		"public_url bad scheme":  func(c *Config) { c.PublicURL = "ftp://example.org" },
		"tls cert without key":   func(c *Config) { c.TLS.CertFile = "a.crt" },
		"open mode":              func(c *Config) { c.UI.DefaultOpenMode = "iframe" },
		"conns":                  func(c *Config) { c.Proxy.MaxConnsPerDevice = 0 },
		"health timeout too big": func(c *Config) { c.Health.Timeout = 2 * time.Minute },
		"log level":              func(c *Config) { c.Log.Level = "verbose" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			c := Default()
			mutate(c)
			if err := c.Validate(); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}

func TestEnvAndOrigin(t *testing.T) {
	c := base()
	c.ApplyEnv(func(k string) string {
		return map[string]string{"ERSPROXY_PUBLIC_URL": "https://ers.example.org/", "ERSPROXY_LISTEN": "0.0.0.0:9050"}[k]
	})
	c.applyDefaults()
	if c.Listen != "0.0.0.0:9050" || !c.SecureCookies() {
		t.Fatalf("env not applied: %+v", c)
	}
	if o := c.Origin(); o != "https://ers.example.org" {
		t.Fatalf("origin = %q", o)
	}
	if strings.HasSuffix(c.PublicURL, "/") {
		t.Fatal("trailing slash not trimmed")
	}
}

// docs/config.example.yaml (shipped in the packages) must match Example.
func TestDocsExampleInSync(t *testing.T) {
	data, err := os.ReadFile("../../docs/config.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != Example {
		t.Fatal("docs/config.example.yaml differs from config.Example; run: ers-proxy -c /tmp/x.yaml config init && cp /tmp/x.yaml docs/config.example.yaml")
	}
}

func TestMonitorRetention(t *testing.T) {
	r := Default().Monitor.Retention
	if r.Minute != 48*time.Hour || r.Quarter != 720*time.Hour || r.Hour != 8760*time.Hour || r.Events != 8760*time.Hour {
		t.Fatalf("defaults: %+v", r)
	}
	path := filepath.Join(t.TempDir(), "config.yaml")
	yaml := "monitor:\n  retention:\n    minute: 2h\n    quarter: 24h\n    hour: 48h\n    events: 36h\n"
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := c.Monitor.Retention; got.Minute != 2*time.Hour || got.Quarter != 24*time.Hour || got.Hour != 48*time.Hour || got.Events != 36*time.Hour {
		t.Fatalf("loaded: %+v", got)
	}
	bad := map[string]func(*Config){
		"minute too short": func(c *Config) { c.Monitor.Retention.Minute = 30 * time.Minute },
		"quarter < minute": func(c *Config) { c.Monitor.Retention.Quarter = 24 * time.Hour },
		"hour < quarter":   func(c *Config) { c.Monitor.Retention.Hour = 24 * time.Hour },
		"events too short": func(c *Config) { c.Monitor.Retention.Events = time.Hour },
	}
	for name, mutate := range bad {
		c := Default()
		mutate(c)
		if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "monitor.retention") {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestConfigureDefaultsAndBounds(t *testing.T) {
	c := Default()
	if c.Configure.Enabled || c.Configure.ConfirmTimeout != 2*time.Minute {
		t.Fatalf("defaults: %+v", c.Configure)
	}
	for _, d := range []time.Duration{30 * time.Second, 2 * time.Minute, 30 * time.Minute} {
		c.Configure.ConfirmTimeout = d
		if err := c.Validate(); err != nil {
			t.Errorf("%s rejected: %v", d, err)
		}
	}
	for _, d := range []time.Duration{29 * time.Second, 31 * time.Minute} {
		c.Configure.ConfirmTimeout = d
		if err := c.Validate(); err == nil {
			t.Errorf("%s accepted", d)
		}
	}
}

// Writing to switches can only be enabled in config.yaml.
func TestConfigureNotEnabledByEnv(t *testing.T) {
	c := Default()
	c.ApplyEnv(func(string) string { return "true" }) // every variable set, incl. ERSPROXY_CONFIGURE_ENABLED
	if c.Configure.Enabled {
		t.Fatal("environment enabled configure")
	}
}
