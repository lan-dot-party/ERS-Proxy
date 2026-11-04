package snmp_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/lan-dot-party/ers-proxy/internal/snmp"
	"github.com/lan-dot-party/ers-proxy/internal/snmptest"
)

// checkRead reads sysName and walks ifDescr of the "basic" data file.
func checkRead(t *testing.T, target snmp.Target) {
	t.Helper()
	g, err := snmp.Dial(context.Background(), target, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer snmp.Close(g)
	res, err := g.Get([]string{"1.3.6.1.2.1.1.5.0"})
	if err != nil || len(res.Variables) != 1 {
		t.Fatalf("get sysName: %v %+v", err, res)
	}
	if got := snmp.String(res.Variables[0]); got != "sim-switch" {
		t.Fatalf("sysName = %q", got)
	}
	rows, err := snmp.WalkColumn(g, "1.3.6.1.2.1.2.2.1.2")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || snmp.String(rows[0].PDU) != "Port 1" || rows[1].Index[0] != 2 {
		t.Fatalf("ifDescr rows: %+v", rows)
	}
}

// TestV3Simulator reads through SNMPv3 from snmpsim; snmpsim maps the v3
// context name to the data file. Verified: noAuthNoPriv and authNoPriv with
// MD5, SHA and SHA-256. The authPriv combinations are skipped by default:
// snmpsim 1.2.2 / pysnmp 7.1.31 cannot decrypt requests of any client (net-snmp's
// snmpget fails with "Decryption error" as well, for DES and AES128 and all
// auth protocols), so the failure is on the simulator side. Set
// ERSPROXY_SNMPSIM_PRIV=1 to run them against a fixed simulator.
func TestV3Simulator(t *testing.T) {
	const pw1, pw2 = "auth-password-1", "priv-password-1"
	cases := []struct {
		name  string
		creds snmp.Credentials
	}{
		{"noAuthNoPriv", snmp.Credentials{SecurityLevel: "noAuthNoPriv"}},
		{"authNoPriv-MD5", snmp.Credentials{SecurityLevel: "authNoPriv", AuthProtocol: "MD5", AuthPassword: pw1}},
		{"authNoPriv-SHA", snmp.Credentials{SecurityLevel: "authNoPriv", AuthProtocol: "SHA", AuthPassword: pw1}},
		{"authNoPriv-SHA-256", snmp.Credentials{SecurityLevel: "authNoPriv", AuthProtocol: "SHA-256", AuthPassword: pw1}},
		{"authPriv-SHA-AES-128", snmp.Credentials{SecurityLevel: "authPriv", AuthProtocol: "SHA", AuthPassword: pw1, PrivProtocol: "AES-128", PrivPassword: pw2}},
		{"authPriv-SHA-256-AES-128", snmp.Credentials{SecurityLevel: "authPriv", AuthProtocol: "SHA-256", AuthPassword: pw1, PrivProtocol: "AES-128", PrivPassword: pw2}},
		{"authPriv-MD5-DES", snmp.Credentials{SecurityLevel: "authPriv", AuthProtocol: "MD5", AuthPassword: pw1, PrivProtocol: "DES", PrivPassword: pw2}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.creds.SecurityLevel == "authPriv" && os.Getenv("ERSPROXY_SNMPSIM_PRIV") == "" {
				t.Skip("snmpsim 1.2.2 cannot decrypt authPriv requests (also fails with net-snmp); set ERSPROXY_SNMPSIM_PRIV=1 to try")
			}
			c.creds.Username = "v3user"
			target := snmptest.SimulatorV3(t, snmptest.DataDir(t), "basic", c.creds)
			checkRead(t, target)
		})
	}
}

// TestV3WrongAuthPassword expects an error, within the client timeout, when
// the authentication password differs from the agent's.
func TestV3WrongAuthPassword(t *testing.T) {
	good := snmp.Credentials{Username: "v3user", SecurityLevel: "authNoPriv", AuthProtocol: "SHA", AuthPassword: "auth-password-1"}
	target := snmptest.SimulatorV3(t, snmptest.DataDir(t), "basic", good)
	target.Creds.AuthPassword = "wrong-password-1"
	target.Timeout = time.Second
	start := time.Now()
	g, err := snmp.Dial(context.Background(), target, 0)
	if err == nil {
		defer snmp.Close(g)
		_, err = g.Get([]string{"1.3.6.1.2.1.1.5.0"})
	}
	if err == nil {
		t.Fatal("request with wrong auth password succeeded")
	}
	if d := time.Since(start); d > 15*time.Second {
		t.Fatalf("failure took %v", d)
	}
}
