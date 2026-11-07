// Package snmpwrite sends SNMP SET requests to a switch. It is the only
// package that may call gosnmp's Set (enforced by a test); internal/snmp stays
// read-only.
//
// Each Set call is one SET PDU that the agent applies as a whole or rejects.
// It is never split and never retried: a lost answer must not lead to a second
// SET, because the first one may have been applied.
package snmpwrite

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/gosnmp/gosnmp"

	"github.com/lan-dot-party/ers-proxy/internal/monitor"
	"github.com/lan-dot-party/ers-proxy/internal/snmp"
	"github.com/lan-dot-party/ers-proxy/internal/switchcfg"
)

// setter is the part of gosnmp.GoSNMP the writer uses (a seam for tests).
type setter interface {
	Set(pdus []gosnmp.SnmpPDU) (*gosnmp.SnmpPacket, error)
}

// Writer implements switchcfg.Writer over SNMP.
type Writer struct {
	mu     sync.Mutex
	g      *gosnmp.GoSNMP // nil in tests that use a seam
	s      setter
	maxOid int
}

var _ switchcfg.Writer = (*Writer)(nil)

// SetError is returned when the agent answers a SET with an error status.
type SetError struct {
	Status gosnmp.SNMPError
	// Index is the 1-based position of the failing varbind (0 = unknown).
	Index int
	// Instance is the "name.index" of that varbind, if known.
	Instance string
}

func (e *SetError) Error() string {
	s := fmt.Sprintf("SNMP SET rejected: %s (error index %d)", e.Status, e.Index)
	if e.Instance != "" {
		s += " at " + e.Instance
	}
	return s
}

// Open connects to the agent with the write credentials in t. Close the
// writer when done.
func Open(ctx context.Context, t snmp.Target) (*Writer, error) {
	g, err := snmp.Dial(ctx, t, 0)
	if err != nil {
		return nil, err
	}
	// A lost answer must not cause a second SET.
	g.Retries = 0
	return &Writer{g: g, s: g, maxOid: g.MaxOids}, nil
}

// Close ends the session.
func (w *Writer) Close() {
	if w.g != nil {
		snmp.Close(w.g)
	}
}

// pdus builds the varbinds of one SET. It never splits: more assignments than
// maxOids is an error.
func pdus(as []switchcfg.Assignment, maxOids int) ([]gosnmp.SnmpPDU, error) {
	if len(as) == 0 {
		return nil, errors.New("nothing to set")
	}
	if maxOids > 0 && len(as) > maxOids {
		return nil, fmt.Errorf("step has %d assignments, more than the %d an SNMP SET may carry", len(as), maxOids)
	}
	out := make([]gosnmp.SnmpPDU, 0, len(as))
	for _, a := range as {
		base, ok := switchcfg.OID(a.Name)
		if !ok {
			return nil, fmt.Errorf("unknown MIB object %q", a.Name)
		}
		idx := a.Index
		if idx == "" {
			idx = "0"
		}
		p := gosnmp.SnmpPDU{Name: "." + base + "." + idx}
		switch a.Value.Kind {
		case monitor.KindInt:
			p.Type, p.Value = gosnmp.Integer, int(a.Value.Int)
		case monitor.KindUint:
			p.Type, p.Value = gosnmp.Gauge32, uint(a.Value.Uint)
		case monitor.KindBytes:
			p.Type, p.Value = gosnmp.OctetString, append([]byte(nil), a.Value.Bytes...)
		case monitor.KindOID:
			p.Type, p.Value = gosnmp.ObjectIdentifier, a.Value.OID
		default:
			return nil, fmt.Errorf("%s has no value", a.Instance())
		}
		out = append(out, p)
	}
	return out, nil
}

// Set sends the assignments as one SET PDU. A transport error does not tell
// whether the agent applied the request; the caller must read back.
func (w *Writer) Set(ctx context.Context, as []switchcfg.Assignment) error {
	vb, err := pdus(as, w.maxOid)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.g != nil {
		w.g.Context = ctx
	}
	res, err := w.s.Set(vb)
	if err != nil {
		return fmt.Errorf("SNMP SET: %w", err)
	}
	if res == nil {
		return errors.New("SNMP SET: no response")
	}
	if res.Error != gosnmp.NoError {
		e := &SetError{Status: res.Error, Index: int(res.ErrorIndex)}
		if e.Index >= 1 && e.Index <= len(as) {
			e.Instance = as[e.Index-1].Instance()
		}
		return e
	}
	return nil
}
