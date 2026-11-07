// Package gwntest is a fake Grandstream GWN web server for tests. It models a
// small 10-port GWN7721; all values are made up.
package gwntest

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"sync"
)

// Credentials accepted by the fake switch.
const (
	User     = "admin"
	Password = "test-password"
)

// Write is one POST to set.cgi other than login and logout.
type Write struct {
	Cmd, Body string
}

// Server is the fake switch.
type Server struct {
	// PageSize > 0 pages get_8021q_vlan with that many entries per page.
	PageSize int

	mu        sync.Mutex
	portBased bool
	nonces    map[string]bool
	tokens    map[string]bool
	logins    int
	logouts   int
	writes    []Write
	requests  int
}

// New returns a fake switch.
func New() *Server {
	return &Server{nonces: map[string]bool{}, tokens: map[string]bool{}}
}

// SetPortBased switches to port-based VLANs (get_vlan_state enable 1).
func (s *Server) SetPortBased(on bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.portBased = on
}

// LoginElsewhere opens a session as if someone had logged in to the web
// interface: like the real switch (one session per account), further logins
// are refused with errorCode 3 until ExpireSessions.
func (s *Server) LoginElsewhere() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tokens[randHex(16)] = true
}

// Sessions returns the number of open sessions.
func (s *Server) Sessions() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.tokens)
}

// ExpireSessions invalidates all tokens: the next request gets code 401.
func (s *Server) ExpireSessions() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tokens = map[string]bool{}
}

// Logins returns the number of login requests (successful or not).
func (s *Server) Logins() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.logins
}

// Logouts returns the number of logout requests.
func (s *Server) Logouts() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.logouts
}

// Writes returns the POSTs to set.cgi other than login and logout.
func (s *Server) Writes() []Write {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Write(nil), s.writes...)
}

// Requests returns the number of requests seen.
func (s *Server) Requests() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.requests
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// portMap encodes ports as the API's bitmap ("0x7FE"): bit n is port n.
func portMap(ports ...int) string {
	var v uint64
	for _, p := range ports {
		v |= 1 << uint(p)
	}
	return fmt.Sprintf("0x%X", v)
}

type m = map[string]any

func reply(w http.ResponseWriter, code int, msg string, data any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(m{"code": code, "msg": msg, "data": data})
}

func ok(w http.ResponseWriter, data any) { reply(w, 200, "success", data) }

// ports of the fake switch: pid, type, descr, enable, link, actSpd, tid.
func portList() []m {
	type pd struct {
		descr             string
		typ, en, link, sp int
		tid               int
	}
	defs := []pd{
		1:  {"office", 1, 1, 1, 3, 0},
		2:  {"", 1, 1, 1, 2, 0},
		3:  {"", 1, 1, 1, 3, 1},
		4:  {"", 1, 1, 0, 3, 1},
		5:  {"", 1, 0, 0, 3, 0},
		6:  {"", 1, 1, 0, 3, 0},
		7:  {"", 1, 1, 0, 3, 0},
		8:  {"", 1, 1, 0, 3, 0},
		9:  {"uplink", 3, 1, 1, 4, 0},
		10: {"", 3, 1, 0, 4, 0},
	}
	var l []m
	for i := 1; i <= 10; i++ {
		d := defs[i]
		l = append(l, m{"pid": i, "type": d.typ, "descr": d.descr, "enable": d.en, "link": d.link, "speed": 3,
			"duplex": 1, "flowCtl": 1, "tid": d.tid, "actSpd": d.sp, "actDpx": 1, "ANEn": 1, "actFctl": 1, "attr": 0})
	}
	return l
}

func vlanList() []m {
	return []m{
		{"id": 1, "vid": 1, "descr": "default", "tagMap": portMap(), "untagMap": portMap(1, 3, 4, 5, 6, 7, 8, 9, 10)},
		{"id": 2, "vid": 100, "descr": "office", "tagMap": portMap(3, 9), "untagMap": portMap(2)},
	}
}

func pvidList() []m {
	var l []m
	for i := 1; i <= 10; i++ {
		pv := 1
		if i == 2 {
			pv = 100
		}
		l = append(l, m{"id": i, "pvid": pv, "ingressCheck": 0})
	}
	return l
}

func mibList() []m {
	var l []m
	for i := 1; i <= 10; i++ {
		e := m{"id": i, "enable": 1, "link": 0, "TxGoodPkt": 0, "RxGoodPkt": 0, "TxBadPkt": 0, "RxBadPkt": 0}
		switch i {
		case 1:
			e["RxBadPkt"], e["TxBadPkt"] = 7, 2
		case 3:
			e["RxBadPkt"] = 5
		case 9:
			e["TxBadPkt"] = 11
		}
		l = append(l, e)
	}
	return l
}

