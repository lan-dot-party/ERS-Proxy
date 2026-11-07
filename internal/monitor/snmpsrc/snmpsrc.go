// Package snmpsrc implements monitor.Reader over SNMP (GET and walks only).
package snmpsrc

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/gosnmp/gosnmp"

	"github.com/lan-dot-party/ers-proxy/internal/monitor"
	"github.com/lan-dot-party/ers-proxy/internal/snmp"
)

// Reader reads MIB objects from one SNMP session.
type Reader struct {
	g *gosnmp.GoSNMP
}

// Open connects to an agent. Close the reader when done.
func Open(ctx context.Context, t snmp.Target) (*Reader, error) {
	g, err := snmp.Dial(ctx, t, 0)
	if err != nil {
		return nil, err
	}
	return &Reader{g: g}, nil
}

// Close ends the session.
func (r *Reader) Close() { snmp.Close(r.g) }

func oidOf(name string) (string, error) {
	oid, ok := monitor.Objects[name]
	if !ok {
		return "", fmt.Errorf("unknown MIB object %q", name)
	}
	return oid, nil
}

// maxGetOIDs is the most objects asked for in one GET. Grandstream GWN
// switches (GWN7721, firmware 1.0.7) silently drop requests with more than
// four variable bindings.
const maxGetOIDs = 4

// Scalars implements monitor.Reader.
func (r *Reader) Scalars(ctx context.Context, names ...string) (map[string]monitor.Value, error) {
	out := map[string]monitor.Value{}
	byOID := map[string]string{}
	var oids []string
	for _, n := range names {
		oid, err := oidOf(n)
		if err != nil {
			return nil, err
		}
		full := oid + ".0"
		byOID[full] = n
		oids = append(oids, full)
	}
	for len(oids) > 0 {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		chunk := oids
		if len(chunk) > maxGetOIDs {
			chunk = chunk[:maxGetOIDs]
		}
		oids = oids[len(chunk):]
		res, err := r.g.Get(chunk)
		if err != nil {
			return nil, err
		}
		for _, p := range res.Variables {
			if v, ok := convert(p); ok {
				out[byOID[strings.TrimPrefix(p.Name, ".")]] = v
			}
		}
	}
	return out, nil
}

// Columns implements monitor.Reader.
func (r *Reader) Columns(ctx context.Context, names ...string) (monitor.Table, error) {
	t := monitor.Table{}
	for _, n := range names {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		oid, err := oidOf(n)
		if err != nil {
			return nil, err
		}
		rows, err := snmp.WalkColumn(r.g, oid)
		if err != nil {
			return nil, fmt.Errorf("walk %s: %w", n, err)
		}
		for _, row := range rows {
			v, ok := convert(row.PDU)
			if !ok {
				continue
			}
			parts := make([]string, len(row.Index))
			for i, x := range row.Index {
				parts[i] = strconv.Itoa(x)
			}
			idx := strings.Join(parts, ".")
			if t[idx] == nil {
				t[idx] = monitor.Row{}
			}
			t[idx][n] = v
		}
	}
	return t, nil
}

// convert maps a PDU to a monitor.Value.
func convert(p gosnmp.SnmpPDU) (monitor.Value, bool) {
	switch p.Type {
	case gosnmp.Integer:
		n, ok := snmp.Int(p)
		if !ok {
			return monitor.Value{Kind: monitor.KindInt, Int: int64(gosnmp.ToBigInt(p.Value).Int64())}, true
		}
		return monitor.Value{Kind: monitor.KindInt, Int: int64(n)}, true
	case gosnmp.Counter32, gosnmp.Gauge32, gosnmp.TimeTicks, gosnmp.Counter64, gosnmp.Uinteger32:
		u, ok := snmp.Uint64(p)
		return monitor.Value{Kind: monitor.KindUint, Uint: u}, ok
	case gosnmp.OctetString, gosnmp.BitString, gosnmp.Opaque:
		return monitor.Value{Kind: monitor.KindBytes, Bytes: snmp.Bytes(p)}, true
	case gosnmp.IPAddress:
		s, _ := p.Value.(string)
		return monitor.Value{Kind: monitor.KindBytes, Bytes: []byte(s)}, true
	case gosnmp.ObjectIdentifier:
		s, _ := p.Value.(string)
		return monitor.Value{Kind: monitor.KindOID, OID: strings.TrimPrefix(s, ".")}, true
	}
	return monitor.Value{}, false
}
