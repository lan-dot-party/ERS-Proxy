// Package gwn reads the web API of Grandstream GWN switches (verified on a
// GWN7721 with firmware 1.0.7.128). The switch's own web interface (a Vue
// application) loads all its data from this API:
//
//   - GET /get.cgi?cmd=<command>[&index=<n>] reads; POST /set.cgi?cmd=<command>
//     with a JSON body changes settings. Answers are JSON objects
//     {"code":200,"msg":"success","data":{…}}; code 401 means the session
//     has ended.
//   - Login: get_nonce, then POST set.cgi?cmd=login with the user name and
//     hex(SHA-256("<user>:<nonce>:<password>")). The answer carries a token,
//     sent as the Authorization header of later requests.
//   - Lists are paged: index=0 returns {list, remain}; while remain > 0 the
//     next page starts at the id of the last entry.
//
// The switch allows one session per account: while someone is logged in to
// the web interface with the same account, a login is refused ("user already
// logged in", ErrBusy), and while the client holds a session nobody else can
// log in. Load therefore logs in, reads and logs out again within one call.
//
// The client only ever sends the read commands in readCommands, and POSTs
// only login and logout. It never changes a setting.
//
// The GWN7721 (firmware 1.0.7.128) reboots when a client resets (TCP RST) a
// connection to its HTTPS port. The client therefore always reads answers to
// the end, so that closing a connection never resets it; prefer http:// for
// the device address (the API is the same on port 80).
package gwn

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
)

// maxAnswer is the largest answer read from the switch.
const maxAnswer = 1 << 20

// maxPages limits paged lists (a switch has at most a few hundred entries).
const maxPages = 64

// readCommands are the only commands sent to get.cgi. Nothing else is ever
// asked for: get_cfgfile, for example, would return the configuration with
// secrets.
var readCommands = map[string]bool{
	"get_nonce": true, "get_product_name": true, "get_sysinfo": true, "get_port": true, "get_8021q_vlan": true, "get_8021q_pvid": true,
	"get_vlan_state": true, "get_lag_list": true, "get_lldp_nei_info": true, "get_mib_state": true,
}

// ErrLogin means the switch did not accept the stored login. Callers must
// not retry with the same login: the switch locks the account after a few
// failed attempts.
var ErrLogin = errors.New("the switch rejected the login (check the web login stored for the device)")

// ErrBusy means the account is logged in elsewhere (the switch allows one
// session per account) or temporarily locked; the next poll tries again.
var ErrBusy = errors.New("the web login is in use or locked on the switch (the switch allows one session per account); trying again later")

// errSession means the token is no longer valid.
var errSession = errors.New("gwn: session ended")

// Client talks to one switch. Load logs in, and once more when the switch
// ends the session in between, and logs out at the end. Safe for concurrent
// use; requests are sent one at a time.
type Client struct {
	Transport http.RoundTripper
	// Base is http(s)://host[:port] of the switch.
	Base               string
	Username, Password string

	mu       sync.Mutex
	token    string
	rejected bool
}

// answer is the envelope of every API answer.
type answer struct {
	Code int             `json:"code"`
	Msg  string          `json:"msg"`
	Data json.RawMessage `json:"data"`
}

func (c *Client) do(ctx context.Context, method, path string, body any) (*answer, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimSuffix(c.Base, "/")+path, rd)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	// No compression: the switch sends some files gzip-compressed without
	// saying so.
	req.Header.Set("Accept-Encoding", "identity")
	if c.token != "" {
		req.Header.Set("Authorization", c.token)
	}
	resp, err := c.Transport.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxAnswer+1))
	// Read the rest too: closing a connection with unread data resets it.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 16*maxAnswer))
	resp.Body.Close()
	if err != nil {
		return nil, err
	}
	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		return nil, errSession
	case resp.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	case len(data) > maxAnswer:
		return nil, errors.New("answer too large")
	}
	var a answer
	if err := json.Unmarshal(data, &a); err != nil {
		return nil, fmt.Errorf("answer is not JSON: %w", err)
	}
	if a.Code == http.StatusUnauthorized {
		return nil, errSession
	}
	return &a, nil
}