func (s *Server) vlanPage(w http.ResponseWriter, index int) {
	all := vlanList()
	var list []m
	for _, v := range all {
		if v["id"].(int) > index {
			list = append(list, v)
		}
	}
	remain := 0
	if s.PageSize > 0 && len(list) > s.PageSize {
		remain = len(list) - s.PageSize
		list = list[:s.PageSize]
	}
	ok(w, m{"remain": remain, "count": len(all), "list": list})
}

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests++
	cmd := r.URL.Query().Get("cmd")
	switch {
	case r.URL.Path == "/get.cgi" && r.Method == http.MethodGet:
		s.get(w, r, cmd)
	case r.URL.Path == "/set.cgi" && r.Method == http.MethodPost:
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		s.set(w, r, cmd, body)
	default:
		http.NotFound(w, r)
	}
}

func (s *Server) set(w http.ResponseWriter, r *http.Request, cmd string, body []byte) {
	switch cmd {
	case "login":
		s.logins++
		var req struct{ Username, Password string }
		_ = json.Unmarshal(body, &req)
		var nonce string
		for n := range s.nonces {
			sum := sha256.Sum256([]byte(req.Username + ":" + n + ":" + Password))
			if req.Username == User && req.Password == hex.EncodeToString(sum[:]) {
				nonce = n
				break
			}
		}
		if nonce == "" {
			reply(w, 400, "fail", m{"errorCode": 1, "retryTimes": 4, "lockTime": 0})
			return
		}
		if len(s.tokens) > 0 { // one session per account
			reply(w, 400, "fail", m{"errorCode": 3, "retryTimes": 5, "lockTime": 0})
			return
		}
		delete(s.nonces, nonce)
		tok := randHex(16)
		s.tokens[tok] = true
		ok(w, m{"token": tok, "username": User, "firstLogin": 0, "errorCode": 0, "retryTimes": 0, "lockTime": 0})
	case "logout":
		s.logouts++
		delete(s.tokens, r.Header.Get("Authorization"))
		reply(w, 200, "success", nil)
	default:
		s.writes = append(s.writes, Write{Cmd: cmd, Body: string(body)})
		reply(w, 403, "forbidden: the fake switch does not accept writes", nil)
	}
}

func (s *Server) get(w http.ResponseWriter, r *http.Request, cmd string) {
	if cmd == "get_nonce" {
		n := randHex(6)
		s.nonces[n] = true
		ok(w, m{"nonce": n})
		return
	}
	if !s.tokens[r.Header.Get("Authorization")] {
		reply(w, 401, "unauthorized", nil)
		return
	}
	index, _ := strconv.Atoi(r.URL.Query().Get("index"))
	switch cmd {
	case "get_product_name":
		ok(w, m{"productName": "GWN7721", "devName": "GWN7721"})
	case "get_sysinfo":
		ok(w, m{"devName": "GWN7721", "sysLocation": "", "sysContact": "", "macAddr": "02:00:00:00:00:10",
			"ipAddr": "192.0.2.10", "netMask": "255.255.255.0", "gateway": "192.0.2.1", "sysVer": "1.0.7.128",
			"upTime": 272, "bootVer": "1.0.23", "devPN": "TESTPN", "hwVer": "V1.1A", "devSN": "TEST0001",
			"sysObjID": "1.3.6.1.4.1.42397"})
	case "get_port":
		ok(w, m{"list": portList(), "count": 10, "remain": 0})
	case "get_8021q_vlan":
		s.vlanPage(w, index)
	case "get_8021q_pvid":
		ok(w, m{"count": 10, "list": pvidList()})
	case "get_vlan_state":
		en := 0
		if s.portBased {
			en = 1
		}
		ok(w, m{"enable": en})
	case "get_lag_list":
		ok(w, m{"list": []m{}})
	case "get_lldp_nei_info":
		ok(w, m{"list": []m{{"localPort": 9, "ttl": 91, "chassisType": "MacAddr", "chassisId": "02:00:00:00:00:01",
			"portIdType": "Interface Name", "portId": "1/47", "sysName": "core-1", "sysDescp": "TEST-CORE (1.0)"}}})
	case "get_mib_state":
		ok(w, m{"list": mibList(), "count": 10, "remain": 0})
	default:
		reply(w, 404, "unknown command", nil)
	}
}
