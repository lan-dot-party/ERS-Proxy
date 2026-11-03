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
	if testing.Short() {
		tb.Skip("SNMP simulator test skipped with -short")
	}
	bin := binary()
	if bin == "" {
		tb.Skip("snmpsim-command-responder not found (python3 -m venv .venv && .venv/bin/pip install snmpsim pysmi, or set ERSPROXY_SNMPSIM)")
	}
	port := freeUDPPort(tb)
	cmd := exec.Command(bin, "--data-dir="+dataDir, "--agent-udpv4-endpoint=127.0.0.1:"+strconv.Itoa(port))
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	if err := cmd.Start(); err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })

	target := snmp.Target{Host: "127.0.0.1", Port: port, Version: "v2c", Creds: snmp.Credentials{Community: community},
		Timeout: time.Second}
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
