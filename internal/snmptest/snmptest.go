// Package snmptest starts an snmpsim agent for tests that need a simulated
// SNMP device. The tests are skipped when snmpsim is not installed.
package snmptest

import (
	"context"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/lan-dot-party/ers-proxy/internal/snmp"
)

// repoRoot walks up from the working directory to the directory with go.mod.
func repoRoot() string {
	dir, err := os.Getwd()
	if err != nil {
		return ""
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

// binary finds snmpsim-command-responder: $ERSPROXY_SNMPSIM, the repository's
// .venv, or PATH. It returns "" when none exists.
func binary() string {
	if p := os.Getenv("ERSPROXY_SNMPSIM"); p != "" {
		return p
	}
	if root := repoRoot(); root != "" {
		p := filepath.Join(root, ".venv", "bin", "snmpsim-command-responder")
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	p, _ := exec.LookPath("snmpsim-command-responder")
	return p
}

// DataDir returns <repo>/testdata/snmpsim.
func DataDir(tb testing.TB) string {
	tb.Helper()
	root := repoRoot()
	if root == "" {
		tb.Fatal("repository root (go.mod) not found")
	}
	return filepath.Join(root, "testdata", "snmpsim")
}

func freeUDPPort(tb testing.TB) int {
	tb.Helper()
	c, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		tb.Fatal(err)
	}
	defer c.Close()
	return c.LocalAddr().(*net.UDPAddr).Port
}

// Simulator starts snmpsim with the .snmprec files in dataDir (the file name
// without extension is the v2c community) and returns a target for the
// community. The test is skipped when snmpsim is missing or with -short.
func Simulator(tb testing.TB, dataDir, community string) snmp.Target {
	tb.Helper()
	return start(tb, dataDir, nil, snmp.Target{Version: "v2c", Creds: snmp.Credentials{Community: community}})
}

// v3Names maps this repository's protocol names to snmpsim's.
var (
	v3Auth = map[string]string{"MD5": "MD5", "SHA": "SHA", "SHA-224": "SHA224", "SHA-256": "SHA256", "SHA-384": "SHA384", "SHA-512": "SHA512"}
	v3Priv = map[string]string{"DES": "DES", "AES-128": "AES128", "AES-192": "AES192", "AES-256": "AES256"}
)

// SimulatorV3 starts snmpsim with a single SNMPv3 user (creds, with the
// protocols named as in package snmp) and returns a target that reads
// dataFile (a .snmprec file name without extension; snmpsim selects the data
// file by the v3 context name, which is set on the returned target). The
// credentials' passwords must be longer than 8 characters. The test is
// skipped when snmpsim is missing or with -short.
func SimulatorV3(tb testing.TB, dataDir, dataFile string, creds snmp.Credentials) snmp.Target {
	tb.Helper()
	args := []string{"--v3-user=" + creds.Username}
	if creds.SecurityLevel != "noAuthNoPriv" {
		args = append(args, "--v3-auth-proto="+v3Auth[creds.AuthProtocol], "--v3-auth-key="+creds.AuthPassword)
	}
	if creds.SecurityLevel == "authPriv" {
		args = append(args, "--v3-priv-proto="+v3Priv[creds.PrivProtocol], "--v3-priv-key="+creds.PrivPassword)
	}
	creds.ContextName = dataFile
	return start(tb, dataDir, args, snmp.Target{Version: "v3", Creds: creds})
}

// start launches snmpsim with extra arguments, fills in host, port and
// timeout of base and waits until the agent answers a sysName request.
func start(tb testing.TB, dataDir string, extra []string, base snmp.Target) snmp.Target {
	tb.Helper()
	if testing.Short() {
		tb.Skip("SNMP simulator test skipped with -short")
	}
	bin := binary()
	if bin == "" {
		tb.Skip("snmpsim-command-responder not found (python3 -m venv .venv && .venv/bin/pip install snmpsim pysmi, or set ERSPROXY_SNMPSIM)")
	}
	port := freeUDPPort(tb)
	args := append([]string{"--data-dir=" + dataDir, "--agent-udpv4-endpoint=127.0.0.1:" + strconv.Itoa(port)}, extra...)
	cmd := exec.Command(bin, args...)
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	if err := cmd.Start(); err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })

	target := base
	target.Host, target.Port, target.Timeout = "127.0.0.1", port, time.Second
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		g, err := snmp.Dial(context.Background(), target, 0)
		if err == nil {
			_, err = g.Get([]string{"1.3.6.1.2.1.1.5.0"})
			snmp.Close(g)
			if err == nil {
				return target
			}
		}
		time.Sleep(300 * time.Millisecond)
	}
	tb.Fatal("snmpsim did not start")
	return target
}
