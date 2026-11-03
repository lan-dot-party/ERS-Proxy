// Package snmp wraps gosnmp with ERS-Proxy's connection settings and the walk
// helpers shared by the monitoring drivers.
//
// The package is read-only toward devices: it offers no way to send SET
// requests (enforced by a test). Adapted from the sibling project LANLeecher.
package snmp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strconv"
	"strings"
	"time"

	"github.com/gosnmp/gosnmp"
)

// Credentials are stored encrypted in devices.snmp_credentials.
type Credentials struct {
	// Community for v2c.
	Community string `json:"community,omitempty"`

	// v3 user-based security.
	Username      string `json:"username,omitempty"`
	SecurityLevel string `json:"security_level,omitempty"` // noAuthNoPriv, authNoPriv, authPriv
	AuthProtocol  string `json:"auth_protocol,omitempty"`  // MD5, SHA, SHA-224, SHA-256, SHA-384, SHA-512
	AuthPassword  string `json:"auth_password,omitempty"`
	PrivProtocol  string `json:"priv_protocol,omitempty"` // DES, AES-128, AES-192, AES-256
	PrivPassword  string `json:"priv_password,omitempty"`
	ContextName   string `json:"context_name,omitempty"`
}

// Marshal encodes credentials for encryption.
func (c Credentials) Marshal() ([]byte, error) { return json.Marshal(c) }

// UnmarshalCredentials decodes decrypted credentials.
func UnmarshalCredentials(b []byte) (Credentials, error) {
	var c Credentials
	err := json.Unmarshal(b, &c)
	return c, err
}

// Target describes how to reach an SNMP agent.
type Target struct {
	Host    string
	Port    int
	Version string // v2c or v3
	Creds   Credentials
	Timeout time.Duration
	Retries int
}

// Versions lists the supported SNMP versions. v1 is deliberately missing: it
// has no Counter64, and a 32-bit counter wraps after ~34 s at 1 Gbit/s.
var Versions = []string{"v2c", "v3"}

// SecurityLevels for v3.
var SecurityLevels = []string{"noAuthNoPriv", "authNoPriv", "authPriv"}

// AuthProtocols for v3.
var AuthProtocols = []string{"MD5", "SHA", "SHA-224", "SHA-256", "SHA-384", "SHA-512"}

// PrivProtocols for v3.
var PrivProtocols = []string{"DES", "AES-128", "AES-192", "AES-256"}

// Validate checks that the target is usable.
func (t Target) Validate() error {
	if strings.TrimSpace(t.Host) == "" {
		return errors.New("host is required")
	}
	switch t.Version {
	case "v2c":
		if t.Creds.Community == "" {
			return errors.New("community is required for v2c")
		}
	case "v3":
		if t.Creds.Username == "" {
			return errors.New("username is required for v3")
		}
		if _, err := msgFlags(t.Creds.SecurityLevel); err != nil {
			return err
		}
		if t.Creds.SecurityLevel != "noAuthNoPriv" {
			if _, err := authProto(t.Creds.AuthProtocol); err != nil {
				return err
			}
			if len(t.Creds.AuthPassword) < 8 {
				return errors.New("v3 auth password must have at least 8 characters")
			}
		}
		if t.Creds.SecurityLevel == "authPriv" {
			if _, err := privProto(t.Creds.PrivProtocol); err != nil {
				return err
			}
			if len(t.Creds.PrivPassword) < 8 {
				return errors.New("v3 privacy password must have at least 8 characters")
			}
		}
	default:
		return fmt.Errorf("SNMP version %q is not supported (use v2c or v3)", t.Version)
	}
	return nil
}

// Dial connects to the agent. vlanContext, if > 0, selects a per-VLAN view
// (community@vlan for v2c, context "vlan-<id>" for v3) as used by Cisco switches.
func Dial(ctx context.Context, t Target, vlanContext int) (*gosnmp.GoSNMP, error) {
	if err := t.Validate(); err != nil {
		return nil, err
	}
	port := t.Port
	if port == 0 {
		port = 161
	}
	g := &gosnmp.GoSNMP{
		Context:        ctx,
		Target:         t.Host,
		Port:           uint16(port),
		Timeout:        t.Timeout,
		Retries:        t.Retries,
		MaxRepetitions: 25,
		MaxOids:        gosnmp.MaxOids,
	}
	if g.Timeout == 0 {
		g.Timeout = 3 * time.Second
	}
	switch t.Version {
	case "v2c":
		g.Version = gosnmp.Version2c
		g.Community = t.Creds.Community
		if vlanContext > 0 {
			g.Community += "@" + strconv.Itoa(vlanContext)
		}
	case "v3":
		g.Version = gosnmp.Version3
		g.SecurityModel = gosnmp.UserSecurityModel
		g.MsgFlags, _ = msgFlags(t.Creds.SecurityLevel)
		usm := &gosnmp.UsmSecurityParameters{UserName: t.Creds.Username}
		if t.Creds.SecurityLevel != "noAuthNoPriv" {
			usm.AuthenticationProtocol, _ = authProto(t.Creds.AuthProtocol)
			usm.AuthenticationPassphrase = t.Creds.AuthPassword
		}
		if t.Creds.SecurityLevel == "authPriv" {
			usm.PrivacyProtocol, _ = privProto(t.Creds.PrivProtocol)
			usm.PrivacyPassphrase = t.Creds.PrivPassword
		}
		g.SecurityParameters = usm
		g.ContextName = t.Creds.ContextName
		if vlanContext > 0 {
			g.ContextName = "vlan-" + strconv.Itoa(vlanContext)
		}
	}
	if err := g.Connect(); err != nil {
		return nil, fmt.Errorf("connect %s: %w", t.Host, err)
	}
	return g, nil
}