// login gets a new token. Called with c.mu held.
func (c *Client) login(ctx context.Context) error {
	if c.rejected {
		return fmt.Errorf("%w (not tried again until the login is changed)", ErrLogin)
	}
	c.token = ""
	a, err := c.do(ctx, http.MethodGet, "/get.cgi?cmd=get_nonce", nil)
	if err != nil {
		return fmt.Errorf("gwn get_nonce: %w", err)
	}
	var n struct {
		Nonce json.RawMessage `json:"nonce"`
	}
	if a.Code != http.StatusOK || json.Unmarshal(a.Data, &n) != nil || len(n.Nonce) == 0 {
		return fmt.Errorf("gwn get_nonce: code %d", a.Code)
	}
	nonce := strings.Trim(string(n.Nonce), `"`)
	sum := sha256.Sum256([]byte(c.Username + ":" + nonce + ":" + c.Password))
	// Field order as in the web interface (username first).
	a, err = c.do(ctx, http.MethodPost, "/set.cgi?cmd=login", struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}{c.Username, hex.EncodeToString(sum[:])})
	if err != nil {
		return fmt.Errorf("gwn login: %w", err)
	}
	var l struct {
		Token      string `json:"token"`
		ErrorCode  int    `json:"errorCode"`
		RetryTimes int    `json:"retryTimes"`
		LockTime   int    `json:"lockTime"`
	}
	_ = json.Unmarshal(a.Data, &l)
	switch {
	case a.Code == http.StatusOK && l.Token != "":
	case l.ErrorCode == 2 || l.ErrorCode == 3: // 2: locked for lockTime s, 3: logged in elsewhere
		return fmt.Errorf("%w (error %d, locked for %d s)", ErrBusy, l.ErrorCode, l.LockTime)
	default: // 1: wrong user name or password; anything else is taken as that too
		c.rejected = true
		return fmt.Errorf("%w (code %d, error %d, retries left %d, locked for %d s)", ErrLogin, a.Code, l.ErrorCode, l.RetryTimes, l.LockTime)
	}
	c.token = l.Token
	return nil
}

// Logout ends the session, if there is one (Load already does).
func (c *Client) Logout(ctx context.Context) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.logout(ctx)
}

// logout ends the session. Called with c.mu held.
func (c *Client) logout(ctx context.Context) {
	if c.token == "" {
		return
	}
	_, _ = c.do(ctx, http.MethodPost, "/set.cgi?cmd=logout", map[string]string{"token": c.token})
	c.token = ""
}

// get runs one read command and decodes its data into out. Called with c.mu
// held; logs in first if needed, and once more if the session has ended.
func (c *Client) get(ctx context.Context, cmd string, index int, out any) error {
	if !readCommands[cmd] {
		return fmt.Errorf("gwn: command %q is not read", cmd)
	}
	q := url.Values{"cmd": {cmd}}
	if index >= 0 {
		q.Set("index", strconv.Itoa(index))
	}
	for attempt := 0; ; attempt++ {
		if c.token == "" {
			if err := c.login(ctx); err != nil {
				return err
			}
		}
		a, err := c.do(ctx, http.MethodGet, "/get.cgi?"+q.Encode(), nil)
		if errors.Is(err, errSession) && attempt == 0 {
			c.token = ""
			continue
		}
		if err != nil {
			return fmt.Errorf("gwn %s: %w", cmd, err)
		}
		if a.Code != http.StatusOK {
			return fmt.Errorf("gwn %s: code %d (%s)", cmd, a.Code, a.Msg)
		}
		if err := json.Unmarshal(a.Data, out); err != nil {
			return fmt.Errorf("gwn %s: %w", cmd, err)
		}
		return nil
	}
}

