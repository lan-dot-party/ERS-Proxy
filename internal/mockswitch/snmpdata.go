package mockswitch

import (
	"bufio"
	"encoding/hex"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"

	"github.com/lan-dot-party/ers-proxy/internal/monitor"
	"github.com/lan-dot-party/ers-proxy/internal/monitor/edmsrc"
)

// SNMPData answers the /jsp/ SNMP bridge from an snmpsim .snmprec file, so
// that the EDM data source can be tested against the same agent data as the
// SNMP reader.
type SNMPData struct {
	vals map[string]record // OID → value
	oids [][]int           // sorted, for walks
}

type record struct {
	tag, val string
}

// LoadSNMPRec parses an .snmprec file (oid|tag|value per line).
func LoadSNMPRec(r io.Reader) (*SNMPData, error) {
	d := &SNMPData{vals: map[string]record{}}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		parts := strings.SplitN(line, "|", 3)
		if len(parts) != 3 {
			continue
		}
		tag := strings.SplitN(parts[1], ":", 2)[0] // drop variation modules ("67:numeric")
		d.vals[parts[0]] = record{tag: tag, val: parts[2]}
		d.oids = append(d.oids, monitor.Ints(parts[0]))
	}
	sort.Slice(d.oids, func(i, j int) bool { return lessInts(d.oids[i], d.oids[j]) })
	return d, sc.Err()
}

func lessInts(a, b []int) bool {
	for i := 0; i < len(a) && i < len(b); i++ {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return len(a) < len(b)
}

func join(ints []int) string {
	s := make([]string, len(ints))
	for i, v := range ints {
		s[i] = strconv.Itoa(v)
	}
	return strings.Join(s, ".")
}

// edmValue formats a value the way EDM's web server does.
func edmValue(r record) string {
	switch r.tag {
	case "4x":
		b, err := hex.DecodeString(r.val)
		if err != nil {
			return ""
		}
		parts := make([]string, len(b))
		for i, x := range b {
			parts[i] = fmt.Sprintf("%02x", x)
		}
		return "0x" + strings.Join(parts, ":")
	case "67":
		if strings.Contains(r.val, "=") { // numeric variation: rate=…,initial=…
			for _, kv := range strings.Split(r.val, ",") {
				if v, ok := strings.CutPrefix(kv, "initial="); ok {
					return v
				}
			}
			return "0"
		}
	}
	return r.val
}

// scalars answers getVariables.
func (d *SNMPData) scalars(names string) string {
	var out []string
	for _, nm := range strings.Split(names, ",") {
		base := strings.SplitN(nm, ".", 2)[0]
		oid, ok := monitor.Objects[base]
		r, found := d.vals[oid+".0"]
		if !ok || !found {
			out = append(out, base+":noSuchObject")
			continue
		}
		out = append(out, base+":"+edmValue(r))
	}
	return strings.Join(out, "\r\n") + "\r\n"
}

// table answers getTableVariables for a table of edmsrc.Tables: one row per
// instance of the first data column, index objects first.
func (d *SNMPData) table(names string) (string, bool) {
	cols := strings.Split(names, ",")
	var t *edmsrc.Table
	naIndex := len(cols) > 1 && cols[0] == "indexNA"
	for i := range edmsrc.Tables {
		tt := &edmsrc.Tables[i]
		n := len(tt.Index)
		if naIndex {
			n = 1
		} else if len(cols) <= n || strings.Join(cols[:n], ",") != strings.Join(tt.Index, ",") {
			continue
		}
		for _, c := range tt.Columns {
			if len(cols) > n && c == cols[n] {
				t = tt
			}
		}
	}
	if t == nil {
		return "", false
	}
	if t.NAIndex && !naIndex {
		return "[]", true // like the switch: not-accessible index objects read as nothing
	}
	data := cols[len(t.Index):]
	if naIndex {
		data = cols[1:]
	}
	first := monitor.Objects[data[0]]
	prefix := monitor.Ints(first)
	var rows []string
	for _, oid := range d.oids {
		if len(oid) <= len(prefix) || !lessOrEqualPrefix(prefix, oid) {
			continue
		}
		idx := oid[len(prefix):]
		if len(idx) != len(t.Index) {
			continue
		}
		cells := make([]string, 0, len(cols))
		if naIndex {
			cells = append(cells, join(idx))
		} else {
			for _, v := range idx {
				cells = append(cells, strconv.Itoa(v))
			}
		}
		for _, c := range data {
			cells = append(cells, edmValue(d.vals[monitor.Objects[c]+"."+join(idx)]))
		}
		for i, c := range cells {
			cells[i] = `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(c) + `"`
		}
		rows = append(rows, "["+strings.Join(cells, ",")+"]")
	}
	return "[" + strings.Join(rows, ",") + "]", true
}

func lessOrEqualPrefix(prefix, oid []int) bool {
	for i, v := range prefix {
		if oid[i] != v {
			return false
		}
	}
	return true
}
