// Package swos reads the web API of MikroTik SwOS switches (CRS3xx, CSS3xx and
// others running SwOS): the files /sys.b, /link.b, /fwd.b, /vlan.b, /lacp.b
// and /stats.b that SwOS's own web interface loads. It only ever sends GET
// requests for these files; it never writes to the switch.
//
// The files hold JavaScript object literals: unquoted keys, numbers in hex
// (0x1f), strings in single quotes with hex-encoded text ('53573033' =
// "SW03") and bitmaps of ports as numbers (bit 0 = first port) or, on
// switches with more than 32 ports, as arrays of 32-bit words.
package swos

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
)

// maxFile is the largest answer read from the switch.
const maxFile = 1 << 20

// Files the reader may request. Nothing else is ever asked for: snmp.b, for
// example, would contain the SNMP community.
var files = map[string]bool{"sys.b": true, "link.b": true, "fwd.b": true, "vlan.b": true, "lacp.b": true, "stats.b": true}

// ErrLogin means the switch did not accept the stored login.
var ErrLogin = errors.New("the switch rejected the login (check the web login stored for the device)")

// Client fetches SwOS files. Transport must answer the switch's HTTP Digest
// challenge (see upstream.WithDigest).
type Client struct {
	Transport http.RoundTripper
	// Base is http://host[:port] of the switch.
	Base string
}

// Get fetches and parses one file ("link.b").
func (c *Client) Get(ctx context.Context, file string) (any, error) {
	if !files[file] {
		return nil, fmt.Errorf("swos: file %q is not read", file)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(c.Base, "/")+"/"+file, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.Transport.RoundTrip(req)
	if err != nil {
		return nil, fmt.Errorf("swos %s: %w", file, err)
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		return nil, ErrLogin
	case resp.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("swos %s: HTTP %d", file, resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxFile+1))
	if err != nil {
		return nil, fmt.Errorf("swos %s: %w", file, err)
	}
	if len(data) > maxFile {
		return nil, fmt.Errorf("swos %s: answer too large", file)
	}
	v, err := Parse(string(data))
	if err != nil {
		return nil, fmt.Errorf("swos %s: %w", file, err)
	}
	return v, nil
}

// Port is one port of the switch.
type Port struct {
	// Index counts from 0; ifIndex = Index + 1 (as SwOS's SNMP agent does).
	Index int
	// Label is the front-panel name SwOS shows (Port1, SFP1, …); Name is the
	// name the administrator gave the port.
	Label, Name string
	Enabled     bool
	Link        bool
	FullDuplex  bool
	Speed       uint64 // bit/s, 0 = unknown

	// VLAN settings (fwd.b): mode disabled/optional/enabled/strict, receive
	// any/only tagged/only untagged, default VLAN ID.
	VLANMode    string
	VLANReceive string
	DefaultVLAN int

	// Trunk is the number of the LAG the port is in (0 = none).
	Trunk int

	HasStats                    bool
	RxBytes, TxBytes            uint64
	RxErrors, TxErrors, RxDrops uint64
}

// VLAN is an entry of the VLAN table.
type VLAN struct {
	ID      int
	Name    string
	Members []int // port indexes
}

// State is what the reader learned from one round of requests.
type State struct {
	Model, Identity, Version, Serial string
	// Uptime in hundredths of a second (like sysUpTime).
	Uptime uint64
	Ports  []Port
	// HasVLANs is set when fwd.b and vlan.b were read.
	HasVLANs bool
	VLANs    []VLAN
	// Notes lists optional files that could not be read.
	Notes []string
}

var vlanModes = []string{"disabled", "optional", "enabled", "strict"}
var vlanReceive = []string{"any", "only tagged", "only untagged"}

// speeds are SwOS's link speed codes ("10M 100M 1G 10G 5G 2.5G 40G").
var speeds = []uint64{10e6, 100e6, 1e9, 10e9, 5e9, 2.5e9, 40e9}

// Load reads sys.b and link.b (required) and fwd.b, vlan.b, lacp.b and
// stats.b (optional; failures become notes).
func (c *Client) Load(ctx context.Context) (*State, error) {
	sys, err := c.Get(ctx, "sys.b")
	if err != nil {
		return nil, err
	}
	link, err := c.Get(ctx, "link.b")
	if err != nil {
		return nil, err
	}
	st, err := build(sys, link)
	if err != nil {
		return nil, err
	}
	optional := func(file string) any {
		v, err := c.Get(ctx, file)
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return nil
			}
			st.Notes = append(st.Notes, err.Error())
			return nil
		}
		return v
	}
	if fwd := optional("fwd.b"); fwd != nil {
		if vlan := optional("vlan.b"); vlan != nil {
			st.applyVLANs(fwd, vlan)
		}
	}
	if lacp := optional("lacp.b"); lacp != nil {
		st.applyLACP(lacp)
	}
	if stats := optional("stats.b"); stats != nil {
		st.applyStats(stats)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return st, nil
}