// list reads all pages of a paged list command.
func (c *Client) list(ctx context.Context, cmd string) ([]json.RawMessage, error) {
	var all []json.RawMessage
	index := 0
	for page := 0; page < maxPages; page++ {
		var p struct {
			List   []json.RawMessage `json:"list"`
			Remain int               `json:"remain"`
		}
		if err := c.get(ctx, cmd, index, &p); err != nil {
			return nil, err
		}
		all = append(all, p.List...)
		if p.Remain <= 0 || len(p.List) == 0 {
			return all, nil
		}
		var last struct {
			ID  int `json:"id"`
			PID int `json:"pid"`
		}
		_ = json.Unmarshal(p.List[len(p.List)-1], &last)
		next := max(last.ID, last.PID)
		if next <= index {
			return all, nil
		}
		index = next
	}
	return all, nil
}

// Port is one front-panel port. ID is the switch's port number, which is also
// the port's ifIndex in its SNMP agent.
type Port struct {
	ID int
	// Label is the name the web interface shows ("2.5GE1", "SFP+9").
	Label string
	// Type is the switch's port type code (see typeNames).
	Type int
	// Descr is the description the administrator gave the port.
	Descr      string
	Enabled    bool
	Link       bool
	FullDuplex bool
	Speed      uint64 // bit/s while the link is up, else 0
	// LAG is the number of the link aggregation group (0 = none).
	LAG  int
	PVID int
	// Bad frames received and sent (the API has no byte counters).
	RxBad, TxBad uint64
	HasStats     bool
}

// VLAN is an 802.1Q VLAN with its member ports (port IDs).
type VLAN struct {
	ID       int
	Name     string
	Tagged   []int
	Untagged []int
}

// Neighbor is an LLDP neighbour seen on a port.
type Neighbor struct {
	LocalPort                         int
	ChassisID, PortID, SysName, Descr string
}

// State is what the client learned from one round of requests.
type State struct {
	Model, Name, Firmware, Serial, Location, Contact, ObjectID string
	// Uptime in hundredths of a second (like sysUpTime).
	Uptime uint64
	Ports  []Port
	// VLANs802 is false when the switch runs port-based VLANs instead of
	// 802.1Q; VLANs is then empty.
	VLANs802  bool
	HasVLANs  bool
	VLANs     []VLAN
	Neighbors []Neighbor
	// Notes lists optional data that could not be read.
	Notes []string
}

// typeNames are the port type names of the web interface; a port's label is
// its type name followed by its number.
var typeNames = map[int]string{0: "GE", 1: "2.5GE", 2: "2.5SFP", 3: "SFP+", 4: "10GE", 5: "SFP"}

// speeds maps the API's speed codes to bit/s.
var speeds = map[int]uint64{0: 10e6, 1: 100e6, 2: 1e9, 3: 2.5e9, 4: 10e9, 5: 5e9}

