// Package devices validates devices and handles import, export and stored
// credentials. Persistence lives in internal/store.
package devices

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"unicode/utf8"

	"github.com/lan-dot-party/ers-proxy/internal/config"
	"github.com/lan-dot-party/ers-proxy/internal/secret"
	"github.com/lan-dot-party/ers-proxy/internal/snmp"
	"github.com/lan-dot-party/ers-proxy/internal/store"
)

// Field errors carry a key for the UI (internal/web/i18n.go: "device.err.<key>").
type FieldError struct {
	Key string
	Err error
}

func (e *FieldError) Error() string { return e.Err.Error() }

func fieldErr(key, format string, args ...any) error {
	return &FieldError{Key: key, Err: fmt.Errorf(format, args...)}
}

// Monitoring interval limits in seconds.
const (
	DefaultMonitorInterval = 60
	MinMonitorInterval     = 30
	MinEDMInterval         = 60
	MaxMonitorInterval     = 3600
)

// Normalize trims and validates a device in place. Unset modes get defaults.
func Normalize(d *store.Device) error {
	d.Name = strings.TrimSpace(d.Name)
	if d.Name == "" || utf8.RuneCountInString(d.Name) > 80 {
		return fieldErr("name", "name must have 1 to 80 characters")
	}
	d.Notes = strings.TrimSpace(d.Notes)
	if utf8.RuneCountInString(d.Notes) > 1000 {
		return fieldErr("notes", "notes must not exceed 1000 characters")
	}
	if d.OpenMode == "" {
		d.OpenMode = store.OpenInherit
	}
	if d.OpenMode != store.OpenInherit && !config.ValidOpenMode(d.OpenMode) {
		return fieldErr("open_mode", "open mode %q: want inherit, embedded, tab or window", d.OpenMode)
	}
	if d.Isolation == "" {
		d.Isolation = store.IsolationSandbox
	}
	if d.Isolation != store.IsolationSandbox && d.Isolation != store.IsolationOff {
		return fieldErr("isolation", "isolation %q: want sandbox or off", d.Isolation)
	}
	if d.TLSMode == "" {
		d.TLSMode = store.TLSAuto
	}
	switch d.TLSMode {
	case store.TLSAuto, store.TLSModern, store.TLSLegacy:
	default:
		return fieldErr("tls_mode", "TLS mode %q: want auto, modern or legacy", d.TLSMode)
	}
	d.TLSPin = strings.ToLower(strings.ReplaceAll(strings.TrimSpace(d.TLSPin), ":", ""))
	if d.TLSPin != "" && (len(d.TLSPin) != 64 || strings.Trim(d.TLSPin, "0123456789abcdef") != "") {
		return fieldErr("tls_pin", "certificate fingerprint must be 64 hex characters (SHA-256)")
	}
	if err := normalizeMonitoring(d); err != nil {
		return err
	}
	switch d.Kind {
	case store.KindERS, store.KindVSP:
		u, err := normalizeBase(d.URL)
		if err != nil {
			return err
		}
		d.URL = u
	case store.KindEDMDirect:
		u, err := normalizeBase(d.URL)
		if err != nil {
			return err
		}
		d.URL = u
		// Not proxied: isolation and auto-login do not apply.
		d.Isolation, d.AutoLogin = store.IsolationSandbox, false
	case store.KindLink:
		u, err := normalizeLink(d.URL)
		if err != nil {
			return err
		}
		d.URL = u
		// Isolation and TLS settings only apply to proxied devices.
		d.Isolation, d.TLSMode, d.TLSPin = store.IsolationSandbox, store.TLSAuto, ""
	default:
		return fieldErr("kind", "kind %q: want ers-edm, vsp-edm, edm-direct or link", d.Kind)
	}
	return nil
}

// normalizeMonitoring validates the monitoring settings and fills defaults.
// Kind is checked by the caller; the EDM source needs an EDM switch.
func normalizeMonitoring(d *store.Device) error {
	if d.MonitorSource == "" {
		d.MonitorSource = store.MonitorNone
	}
	switch d.MonitorSource {
	case store.MonitorNone, store.MonitorSNMP:
	case store.MonitorEDM:
		if !d.EDM() {
			return fieldErr("monitor_source", "the EDM data source needs a device of kind ers-edm, vsp-edm or edm-direct")
		}
	default:
		return fieldErr("monitor_source", "monitor source %q: want none, snmp or edm", d.MonitorSource)
	}
	if d.MonitorDriver == "" {
		d.MonitorDriver = store.DriverAuto
	}
	switch d.MonitorDriver {
	case store.DriverAuto, store.DriverGeneric, store.DriverERS, store.DriverCisco, store.DriverTPLink, store.DriverVOSS:
	default:
		return fieldErr("monitor_driver", "monitor driver %q: want auto, generic, ers, cisco, tplink or voss", d.MonitorDriver)
	}
	if d.MonitorInterval == 0 {
		d.MonitorInterval = DefaultMonitorInterval
	}
	if d.MonitorInterval < MinMonitorInterval || d.MonitorInterval > MaxMonitorInterval {
		return fieldErr("monitor_interval", "monitor interval must be 30 to 3600 seconds")
	}
	if d.MonitorSource == store.MonitorEDM && d.MonitorInterval < MinEDMInterval {
		return fieldErr("monitor_interval", "the EDM data source needs an interval of at least 60 seconds")
	}
	if d.SNMPPort == 0 {
		d.SNMPPort = 161
	}
	if d.SNMPPort < 1 || d.SNMPPort > 65535 {
		return fieldErr("snmp_port", "SNMP port must be 1 to 65535")
	}
	if d.SNMPVersion == "" {
		d.SNMPVersion = store.SNMPv2c
	}
	if d.SNMPVersion != store.SNMPv2c && d.SNMPVersion != store.SNMPv3 {
		return fieldErr("snmp_version", "SNMP version %q: want v2c or v3", d.SNMPVersion)
	}
	return nil
}

