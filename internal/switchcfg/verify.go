package switchcfg

import (
	"bytes"
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/lan-dot-party/ers-proxy/internal/monitor"
)

// SetBit returns a copy of a PortSet/PortList bitmap with bit i (counted from
// the most significant bit of the first octet, like monitor.BitsMSB) set or
// cleared. The bitmap grows to hold i; size is the minimum length in octets.
func SetBit(b []byte, i int, on bool, size int) []byte {
	n := len(b)
	if need := i/8 + 1; need > n {
		n = need
	}
	if size > n {
		n = size
	}
	out := make([]byte, n)
	copy(out, b)
	mask := byte(1) << uint(7-i%8)
	if on {
		out[i/8] |= mask
	} else {
		out[i/8] &^= mask
	}
	return out
}

// HasBit reports whether bit i is set.
func HasBit(b []byte, i int) bool {
	if i < 0 || i/8 >= len(b) {
		return false
	}
	return b[i/8]&(byte(1)<<uint(7-i%8)) != 0
}

// sameValue compares a written value with one read back. Numbers compare by
// value whatever their kind; octet strings ignore trailing zero octets
// (switches return bitmaps of their own length) and, for text, trailing
// spaces.
func sameValue(want, got monitor.Value) bool {
	if want.Kind == monitor.KindBytes || got.Kind == monitor.KindBytes {
		if want.Kind != got.Kind {
			return false
		}
		w := bytes.TrimRight(want.Bytes, "\x00")
		g := bytes.TrimRight(got.Bytes, "\x00")
		if bytes.Equal(w, g) {
			return true
		}
		return bytes.Equal(bytes.TrimRight(w, " "), bytes.TrimRight(g, " "))
	}
	if want.Kind == monitor.KindOID || got.Kind == monitor.KindOID {
		return want.Kind == got.Kind && want.OID == got.OID
	}
	a, ok1 := want.AsInt()
	b, ok2 := got.AsInt()
	return ok1 && ok2 && a == b
}

// Absent marks an expected row that must not exist (a deleted VLAN): the
// value of an Assignment in Expected or Before.
var Absent = monitor.Value{Kind: monitor.KindNone}

// Mismatch is a value that did not read back as expected.
type Mismatch struct {
	Instance  string
	Want, Got string
}

func (m Mismatch) String() string {
	return fmt.Sprintf("%s: want %s, got %s", m.Instance, m.Want, m.Got)
}

// Verify reads the objects of expected through r and returns those that
// differ. An error means the switch could not be read (not reachable).
func Verify(ctx context.Context, r monitor.Reader, expected []Assignment) ([]Mismatch, error) {
	var scalars []string
	columns := map[string]bool{}
	for _, a := range expected {
		if a.Index == "" {
			scalars = append(scalars, a.Name)
		} else {
			columns[a.Name] = true
		}
	}
	var got map[string]monitor.Value
	if len(scalars) > 0 {
		v, err := r.Scalars(ctx, scalars...)
		if err != nil {
			return nil, err
		}
		got = v
	}
	var table monitor.Table
	if len(columns) > 0 {
		names := make([]string, 0, len(columns))
		for n := range columns {
			names = append(names, n)
		}
		sort.Strings(names)
		// One table per object: objects of different tables have different
		// indexes, and Columns merges by index.
		table = monitor.Table{}
		for _, n := range names {
			t, err := r.Columns(ctx, n)
			if err != nil {
				return nil, err
			}
			for idx, row := range t {
				if table[idx] == nil {
					table[idx] = monitor.Row{}
				}
				for k, v := range row {
					table[idx][k] = v
				}
			}
		}
	}
	var out []Mismatch
	for _, a := range expected {
		var v monitor.Value
		var ok bool
		if a.Index == "" {
			v, ok = got[a.Name]
		} else if row := table[a.Index]; row != nil {
			v, ok = row[a.Name]
		}
		switch {
		case a.Value.Kind == monitor.KindNone:
			if ok {
				out = append(out, Mismatch{a.Instance(), "(absent)", valueText(v)})
			}
		case !ok:
			out = append(out, Mismatch{a.Instance(), valueText(a.Value), "(absent)"})
		case !sameValue(a.Value, v):
			out = append(out, Mismatch{a.Instance(), valueText(a.Value), valueText(v)})
		}
	}
	return out, nil
}

// MismatchError joins mismatches into one error text.
func MismatchError(ms []Mismatch) error {
	parts := make([]string, len(ms))
	for i, m := range ms {
		parts[i] = m.String()
	}
	return fmt.Errorf("read-back differs: %s", strings.Join(parts, "; "))
}
