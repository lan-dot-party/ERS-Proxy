// Package edmsrc implements monitor.Reader over the EDM HTTP interface of ERS
// switches ("SNMP over HTTP": /jsp/getFormMibs.jsp and /jsp/getTableMibs.jsp),
// for switches whose SNMP agent is not reachable. Only the get commands are
// used; nothing is ever written.
//
// Experimental: the request and answer formats come from EDM's JavaScript
// (firmware 6.6.3) and are verified against the mock switch only.
//   - getVariables answers "name:value\r\n" per object;
//   - getTableVariables answers a JavaScript array literal, one array per row
//     with the requested columns in order; index objects are requested as
//     columns to know the row index;
//   - octet strings of binary objects come as "0x00:1a:…".
package edmsrc

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/lan-dot-party/ers-proxy/internal/edmlogin"
	"github.com/lan-dot-party/ers-proxy/internal/monitor"
	"github.com/lan-dot-party/ers-proxy/internal/upstream"
)

// Table describes an EDM table: its index objects and the columns drivers use.
type Table struct {
	Index   []string
	Columns []string
	// NAIndex: the index objects are not-accessible (LLDP-MIB). The switch
	// returns nothing when they are requested; like EDM, the reader then asks
	// for the pseudo column "indexNA", which carries the instance ("a.b.c").
	NAIndex bool
}

// Tables lists the tables the EDM reader can read. Objects not listed here
// (Q-BRIDGE, POWER-ETHERNET, EtherLike duplex) are not available through EDM
// on ERS firmware and read as empty.
var Tables = []Table{
	{Index: []string{"ifIndex"}, Columns: []string{"ifDescr", "ifType", "ifSpeed", "ifAdminStatus", "ifOperStatus",
		"ifLastChange", "ifInOctets", "ifInDiscards", "ifInErrors", "ifOutOctets", "ifOutDiscards", "ifOutErrors"}},
	{Index: []string{"ifIndex"}, Columns: []string{"ifName", "ifHCInOctets", "ifHCOutOctets", "ifHighSpeed", "ifAlias",
		"ifCounterDiscontinuityTime"}},
	{Index: []string{"dot1dBasePort"}, Columns: []string{"dot1dBasePortIfIndex"}},
	{Index: []string{"dot3adAggPortIndex"}, Columns: []string{"dot3adAggPortAttachedAggID"}},
	{Index: []string{"lldpLocPortNum"}, Columns: []string{"lldpLocPortIdSubtype", "lldpLocPortId", "lldpLocPortDesc"},
		NAIndex: true},
	{Index: []string{"lldpRemTimeMark", "lldpRemLocalPortNum", "lldpRemIndex"}, Columns: []string{"lldpRemPortId",
		"lldpRemPortDesc", "lldpRemSysName", "lldpRemSysCapEnabled"}, NAIndex: true},
	{Index: []string{"rcVlanId"}, Columns: []string{"rcVlanName", "rcVlanPortMembers"}},
	{Index: []string{"rcVlanPortIndex"}, Columns: []string{"rcVlanPortDefaultVlanId", "rcVlanPortPerformTagging"}},
	{Index: []string{"rcPortIndex"}, Columns: []string{"rcPortOperDuplex", "rcPortOperSpeed"}},
	{Index: []string{"rcMltId"}, Columns: []string{"rcMltName", "rcMltPortMembers"}},
	{Index: []string{"s5ChasComGrpIndx", "s5ChasComIndx", "s5ChasComSubIndx"}, Columns: []string{"s5ChasComType",
		"s5ChasComDescr", "s5ChasComVer", "s5ChasComSerNum", "s5ChasComRunningSoftwareVer"}},
}

