package switchcfg_test

import (
	"bytes"
	"context"
	"testing"

	"github.com/lan-dot-party/ers-proxy/internal/monitor"
	"github.com/lan-dot-party/ers-proxy/internal/switchcfg"
	"github.com/lan-dot-party/ers-proxy/internal/switchcfg/switchcfgtest"
)

func TestSetBitHasBit(t *testing.T) {
	b := switchcfg.SetBit(nil, 0, true, 0)
	if !bytes.Equal(b, []byte{0x80}) {
		t.Errorf("bit 0 = %x", b)
	}
	b = switchcfg.SetBit(b, 17, true, 0)
	if !bytes.Equal(b, []byte{0x80, 0x00, 0x40}) {
		t.Errorf("grown = %x", b)
	}
	if !switchcfg.HasBit(b, 0) || !switchcfg.HasBit(b, 17) || switchcfg.HasBit(b, 1) {
		t.Error("HasBit")
	}
	if switchcfg.HasBit(b, 100) || switchcfg.HasBit(b, -1) {
		t.Error("HasBit out of range")
	}
	in := []byte{0xff}
	out := switchcfg.SetBit(in, 3, false, 4)
	if len(out) != 4 || out[0] != 0xef {
		t.Errorf("cleared and padded = %x", out)
	}
	if in[0] != 0xff {
		t.Error("input was modified")
	}
	if got := switchcfg.SetBit(nil, 2, false, 0); !bytes.Equal(got, []byte{0}) {
		t.Errorf("clear on empty = %x", got)
	}
}

func TestVerify(t *testing.T) {
	ctx := context.Background()
	sw := switchcfgtest.New()
	sw.Put("ifAdminStatus", "1", monitor.Value{Kind: monitor.KindUint, Uint: 1})
	sw.Put("ifAlias", "1", monitor.Value{Kind: monitor.KindBytes, Bytes: []byte("uplink  ")})
	sw.Put("rcVlanPortMembers", "10", monitor.Value{Kind: monitor.KindBytes, Bytes: []byte{0x80, 0, 0, 0}})
	sw.Put("rcSysAction", "", switchcfg.Int(9))

	ok := []switchcfg.Assignment{
		{Name: "ifAdminStatus", Index: "1", Value: switchcfg.Int(1)},
		{Name: "ifAlias", Index: "1", Value: switchcfg.Text("uplink")},
		{Name: "rcVlanPortMembers", Index: "10", Value: switchcfg.Octets([]byte{0x80})},
		{Name: "rcSysAction", Value: switchcfg.Int(9)},
		{Name: "rcVlanName", Index: "99", Value: switchcfg.Absent},
	}
	ms, err := switchcfg.Verify(ctx, sw, ok)
	if err != nil || len(ms) != 0 {
		t.Fatalf("Verify = %v %v", ms, err)
	}

	bad := []switchcfg.Assignment{
		{Name: "ifAdminStatus", Index: "1", Value: switchcfg.Int(2)},
		{Name: "ifAlias", Index: "1", Value: switchcfg.Text("other")},
		{Name: "ifAlias", Index: "2", Value: switchcfg.Text("x")},
		{Name: "ifAdminStatus", Index: "1", Value: switchcfg.Absent},
		{Name: "rcSysAction", Value: switchcfg.Int(8)},
	}
	ms, err = switchcfg.Verify(ctx, sw, bad)
	if err != nil || len(ms) != len(bad) {
		t.Fatalf("Verify = %v %v", ms, err)
	}
	if switchcfg.MismatchError(ms) == nil {
		t.Error("MismatchError returned nil")
	}

	sw.SetReachable(false)
	if _, err := switchcfg.Verify(ctx, sw, ok); err == nil {
		t.Error("no error for an unreachable switch")
	}
}