func parseHTTPURL(raw string) (*url.URL, error) {
	raw = strings.TrimSpace(raw)
	if raw != "" && !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.Hostname() == "" {
		return nil, fieldErr("url", "address %q: want http(s)://host[:port]", raw)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fieldErr("url", "address %q: only http and https are supported", raw)
	}
	if u.User != nil {
		return nil, fieldErr("url", "do not put user names or passwords into the address")
	}
	if p := u.Port(); p != "" {
		var n int
		if _, err := fmt.Sscanf(p, "%d", &n); err != nil || n < 1 || n > 65535 {
			return nil, fieldErr("url", "port %q out of range", p)
		}
	}
	u.Scheme = strings.ToLower(u.Scheme)
	u.Host = strings.ToLower(u.Host)
	return u, nil
}

// normalizeBase returns scheme://host[:port] for a proxied switch. A path is
// not allowed: EDM always lives at the root of the switch's web server.
func normalizeBase(raw string) (string, error) {
	u, err := parseHTTPURL(raw)
	if err != nil {
		return "", err
	}
	if (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return "", fieldErr("url", "the switch address must not contain a path")
	}
	return u.Scheme + "://" + u.Host, nil
}

func normalizeLink(raw string) (string, error) {
	u, err := parseHTTPURL(raw)
	if err != nil {
		return "", err
	}
	if u.Path == "" {
		u.Path = "/"
	}
	return u.String(), nil
}

// HostPort returns host and port of a device URL, with the default port of
// the scheme filled in.
func HostPort(raw string) (host, port string) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", ""
	}
	port = u.Port()
	if port == "" {
		port = "80"
		if u.Scheme == "https" {
			port = "443"
		}
	}
	return u.Hostname(), port
}

func scheme(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Scheme
}

// Origin returns scheme://host[:port] of a URL (for CSP frame-src).
func Origin(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return ""
	}
	return u.Scheme + "://" + u.Host
}

// AAD is the associated data that binds stored credentials to a device and
// its address. Changing the host or port makes them unusable.
func AAD(d *store.Device) []byte {
	host, port := HostPort(d.URL)
	return []byte("ers-proxy device|" + fmt.Sprint(d.ID) + "|" + host + "|" + port)
}

// Credentials is the login for auto-login.
type Credentials struct {
	Username string `json:"u"`
	Password string `json:"p"`
}

// EncryptCredentials encrypts a login for d. d.ID must be set.
func EncryptCredentials(k *secret.Key, d *store.Device, c Credentials) ([]byte, error) {
	if d.ID == 0 {
		return nil, errors.New("device must be saved before credentials are stored")
	}
	plain, err := json.Marshal(c)
	if err != nil {
		return nil, err
	}
	return k.Encrypt(plain, AAD(d))
}

// DecryptCredentials opens the login stored for d.
func DecryptCredentials(k *secret.Key, d *store.Device) (Credentials, error) {
	var c Credentials
	if !d.HasCredentials() {
		return c, errors.New("no credentials stored")
	}
	plain, err := k.Decrypt(d.Credentials, AAD(d))
	if err != nil {
		return c, err
	}
	err = json.Unmarshal(plain, &c)
	return c, err
}

// SNMPAAD is the associated data that binds stored SNMP credentials to a
// device, its host and its SNMP port.
func SNMPAAD(d *store.Device) []byte {
	host, _ := HostPort(d.URL)
	return []byte("ers-proxy snmp|" + fmt.Sprint(d.ID) + "|" + host + "|" + fmt.Sprint(d.SNMPPort))
}

// EncryptSNMP encrypts SNMP credentials for d. d.ID must be set.
func EncryptSNMP(k *secret.Key, d *store.Device, c snmp.Credentials) ([]byte, error) {
	if d.ID == 0 {
		return nil, errors.New("device must be saved before SNMP credentials are stored")
	}
	plain, err := c.Marshal()
	if err != nil {
		return nil, err
	}
	return k.Encrypt(plain, SNMPAAD(d))
}

// DecryptSNMP opens the SNMP credentials stored for d.
func DecryptSNMP(k *secret.Key, d *store.Device) (snmp.Credentials, error) {
	if !d.HasSNMPCredentials() {
		return snmp.Credentials{}, errors.New("no SNMP credentials stored")
	}
	plain, err := k.Decrypt(d.SNMPCredentials, SNMPAAD(d))
	if err != nil {
		return snmp.Credentials{}, err
	}
	return snmp.UnmarshalCredentials(plain)
}

// AddressChanged reports whether an update moves the device to another host
// or port (stored credentials and the certificate pin must then be dropped).
func AddressChanged(before, after *store.Device) bool {
	h1, p1 := HostPort(before.URL)
	h2, p2 := HostPort(after.URL)
	return h1 != h2 || p1 != p2 || scheme(before.URL) != scheme(after.URL)
}