// build reads the system and the port list.
func build(sysV, linkV any) (*State, error) {
	sys, ok := sysV.(map[string]any)
	if !ok {
		return nil, errors.New("swos sys.b: unexpected format")
	}
	link, ok := linkV.(map[string]any)
	if !ok {
		return nil, errors.New("swos link.b: unexpected format")
	}
	st := &State{Model: Text(sys["brd"]), Identity: Text(sys["id"]), Version: Text(sys["ver"]), Serial: Text(sys["sid"])}
	st.Uptime, _ = Uint(sys["upt"])
	names, _ := link["nm"].([]any)
	n := len(names)
	if v, ok := Uint(link["prt"]); ok && v > 0 && v <= 256 {
		n = int(v)
	}
	if n == 0 || n > 256 {
		return nil, errors.New("swos link.b: no ports")
	}
	sfp := 0
	if v, ok := Uint(link["sfp"]); ok && int(v) <= n {
		sfp = int(v)
	}
	sfpFirst := n - sfp
	if v, ok := Uint(link["sfpo"]); ok && int(v) <= n {
		sfpFirst = int(v)
	}
	spd, _ := link["spd"].([]any)
	for i := 0; i < n; i++ {
		p := Port{Index: i, Label: label(st.Model, i, sfpFirst, sfp), Enabled: Bit(link["en"], i), Link: Bit(link["lnk"], i),
			FullDuplex: Bit(link["dpx"], i)}
		if i < len(names) {
			p.Name = Text(names[i])
		}
		if i < len(spd) {
			if code, ok := Uint(spd[i]); ok && code < uint64(len(speeds)) {
				p.Speed = speeds[code]
			}
		}
		st.Ports = append(st.Ports, p)
	}
	return st, nil
}

// label follows SwOS's web interface: copper ports before the SFP cages are
// Port1…, the cages SFP1…, copper ports after them continue the Port count.
// Some models name single ports differently (the copper boot port of a
// CRS305/CSS305 is "ETH/BOOT").
func label(model string, i, sfpFirst, sfp int) string {
	family, _, _ := strings.Cut(strings.Replace(model, "CRS", "CSS", 1), "-")
	switch {
	case family == "CSS305" && i == 0, family == "CSS309" && i == 8, family == "CSS317" && i == 16:
		return "ETH/BOOT"
	case model == "CRS328-4C-20S-4S+" && i >= 20 && i < 24:
		return "COMBO" + strconv.Itoa(i-19)
	case model == "CRS328-4C-20S-4S+" && i >= 24 && i < 28:
		return "SFP+" + strconv.Itoa(i-23)
	case family == "CSS312" && i >= 8 && i < 12:
		return "COMBO" + strconv.Itoa(i-7)
	}
	switch {
	case i < sfpFirst:
		return "Port" + strconv.Itoa(i+1)
	case i >= sfpFirst+sfp:
		return "Port" + strconv.Itoa(i-sfp+1)
	}
	return "SFP" + strconv.Itoa(i-sfpFirst+1)
}

