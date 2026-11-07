package mockswitch

import (
	"bufio"
	"encoding/hex"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/lan-dot-party/ers-proxy/internal/monitor"
	"github.com/lan-dot-party/ers-proxy/internal/monitor/edmsrc"
)

// SNMPData answers the /jsp/ SNMP bridge from an snmpsim .snmprec file, so
// that the EDM data source can be tested against the same agent data as the
// SNMP reader.
type SNMPData struct {
	mu   sync.RWMutex      // guards vals and oids (setVariables changes them)
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
	d.mu.RLock()
	defer d.mu.RUnlock()
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
	d.mu.RLock()
	defer d.mu.RUnlock()
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

// writeObjects are objects that only setVariables writes (not in
// monitor.Objects).
var writeObjects = map[string]string{
	"rcVlanRowStatus": "1.3.6.1.4.1.2272.1.3.2.1.20",
	// rcSysActionL1.0 = 7 saves the running configuration (VOSS).
	"rcSysActionL1": "1.3.6.1.4.1.2272.1.1.86",
}

// rcVlanTableOID is the prefix of the columns of rcVlanTable; the row index
// follows the column number.
const rcVlanTableOID = "1.3.6.1.4.1.2272.1.3.2.1"

// octetObjects are the written objects that hold octet strings; all others
// are integers. Of these, binary ones are written as "0x00:40:80" PortSets.
var octetObjects = map[string]bool{
	"rcVlanName": true, "rcVlanPortMembers": true, "rcMltName": true, "rcMltPortMembers": true, "ifAlias": true,
}
var binaryObjects = map[string]bool{"rcVlanPortMembers": true, "rcMltPortMembers": true}

// SetFailure is a rejected setVariables request, in the form of an SNMP error.
type SetFailure struct {
	Status int    // SNMP error status (10 wrongValue, 2 noSuchName, …)
	Index  int    // 1-based position of the failing variable
	Msg    string // name of the status
}

// Error implements error.
func (e *SetFailure) Error() string {
	return fmt.Sprintf("snmpErrorStatus:%d (variable %d: %s)", e.Status, e.Index, e.Msg)
}

// SetVar is one variable of a setVariables request: "object.index" and the
// value as EDM sends it.
type SetVar struct{ Name, Value string }

func objectOID(name string) (string, bool) {
	if o, ok := monitor.Objects[name]; ok {
		return o, true
	}
	o, ok := writeObjects[name]
	return o, ok
}

// Apply writes the variables like the switch does: all or none. Integers are
// decimal, octet strings plain text ("0x00:" for an empty one), PortSets "0x" plus hex octets separated
// by colons. A rcVlanRowStatus of createAndGo(4) creates the VLAN row with
// an empty PortSet; destroy(6) removes every rcVlanTable column of the row.
// Rows are written in order, so a request may create a row and then fill it.
func (d *SNMPData) Apply(vars []SetVar) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	next := make(map[string]record, len(d.vals)+len(vars))
	for k, v := range d.vals {
		next[k] = v
	}
	for i, v := range vars {
		if err := applyVar(next, v); err != nil {
			err.Index = i + 1
			return err
		}
	}
	d.vals = next
	d.oids = d.oids[:0]
	for oid := range next {
		d.oids = append(d.oids, monitor.Ints(oid))
	}
	sort.Slice(d.oids, func(i, j int) bool { return lessInts(d.oids[i], d.oids[j]) })
	return nil
}

func applyVar(vals map[string]record, v SetVar) *SetFailure {
	name, index, _ := strings.Cut(v.Name, ".")
	oid, ok := objectOID(name)
	if !ok || index == "" {
		return &SetFailure{Status: 2, Msg: "noSuchName"}
	}
	full := oid + "." + index
	if name == "rcVlanRowStatus" {
		n, err := strconv.Atoi(v.Value)
		if err != nil {
			return &SetFailure{Status: 7, Msg: "wrongType"}
		}
		_, exists := vals[full]
		switch n {
		case 4: // createAndGo
			if exists {
				return &SetFailure{Status: 12, Msg: "inconsistentValue"}
			}
			vals[full] = record{tag: "2", val: "1"} // active
			size := 2
			for k, r := range vals {
				if strings.HasPrefix(k, rcVlanTableOID+".11.") {
					size = len(r.val) / 2
					break
				}
			}
			vals[rcVlanTableOID+".11."+index] = record{tag: "4x", val: strings.Repeat("00", size)}
			vals[rcVlanTableOID+".2."+index] = record{tag: "4", val: ""}
		case 6: // destroy
			if !exists {
				return &SetFailure{Status: 12, Msg: "inconsistentValue"}
			}
			for k := range vals {
				if rest, ok := strings.CutPrefix(k, rcVlanTableOID+"."); ok {
					if _, idx, ok := strings.Cut(rest, "."); ok && idx == index {
						delete(vals, k)
					}
				}
			}
		default:
			return &SetFailure{Status: 10, Msg: "wrongValue"}
		}
		return nil
	}
	switch {
	case binaryObjects[name]:
		b, ok := parseHexOctets(v.Value)
		if !ok {
			return &SetFailure{Status: 10, Msg: "wrongValue"}
		}
		vals[full] = record{tag: "4x", val: hex.EncodeToString(b)}
	case octetObjects[name]:
		// EDM sends an empty text as "0x00:"; a truly empty value makes a
		// VOSS web server hang, which the mock answers with an error.
		switch v.Value {
		case "0x00:":
			vals[full] = record{tag: "4", val: ""}
		case "":
			return &SetFailure{Status: 7, Msg: "wrongType (empty value)"}
		default:
			vals[full] = record{tag: "4", val: v.Value}
		}
	default:
		if _, err := strconv.ParseInt(v.Value, 10, 64); err != nil {
			return &SetFailure{Status: 7, Msg: "wrongType"}
		}
		vals[full] = record{tag: "2", val: v.Value}
	}
	return nil
}

// parseHexOctets decodes "0x00:40:80".
func parseHexOctets(s string) ([]byte, bool) {
	t, ok := strings.CutPrefix(s, "0x")
	if !ok {
		return nil, false
	}
	var out []byte
	for _, p := range strings.Split(t, ":") {
		if len(p) != 2 {
			return nil, false
		}
		b, err := hex.DecodeString(p)
		if err != nil {
			return nil, false
		}
		out = append(out, b...)
	}
	return out, true
}

// Put sets name.index directly (for tests that need data the .snmprec file
// lacks), in the same value format as Apply.
func (d *SNMPData) Put(name, index, value string) error {
	return d.Apply([]SetVar{{Name: name + "." + index, Value: value}})
}