// Close closes the connection of g.
func Close(g *gosnmp.GoSNMP) {
	if g != nil && g.Conn != nil {
		g.Conn.Close()
	}
}

func msgFlags(level string) (gosnmp.SnmpV3MsgFlags, error) {
	switch level {
	case "noAuthNoPriv":
		return gosnmp.NoAuthNoPriv, nil
	case "authNoPriv":
		return gosnmp.AuthNoPriv, nil
	case "authPriv":
		return gosnmp.AuthPriv, nil
	}
	return 0, fmt.Errorf("unknown v3 security level %q", level)
}

func authProto(p string) (gosnmp.SnmpV3AuthProtocol, error) {
	switch p {
	case "MD5":
		return gosnmp.MD5, nil
	case "SHA":
		return gosnmp.SHA, nil
	case "SHA-224":
		return gosnmp.SHA224, nil
	case "SHA-256":
		return gosnmp.SHA256, nil
	case "SHA-384":
		return gosnmp.SHA384, nil
	case "SHA-512":
		return gosnmp.SHA512, nil
	}
	return 0, fmt.Errorf("unknown v3 auth protocol %q", p)
}

func privProto(p string) (gosnmp.SnmpV3PrivProtocol, error) {
	switch p {
	case "DES":
		return gosnmp.DES, nil
	case "AES-128":
		return gosnmp.AES, nil
	case "AES-192":
		return gosnmp.AES192, nil
	case "AES-256":
		return gosnmp.AES256, nil
	}
	return 0, fmt.Errorf("unknown v3 privacy protocol %q", p)
}

// Row is one walked value with its index below the column OID.
type Row struct {
	Index []int
	PDU   gosnmp.SnmpPDU
}

// WalkColumn walks a table column and returns rows with parsed indexes.
// A missing column ("no such object") yields no rows and no error.
func WalkColumn(g *gosnmp.GoSNMP, column string) ([]Row, error) {
	prefix := "." + strings.TrimPrefix(column, ".")
	pdus, err := g.BulkWalkAll(prefix)
	if err != nil {
		return nil, err
	}
	out := make([]Row, 0, len(pdus))
	for _, p := range pdus {
		switch p.Type {
		case gosnmp.NoSuchObject, gosnmp.NoSuchInstance, gosnmp.EndOfMibView:
			continue
		}
		if !strings.HasPrefix(p.Name, prefix+".") {
			continue
		}
		idx, err := parseIndex(p.Name[len(prefix)+1:])
		if err != nil {
			continue
		}
		out = append(out, Row{Index: idx, PDU: p})
	}
	return out, nil
}

func parseIndex(s string) ([]int, error) {
	parts := strings.Split(s, ".")
	out := make([]int, len(parts))
	for i, p := range parts {
		v, err := strconv.Atoi(p)
		if err != nil {
			return nil, err
		}
		out[i] = v
	}
	return out, nil
}

// Uint64 converts a numeric PDU value.
func Uint64(p gosnmp.SnmpPDU) (uint64, bool) {
	switch p.Type {
	case gosnmp.Counter64, gosnmp.Counter32, gosnmp.Gauge32, gosnmp.TimeTicks, gosnmp.Integer, gosnmp.Uinteger32:
		b := gosnmp.ToBigInt(p.Value)
		if b.Sign() < 0 || b.Cmp(new(big.Int).SetUint64(^uint64(0))) > 0 {
			return 0, false
		}
		return b.Uint64(), true
	}
	return 0, false
}

// Int converts a numeric PDU value to int.
func Int(p gosnmp.SnmpPDU) (int, bool) {
	v, ok := Uint64(p)
	return int(v), ok
}

// String converts an OctetString PDU to a printable string.
func String(p gosnmp.SnmpPDU) string {
	if b, ok := p.Value.([]byte); ok {
		return strings.ToValidUTF8(strings.TrimRight(string(b), "\x00"), "?")
	}
	if s, ok := p.Value.(string); ok {
		return s
	}
	return ""
}

// Bytes returns the raw OctetString value.
func Bytes(p gosnmp.SnmpPDU) []byte {
	b, _ := p.Value.([]byte)
	return b
}
