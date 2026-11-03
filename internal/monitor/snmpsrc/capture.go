package snmpsrc

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"

	"github.com/gosnmp/gosnmp"

	"github.com/lan-dot-party/ers-proxy/internal/monitor"
	"github.com/lan-dot-party/ers-proxy/internal/snmp"
)

// Capture walks every object the drivers use and writes the answers in
// snmpsim's .snmprec format, so that a device can be reproduced in tests
// ("device report"). With anonymize, names, aliases, serial numbers, VLAN
// names and LLDP neighbours are replaced; model and firmware strings stay.
// Only GET/GETBULK requests are sent. Objects the device cannot answer (a
// timeout, a value gosnmp cannot decode) are skipped and returned in skipped.
func Capture(ctx context.Context, t snmp.Target, w io.Writer, anonymize bool) (n int, skipped []string, err error) {
	g, err := snmp.Dial(ctx, t, 0)
	if err != nil {
		return 0, nil, err
	}
	defer snmp.Close(g)
	names := make([]string, 0, len(monitor.Objects))
	for n := range monitor.Objects {
		names = append(names, n)
	}
	sort.Strings(names)
	type line struct {
		oid []int
		s   string
	}
	var lines []line
	for _, name := range names {
		if err := ctx.Err(); err != nil {
			return 0, skipped, err
		}
		base := monitor.Objects[name]
		pdus, err := g.BulkWalkAll("." + base)
		if err != nil {
			skipped = append(skipped, fmt.Sprintf("%s: %v", name, err))
			continue
		}
		for _, p := range pdus {
			oid := strings.TrimPrefix(p.Name, ".")
			if !strings.HasPrefix(oid, base+".") {
				continue
			}
			tag, val, ok := encode(p)
			if !ok {
				continue
			}
			if anonymize {
				val = anon(name, oid[len(base)+1:], tag, val)
			}
			lines = append(lines, line{oid: parseOID(oid), s: oid + "|" + tag + "|" + val})
		}
	}
	sort.Slice(lines, func(i, j int) bool { return lessOID(lines[i].oid, lines[j].oid) })
	for _, l := range lines {
		if _, err := fmt.Fprintln(w, l.s); err != nil {
			return 0, skipped, err
		}
	}
	return len(lines), skipped, nil
}

func parseOID(s string) []int {
	parts := strings.Split(s, ".")
	out := make([]int, len(parts))
	for i, p := range parts {
		out[i], _ = strconv.Atoi(p)
	}
	return out
}

func lessOID(a, b []int) bool {
	for i := 0; i < len(a) && i < len(b); i++ {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return len(a) < len(b)
}

// encode returns the snmprec type tag and value of a PDU.
func encode(p gosnmp.SnmpPDU) (string, string, bool) {
	switch p.Type {
	case gosnmp.Integer:
		return "2", gosnmp.ToBigInt(p.Value).String(), true
	case gosnmp.OctetString, gosnmp.Opaque, gosnmp.BitString:
		b := snmp.Bytes(p)
		for _, c := range b {
			if c < 0x20 || c > 0x7e || c == '|' {
				return "4x", fmt.Sprintf("%x", b), true
			}
		}
		return "4", string(b), true
	case gosnmp.ObjectIdentifier:
		s, _ := p.Value.(string)
		return "6", strings.TrimPrefix(s, "."), true
	case gosnmp.IPAddress:
		s, _ := p.Value.(string)
		return "64", s, true
	case gosnmp.Counter32:
		return "65", gosnmp.ToBigInt(p.Value).String(), true
	case gosnmp.Gauge32, gosnmp.Uinteger32:
		return "66", gosnmp.ToBigInt(p.Value).String(), true
	case gosnmp.TimeTicks:
		return "67", gosnmp.ToBigInt(p.Value).String(), true
	case gosnmp.Counter64:
		return "70", gosnmp.ToBigInt(p.Value).String(), true
	}
	return "", "", false
}

// anon replaces identifying values. index is the instance part of the OID.
func anon(name, index, tag, val string) string {
	text := func(s string) string {
		if tag == "4x" {
			return fmt.Sprintf("%x", s)
		}
		return s
	}
	switch name {
	case "sysName":
		return text("switch")
	case "sysLocation":
		return text("")
	case "ifAlias":
		if val == "" {
			return val
		}
		return text("alias-" + index)
	case "rcVlanName", "dot1qVlanStaticName", "rcMltName", "tpVlanName":
		if val == "" {
			return val
		}
		return text("name-" + index)
	case "s5ChasComSerNum":
		return text("SN-" + index)
	case "lldpRemSysName":
		return text("neighbor-" + index)
	case "lldpRemPortDesc":
		return text("neighbor port")
	case "lldpRemPortId":
		if tag == "4x" {
			return "020000000001" // locally administered MAC
		}
		return text("port-" + index)
	}
	return val
}