func (st *State) applyVLANs(fwdV, vlanV any) {
	fwd, ok := fwdV.(map[string]any)
	list, ok2 := vlanV.([]any)
	if !ok || !ok2 {
		st.Notes = append(st.Notes, "swos fwd.b/vlan.b: unexpected format")
		return
	}
	mode, _ := fwd["vlan"].([]any)
	recv, _ := fwd["vlni"].([]any)
	dvid, _ := fwd["dvid"].([]any)
	for i := range st.Ports {
		p := &st.Ports[i]
		if i < len(mode) {
			if v, ok := Uint(mode[i]); ok && v < uint64(len(vlanModes)) {
				p.VLANMode = vlanModes[v]
			}
		}
		if i < len(recv) {
			if v, ok := Uint(recv[i]); ok && v < uint64(len(vlanReceive)) {
				p.VLANReceive = vlanReceive[v]
			}
		}
		if i < len(dvid) {
			if v, ok := Uint(dvid[i]); ok && v >= 1 && v <= 4095 {
				p.DefaultVLAN = int(v)
			}
		}
	}
	for _, e := range list {
		m, ok := e.(map[string]any)
		if !ok {
			continue
		}
		id, ok := Uint(m["vid"])
		if !ok || id < 1 || id > 4094 {
			continue
		}
		v := VLAN{ID: int(id), Name: Text(m["nm"])}
		for i := range st.Ports {
			if Bit(m["mbr"], i) {
				v.Members = append(v.Members, i)
			}
		}
		st.VLANs = append(st.VLANs, v)
	}
	st.HasVLANs = true
}

func (st *State) applyLACP(v any) {
	m, ok := v.(map[string]any)
	if !ok {
		return
	}
	grp, _ := m["grp"].([]any)
	for i := range st.Ports {
		if i < len(grp) {
			if g, ok := Uint(grp[i]); ok && g > 0 && g < 128 {
				st.Ports[i].Trunk = int(g)
			}
		}
	}
}

func (st *State) applyStats(v any) {
	m, ok := v.(map[string]any)
	if !ok {
		return
	}
	at := func(key string, i int) uint64 {
		a, _ := m[key].([]any)
		if i >= len(a) {
			return 0
		}
		n, _ := Uint(a[i])
		return n
	}
	rb, _ := m["rb"].([]any)
	for i := range st.Ports {
		if i >= len(rb) {
			break
		}
		p := &st.Ports[i]
		p.HasStats = true
		p.RxBytes = at("rbh", i)<<32 | at("rb", i)
		p.TxBytes = at("tbh", i)<<32 | at("tb", i)
		p.RxErrors = at("rfcs", i) + at("rte", i) + at("rae", i) + at("rr", i) + at("fr", i)
		p.TxErrors = at("tur", i) + at("tec", i) + at("tlc", i)
		p.RxDrops = at("rov", i)
	}
}

// Uint returns a number.
func Uint(v any) (uint64, bool) {
	n, ok := v.(uint64)
	return n, ok
}

// Bit reports whether bit i of a SwOS bitmap is set: a number, or an array
// of 32-bit words whose last element holds bits 0–31.
func Bit(v any, i int) bool {
	if i < 0 {
		return false
	}
	switch b := v.(type) {
	case uint64:
		return i < 64 && b>>uint(i)&1 == 1
	case []any:
		for k := len(b) - 1; k >= 0; k-- {
			if i < 32 {
				w, _ := Uint(b[k])
				return w>>uint(i)&1 == 1
			}
			i -= 32
		}
	}
	return false
}

// Text decodes a hex-encoded SwOS string ('53573033' = "SW03"). Strings that
// are not valid hex are returned as they are.
func Text(v any) string {
	s, ok := v.(string)
	if !ok {
		return ""
	}
	b, err := hex.DecodeString(s)
	if err != nil {
		return s
	}
	out := strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1 // control characters
		}
		return r
	}, strings.ToValidUTF8(string(b), "?"))
	return strings.TrimSpace(out)
}