// Load reads the switch's state. Only the system information and the port
// list are required; other parts that fail are listed in State.Notes.
func (c *Client) Load(ctx context.Context) (*State, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	// Log out even when the poll's context has ended, so the account is free
	// for the web interface again.
	defer c.logout(context.WithoutCancel(ctx))
	st := &State{}
	var sys struct {
		DevName, SysLocation, SysContact, SysVer, DevSN, SysObjID string
		UpTime                                                    uint64
	}
	if err := c.get(ctx, "get_sysinfo", -1, &sys); err != nil {
		return nil, err
	}
	st.Name, st.Location, st.Contact, st.Firmware, st.Serial, st.ObjectID =
		sys.DevName, sys.SysLocation, sys.SysContact, sys.SysVer, sys.DevSN, strings.TrimPrefix(sys.SysObjID, ".")
	st.Uptime = sys.UpTime * 100

	raw, err := c.list(ctx, "get_port")
	if err != nil {
		return nil, err
	}
	byID := map[int]*Port{}
	for _, r := range raw {
		var p struct {
			PID, Type, Enable, Link, ActSpd, ActDpx, Tid int
			Descr                                        string
		}
		if json.Unmarshal(r, &p) != nil || p.PID <= 0 {
			continue
		}
		port := Port{ID: p.PID, Type: p.Type, Label: typeNames[p.Type] + strconv.Itoa(p.PID), Descr: p.Descr,
			Enabled: p.Enable == 1, Link: p.Link == 1, LAG: p.Tid}
		if port.Link {
			port.Speed = speeds[p.ActSpd]
			port.FullDuplex = p.ActDpx == 1
		}
		st.Ports = append(st.Ports, port)
	}
	if len(st.Ports) == 0 {
		return nil, errors.New("gwn get_port: the switch returned no ports")
	}
	for i := range st.Ports {
		byID[st.Ports[i].ID] = &st.Ports[i]
	}
	var pn struct{ ProductName string }
	if err := c.get(ctx, "get_product_name", -1, &pn); err == nil && pn.ProductName != "" {
		st.Model = pn.ProductName
	} else {
		st.Model = modelFromName(sys.DevName)
	}

	if raw, err := c.list(ctx, "get_mib_state"); err == nil {
		for _, r := range raw {
			var m struct {
				ID                 int
				TxBadPkt, RxBadPkt uint64
			}
			if json.Unmarshal(r, &m) == nil {
				if p := byID[m.ID]; p != nil {
					p.RxBad, p.TxBad, p.HasStats = m.RxBadPkt, m.TxBadPkt, true
				}
			}
		}
	} else {
		st.Notes = append(st.Notes, "port statistics: "+err.Error())
	}

	var vs struct{ Enable int }
	if err := c.get(ctx, "get_vlan_state", -1, &vs); err != nil {
		st.Notes = append(st.Notes, "VLAN mode: "+err.Error())
	} else if vs.Enable == 0 { // 0: 802.1Q VLANs; 1: port-based VLANs
		st.VLANs802 = true
		if err := c.loadVLANs(ctx, st, byID); err != nil {
			st.Notes = append(st.Notes, "VLANs: "+err.Error())
		}
	}

	if raw, err := c.list(ctx, "get_lldp_nei_info"); err == nil {
		for _, r := range raw {
			var n struct {
				LocalPort                            int
				ChassisID, PortID, SysName, SysDescp string
			}
			if json.Unmarshal(r, &n) == nil && n.LocalPort > 0 {
				st.Neighbors = append(st.Neighbors, Neighbor{LocalPort: n.LocalPort, ChassisID: n.ChassisID,
					PortID: n.PortID, SysName: n.SysName, Descr: n.SysDescp})
			}
		}
	} else {
		st.Notes = append(st.Notes, "LLDP: "+err.Error())
	}
	return st, nil
}

func (c *Client) loadVLANs(ctx context.Context, st *State, byID map[int]*Port) error {
	var pv struct {
		List []struct{ ID, Pvid int } `json:"list"`
	}
	if err := c.get(ctx, "get_8021q_pvid", -1, &pv); err != nil {
		return err
	}
	for _, x := range pv.List {
		if p := byID[x.ID]; p != nil {
			p.PVID = x.Pvid
		}
	}
	raw, err := c.list(ctx, "get_8021q_vlan")
	if err != nil {
		return err
	}
	for _, r := range raw {
		var v struct {
			Vid              int
			Descr            string
			TagMap, UntagMap string
		}
		if json.Unmarshal(r, &v) != nil || v.Vid < 1 || v.Vid > 4094 {
			continue
		}
		st.VLANs = append(st.VLANs, VLAN{ID: v.Vid, Name: v.Descr, Tagged: PortMap(v.TagMap), Untagged: PortMap(v.UntagMap)})
	}
	st.HasVLANs = true
	return nil
}

// PortMap decodes the API's port bitmaps ("0x7FE"): bit n (counted from the
// least significant bit) is port n; bit 0 is unused. Invalid text yields nil.
func PortMap(s string) []int {
	s = strings.TrimPrefix(strings.TrimPrefix(strings.TrimSpace(s), "0x"), "0X")
	if s == "" || len(s) > 1024 {
		return nil
	}
	n, ok := new(big.Int).SetString(s, 16)
	if !ok {
		return nil
	}
	var ports []int
	for i := 1; i < n.BitLen(); i++ {
		if n.Bit(i) == 1 {
			ports = append(ports, i)
		}
	}
	return ports
}

// modelFromName returns the device name when it looks like a model
// ("GWN7721", the factory setting).
func modelFromName(devName string) string {
	if strings.HasPrefix(devName, "GWN") && !strings.ContainsAny(devName, " \t") {
		return devName
	}
	return ""
}