// kinds of the objects that are not text.
var kinds = map[string]monitor.Kind{
	"sysObjectID": monitor.KindOID, "s5ChasComType": monitor.KindOID,
	"sysUpTime": monitor.KindUint, "ifLastChange": monitor.KindUint, "ifCounterDiscontinuityTime": monitor.KindUint,
	"ifSpeed": monitor.KindUint, "ifHighSpeed": monitor.KindUint, "ifInOctets": monitor.KindUint, "ifOutOctets": monitor.KindUint,
	"ifHCInOctets": monitor.KindUint, "ifHCOutOctets": monitor.KindUint, "ifInErrors": monitor.KindUint,
	"ifOutErrors": monitor.KindUint, "ifInDiscards": monitor.KindUint, "ifOutDiscards": monitor.KindUint,
	"ifType": monitor.KindInt, "ifAdminStatus": monitor.KindInt, "ifOperStatus": monitor.KindInt,
	"dot1dBasePortIfIndex": monitor.KindInt, "dot3adAggPortAttachedAggID": monitor.KindInt, "lldpLocPortIdSubtype": monitor.KindInt,
	"rcVlanPortDefaultVlanId": monitor.KindInt, "rcVlanPortPerformTagging": monitor.KindInt, "rcPortOperDuplex": monitor.KindInt,
	"rcPortOperSpeed": monitor.KindUint,
	"ifIndex":         monitor.KindInt, "dot1dBasePort": monitor.KindInt, "dot3adAggPortIndex": monitor.KindInt, "lldpLocPortNum": monitor.KindInt,
	"lldpRemTimeMark": monitor.KindUint, "lldpRemLocalPortNum": monitor.KindInt, "lldpRemIndex": monitor.KindInt,
	"rcVlanId": monitor.KindInt, "rcVlanPortIndex": monitor.KindInt, "rcPortIndex": monitor.KindInt, "rcMltId": monitor.KindInt,
	"s5ChasComGrpIndx": monitor.KindInt, "s5ChasComIndx": monitor.KindInt, "s5ChasComSubIndx": monitor.KindInt,
}

// binary objects: "0x.." / "aa:bb" answers are decoded as hex.
var binary = map[string]bool{
	"rcVlanPortMembers": true, "rcMltPortMembers": true, "lldpRemSysCapEnabled": true,
	"lldpRemPortId": true, "lldpLocPortId": true,
}

// Session is a logged-in EDM session at one switch. Requests are serialized:
// the switch's web server is slow and handles few connections.
type Session struct {
	rt        http.RoundTripper
	base      string
	user, pwd string

	mu     sync.Mutex
	jar    *edmlogin.Jar
	logged bool
	// refused is set when the switch rejected the login. The session does not
	// try again: ERS switches lock the account after a few failed logins, and
	// a poll every minute would keep it locked for people, too. Storing new
	// credentials creates a new session.
	refused error

	// unknown holds object names the switch answered with Oid_Conversion_Fail
	// (VOSS knows neither ifName nor ifHighSpeed); they are left out of later
	// requests.
	umu     sync.Mutex
	unknown map[string]bool
}

// convFail is the switch's answer for an object name it cannot resolve. One
// such name in a table request truncates the whole answer to one row.
const convFail = "Oid_Conversion_Fail"

func (s *Session) isUnknown(name string) bool {
	s.umu.Lock()
	defer s.umu.Unlock()
	return s.unknown[name]
}

func (s *Session) markUnknown(name string) {
	s.umu.Lock()
	defer s.umu.Unlock()
	if s.unknown == nil {
		s.unknown = map[string]bool{}
	}
	s.unknown[name] = true
}

// NewSession prepares a session; it logs in on the first request.
func NewSession(rt http.RoundTripper, base, user, password string) *Session {
	return &Session{rt: rt, base: strings.TrimRight(base, "/"), user: user, pwd: password, jar: edmlogin.NewJar()}
}

// ErrSession is returned when the switch rejects the session.
var ErrSession = errors.New("EDM session not accepted")

