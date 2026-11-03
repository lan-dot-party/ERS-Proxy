// Package upstream connects ERS-Proxy to a device: HTTP or HTTPS with the old
// TLS versions that ERS switches speak, certificate pinning instead of chain
// validation, a tolerant response parser and a per-device connection limit.
package upstream

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"time"
)

// TLS profiles.
const (
	// ProfileModern is TLS 1.2 and 1.3 with Go's default cipher suites.
	ProfileModern = "modern"
	// ProfileLegacy is TLS 1.0 to 1.2 with RSA key exchange and old ciphers
	// (AES-CBC, 3DES, RC4). Firmware of ERS switches often supports nothing else.
	ProfileLegacy = "legacy"
)

// legacySuites in order of preference. Go only offers RSA key exchange, 3DES
// and RC4 when they are listed explicitly (they are not in the default list).
var legacySuites = []uint16{
	tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
	tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,
	tls.TLS_ECDHE_RSA_WITH_AES_128_CBC_SHA,
	tls.TLS_ECDHE_RSA_WITH_AES_256_CBC_SHA,
	tls.TLS_RSA_WITH_AES_128_GCM_SHA256,
	tls.TLS_RSA_WITH_AES_256_GCM_SHA384,
	tls.TLS_RSA_WITH_AES_128_CBC_SHA256,
	tls.TLS_RSA_WITH_AES_128_CBC_SHA,
	tls.TLS_RSA_WITH_AES_256_CBC_SHA,
	tls.TLS_RSA_WITH_3DES_EDE_CBC_SHA,
	tls.TLS_RSA_WITH_RC4_128_SHA,
}

// ErrNotPinned is returned when a certificate has not been confirmed yet.
var ErrNotPinned = errors.New("the certificate of this device has not been confirmed yet")

// PinMismatchError is returned when a device presents another certificate
// than the pinned one.
type PinMismatchError struct {
	Want, Got string
}

func (e *PinMismatchError) Error() string {
	return fmt.Sprintf("certificate changed: pinned %s…, device presented %s…", short(e.Want), short(e.Got))
}

func short(s string) string {
	if len(s) > 16 {
		return s[:16]
	}
	return s
}

// Fingerprint returns the hex SHA-256 of a DER certificate.
func Fingerprint(der []byte) string {
	sum := sha256.Sum256(der)
	return hex.EncodeToString(sum[:])
}

// TLSConfig returns the client configuration of a profile. Chain validation is
// replaced by pinning: switches use self-signed certificates. With an empty
// pin every handshake fails with ErrNotPinned (only Probe connects without one).
func TLSConfig(profile, serverName, pin string) *tls.Config {
	c := &tls.Config{
		ServerName:         serverName,
		InsecureSkipVerify: true, //nolint:gosec // replaced by VerifyConnection (pinning)
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 {
				return errors.New("device sent no certificate")
			}
			got := Fingerprint(cs.PeerCertificates[0].Raw)
			if pin == "" {
				return ErrNotPinned
			}
			if got != pin {
				return &PinMismatchError{Want: pin, Got: got}
			}
			return nil
		},
	}
	if net.ParseIP(serverName) != nil {
		c.ServerName = "" // no SNI for IP addresses
	}
	if profile == ProfileLegacy {
		c.MinVersion = tls.VersionTLS10
		c.MaxVersion = tls.VersionTLS12
		c.CipherSuites = legacySuites
	} else {
		c.MinVersion = tls.VersionTLS12
	}
	return c
}

// IsHandshakeIncompatibility reports whether err means "these two TLS stacks
// do not agree" (try the legacy profile), as opposed to network errors or a
// certificate problem.
func IsHandshakeIncompatibility(err error) bool {
	if err == nil {
		return false
	}
	var pm *PinMismatchError
	if errors.Is(err, ErrNotPinned) || errors.As(err, &pm) {
		return false
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return false
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) && opErr.Op == "dial" {
		return false
	}
	// Alerts (protocol_version, handshake_failure), EOF or reset during the
	// handshake, unsupported versions or suites.
	return true
}

// Certificate describes the certificate a device presented.
type Certificate struct {
	Fingerprint string
	Subject     string
	Issuer      string
	NotAfter    time.Time
	KeyBits     int
	SelfSigned  bool
}

func describe(cert *x509.Certificate) Certificate {
	c := Certificate{Fingerprint: Fingerprint(cert.Raw), Subject: cert.Subject.String(), Issuer: cert.Issuer.String(),
		NotAfter: cert.NotAfter, SelfSigned: cert.Subject.String() == cert.Issuer.String()}
	if k, ok := cert.PublicKey.(interface{ Size() int }); ok {
		c.KeyBits = k.Size() * 8
	}
	return c
}

// Attempt is one handshake of a probe.
type Attempt struct {
	Profile string
	Version string // e.g. "TLS 1.0"
	Cipher  string
	Err     error
}

// ProbeResult is the outcome of Probe.
type ProbeResult struct {
	Attempts []Attempt
	// Best is the first successful attempt in preference order (nil if none).
	Best *Attempt
	Cert *Certificate
}

// Probe connects to addr (host:port) with each TLS version of both profiles
// and reports what the device accepts, without checking the certificate. It
// is used to show the fingerprint for confirmation and by "ers-proxy tls-probe".
func Probe(ctx context.Context, addr, serverName string, timeout time.Duration) *ProbeResult {
	res := &ProbeResult{}
	steps := []struct {
		profile string
		version uint16
	}{
		{ProfileModern, tls.VersionTLS13},
		{ProfileModern, tls.VersionTLS12},
		{ProfileLegacy, tls.VersionTLS12},
		{ProfileLegacy, tls.VersionTLS11},
		{ProfileLegacy, tls.VersionTLS10},
	}
	for _, st := range steps {
		cfg := TLSConfig(st.profile, serverName, "")
		cfg.VerifyConnection = nil
		cfg.MinVersion, cfg.MaxVersion = st.version, st.version
		a := Attempt{Profile: st.profile, Version: tls.VersionName(st.version)}
		cs, err := handshake(ctx, addr, cfg, timeout)
		if err != nil {
			a.Err = err
		} else {
			a.Cipher = tls.CipherSuiteName(cs.CipherSuite)
			if res.Cert == nil && len(cs.PeerCertificates) > 0 {
				c := describe(cs.PeerCertificates[0])
				res.Cert = &c
			}
		}
		res.Attempts = append(res.Attempts, a)
		if a.Err == nil && res.Best == nil {
			best := a
			res.Best = &best
		}
		var opErr *net.OpError
		if errors.As(err, &opErr) && opErr.Op == "dial" {
			break // host unreachable: no point in trying other versions
		}
	}
	return res
}

func handshake(ctx context.Context, addr string, cfg *tls.Config, timeout time.Duration) (tls.ConnectionState, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	d := &net.Dialer{Timeout: timeout}
	raw, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return tls.ConnectionState{}, err
	}
	defer raw.Close()
	conn := tls.Client(raw, cfg)
	if err := conn.HandshakeContext(ctx); err != nil {
		return tls.ConnectionState{}, err
	}
	return conn.ConnectionState(), nil
}