// post sends one command, logging in first if needed and once more if the
// session has expired.
func (s *Session) post(ctx context.Context, path string, form url.Values) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for attempt := 0; attempt < 2; attempt++ {
		if !s.logged {
			if s.refused != nil {
				return "", fmt.Errorf("%w (not retried until the stored login changes)", s.refused)
			}
			s.jar = edmlogin.NewJar()
			if err := edmlogin.Login(ctx, s.rt, s.base, s.jar, s.user, s.pwd); err != nil {
				if errors.Is(err, edmlogin.ErrRejected) || errors.Is(err, edmlogin.ErrChangePassword) {
					s.refused = err
				}
				return "", err
			}
			s.logged = true
		}
		f := url.Values{}
		for k, v := range form {
			f[k] = v
		}
		f.Set("userId", s.jar.Value("auth"))
		rctx := ctx
		if upstream.EDMReadCommand(f.Get("command")) {
			rctx = upstream.WithRetry(ctx)
		}
		req, err := http.NewRequestWithContext(rctx, http.MethodPost, s.base+path, strings.NewReader(f.Encode()))
		if err != nil {
			return "", err
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=UTF-8")
		req.Header.Set("X-Requested-With", "XMLHttpRequest")
		req.Header.Set("Cookie", s.jar.Header())
		resp, err := (&http.Client{Transport: s.rt, Timeout: time.Minute}).Do(req)
		if err != nil {
			return "", err
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		resp.Body.Close()
		if err != nil {
			return "", err
		}
		s.jar.Store(resp)
		text := string(body)
		if resp.StatusCode == http.StatusUnauthorized || strings.Contains(text, "Not Authorized") ||
			strings.Contains(text, "submitForm") { // the login page instead of data
			s.logged = false
			continue
		}
		if resp.StatusCode != http.StatusOK {
			return "", fmt.Errorf("%s: HTTP %d", path, resp.StatusCode)
		}
		return text, nil
	}
	return "", ErrSession
}

// Logout ends the switch's session (GET /logout.html, like EDM's "Log out"
// link). VSP switches allow only a few web sessions at a time, so a session
// that is replaced should not wait for the switch's idle timeout.
func (s *Session) Logout(ctx context.Context) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.logged {
		return
	}
	s.logged = false
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.base+"/logout.html", nil)
	if err != nil {
		return
	}
	req.Header.Set("Cookie", s.jar.Header())
	resp, err := (&http.Client{Transport: s.rt, Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
}

// Reader implements monitor.Reader on a session.
type Reader struct{ S *Session }

// Scalars implements monitor.Reader.
func (r Reader) Scalars(ctx context.Context, names ...string) (map[string]monitor.Value, error) {
	text, err := r.S.post(ctx, "/jsp/getFormMibs.jsp", url.Values{"command": {"getVariables"}, "varNames": {strings.Join(names, ",")}})
	if err != nil {
		return nil, err
	}
	out := map[string]monitor.Value{}
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimRight(line, "\r")
		name, val, ok := strings.Cut(line, ":")
		if !ok || val == "noSuchInstance" || val == "noSuchObject" {
			continue
		}
		name = strings.SplitN(name, ".", 2)[0]
		if val == convFail {
			continue
		}
		if v, ok := convert(name, val); ok {
			out[name] = v
		}
	}
	return out, nil
}

// Columns implements monitor.Reader: columns are grouped by table and read
// together with the table's index objects.
func (r Reader) Columns(ctx context.Context, names ...string) (monitor.Table, error) {
	out := monitor.Table{}
	for _, t := range Tables {
		var want []string
		for _, n := range names {
			for _, c := range t.Columns {
				if n == c && !r.S.isUnknown(n) {
					want = append(want, n)
				}
			}
		}
		if len(want) == 0 {
			continue
		}
		idxCols := len(t.Index)
		if t.NAIndex {
			idxCols = 1
		}
		var rows [][]string
		for len(want) > 0 {
			cols := append(append([]string{}, t.Index...), want...)
			if t.NAIndex {
				cols = append([]string{"indexNA"}, want...)
			}
			text, err := r.S.post(ctx, "/jsp/getTableMibs.jsp", url.Values{"command": {"getTableVariables"}, "varNames": {strings.Join(cols, ",")}})
			if err != nil {
				return nil, err
			}
			if rows, err = ParseArray(text); err != nil {
				return nil, fmt.Errorf("table %s: %w", t.Columns[0], err)
			}
			// Leave out the names the switch cannot resolve and ask again.
			var keep []string
			for i, n := range want {
				failed := false
				for _, row := range rows {
					if idxCols+i < len(row) && row[idxCols+i] == convFail {
						failed = true
						break
					}
				}
				if failed {
					r.S.markUnknown(n)
				} else {
					keep = append(keep, n)
				}
			}
			if len(keep) == len(want) {
				break
			}
			want, rows = keep, nil
		}
		for _, row := range rows {
			if len(row) < idxCols+len(want) {
				continue
			}
			idx := row[:idxCols]
			if t.NAIndex {
				idx = strings.Split(strings.TrimPrefix(row[0], "."), ".")
			}
			if len(idx) != len(t.Index) {
				continue
			}
			ok := true
			for _, part := range idx {
				if _, err := strconv.ParseUint(part, 10, 64); err != nil {
					ok = false
					break
				}
			}
			if !ok {
				continue
			}
			key := strings.Join(idx, ".")
			for i, n := range want {
				if v, ok := convert(n, row[idxCols+i]); ok && row[idxCols+i] != convFail {
					if out[key] == nil {
						out[key] = monitor.Row{}
					}
					out[key][n] = v
				}
			}
		}
	}
	return out, nil
}

func convert(name, s string) (monitor.Value, bool) {
	switch kinds[name] {
	case monitor.KindOID:
		return monitor.Value{Kind: monitor.KindOID, OID: strings.TrimPrefix(s, ".")}, s != ""
	case monitor.KindUint:
		if name == "sysUpTime" || name == "ifLastChange" || name == "ifCounterDiscontinuityTime" {
			if t, ok := parseTicks(s); ok {
				return monitor.Value{Kind: monitor.KindUint, Uint: t}, true
			}
			return monitor.Value{}, false
		}
		u, err := strconv.ParseUint(s, 10, 64)
		return monitor.Value{Kind: monitor.KindUint, Uint: u}, err == nil
	case monitor.KindInt:
		n, err := strconv.ParseInt(s, 10, 64)
		return monitor.Value{Kind: monitor.KindInt, Int: n}, err == nil
	}
	if binary[name] {
		if b, ok := parseHex(s); ok {
			return monitor.Value{Kind: monitor.KindBytes, Bytes: b}, true
		}
	}
	return monitor.Value{Kind: monitor.KindBytes, Bytes: []byte(s)}, true
}

// parseHex decodes "0x0a:ff:…", "0a:ff" or "0x0aff".
func parseHex(s string) ([]byte, bool) {
	t := s
	if strings.HasPrefix(t, "0x") || strings.HasPrefix(t, "0X") {
		t = t[2:]
	} else if !strings.Contains(t, ":") {
		return nil, false
	}
	if t == "" {
		return []byte{}, true
	}
	var parts []string
	if strings.Contains(t, ":") {
		parts = strings.Split(t, ":")
	} else {
		for i := 0; i+1 < len(t); i += 2 {
			parts = append(parts, t[i:i+2])
		}
	}
	out := make([]byte, 0, len(parts))
	for _, p := range parts {
		v, err := strconv.ParseUint(p, 16, 8)
		if err != nil {
			return nil, false
		}
		out = append(out, byte(v))
	}
	return out, true
}

// parseTicks accepts raw TimeTicks or EDM's "N day(s), HHh:MMm:SSs".
func parseTicks(s string) (uint64, bool) {
	if v, err := strconv.ParseUint(s, 10, 64); err == nil {
		return v, true
	}
	var d, h, m, sec uint64
	s2 := strings.Replace(s, "days", "day", 1)
	if _, err := fmt.Sscanf(s2, "%d day, %dh:%dm:%ds", &d, &h, &m, &sec); err != nil {
		return 0, false
	}
	return ((d*24+h)*60+m)*60*100 + sec*100, true
}

// ParseArray parses EDM's JavaScript array literal of string rows, e.g.
// [["1","Port 1"],['2','it\'s']]. Numbers without quotes are accepted too.
func ParseArray(s string) ([][]string, error) {
	p := &parser{s: strings.TrimSpace(s)}
	if p.s == "" {
		return nil, nil
	}
	if !p.eat('[') {
		return nil, errors.New("answer is not an array")
	}
	var rows [][]string
	for {
		p.space()
		if p.eat(']') {
			return rows, nil
		}
		if !p.eat('[') {
			return nil, fmt.Errorf("expected a row at offset %d", p.i)
		}
		var row []string
		for {
			p.space()
			if p.eat(']') {
				break
			}
			v, err := p.value()
			if err != nil {
				return nil, err
			}
			row = append(row, v)
			p.space()
			p.eat(',')
		}
		rows = append(rows, row)
		p.space()
		p.eat(',')
	}
}

type parser struct {
	s string
	i int
}

func (p *parser) space() {
	for p.i < len(p.s) && strings.ContainsRune(" \t\r\n", rune(p.s[p.i])) {
		p.i++
	}
}

func (p *parser) eat(c byte) bool {
	if p.i < len(p.s) && p.s[p.i] == c {
		p.i++
		return true
	}
	return false
}

func (p *parser) value() (string, error) {
	if p.i >= len(p.s) {
		return "", errors.New("unexpected end of answer")
	}
	q := p.s[p.i]
	if q != '"' && q != '\'' {
		start := p.i
		for p.i < len(p.s) && p.s[p.i] != ',' && p.s[p.i] != ']' {
			p.i++
		}
		return strings.TrimSpace(p.s[start:p.i]), nil
	}
	p.i++
	var b strings.Builder
	for p.i < len(p.s) {
		c := p.s[p.i]
		p.i++
		switch {
		case c == q:
			return b.String(), nil
		case c == '\\' && p.i < len(p.s):
			e := p.s[p.i]
			p.i++
			switch e {
			case 'n':
				b.WriteByte('\n')
			case 't':
				b.WriteByte('\t')
			case 'r':
				b.WriteByte('\r')
			default:
				b.WriteByte(e)
			}
		default:
			b.WriteByte(c)
		}
	}
	return "", errors.New("unterminated string in answer")
}
