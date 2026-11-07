// Package swostest is a fake MikroTik SwOS web server for tests. Its files
// follow the format of SwOS 2.18 (CRS305-1G-4S+); all values are made up.
package swostest

import (
	"crypto/md5" //nolint:gosec // HTTP Digest as SwOS implements it
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
)

// Credentials accepted by the fake switch.
const (
	User     = "admin"
	Password = "test-password"
	Realm    = "CSS305-1G-4S+"
)

// hexText encodes a string the way SwOS does.
func hexText(s string) string { return hex.EncodeToString([]byte(s)) }

// Files returns the files of a five-port switch: Port1 (copper, 1G, up,
// access VLAN 20), SFP1 (10G, up, trunk with VLANs 10 and 20, LAG 1), SFP2
// (10G, up, LAG 1), SFP3 (no link), SFP4 (disabled).
func Files() map[string]string {
	return map[string]string{
		"/sys.b": fmt.Sprintf(`{upt:0x00012345,mac:'020000000001',brd:'%s',sid:'%s',id:'%s',ver:'%s',bld:0x68000000,temp:0x0000001e}`,
			hexText("CRS305-1G-4S+"), hexText("TEST0001"), hexText("swos-test"), hexText("2.18")),
		"/link.b": fmt.Sprintf(`{en:0x0f,an:0x1f,lnk:0x07,dpx:0x07,spd:[0x02,0x03,0x03,0x07,0x07],prt:0x05,sfp:0x04,sfpo:0x01,`+
			`nm:['%s','%s','%s','',''],flt:[0x00,0x00,0x00,0x00,0x00]}`, hexText("office"), hexText("uplink"), hexText("uplink-2")),
		"/fwd.b": `{fp1:0x1e,fp2:0x1d,fp3:0x1b,fp4:0x17,fp5:0x0f,vlan:[0x03,0x03,0x03,0x01,0x00],vlni:[0x02,0x01,0x01,0x00,0x00],` +
			`dvid:[0x0014,0x0001,0x0001,0x000a,0x0001],fvid:0x00}`,
		"/vlan.b": fmt.Sprintf(`[{nm:'%s',mbr:0x0e,vid:0x000a,piso:0x01},{nm:'%s',mbr:0x07,vid:0x0014,piso:0x01}]`,
			hexText("servers"), hexText("office")),
		"/lacp.b": `{mode:[0x00,0x01,0x01,0x00,0x00],grp:[0x00,0x01,0x01,0x00,0x00],sgrp:[0x00,0x00,0x00,0x00,0x00],` +
			`mac:['000000000000','020000000002','020000000002','000000000000','000000000000']}`,
		"/stats.b": `{rb:[0x00001000,0x00000010,0x00002000,0x00000000,0x00000000],rbh:[0x00000000,0x00000001,0x00000000,0x00000000,0x00000000],` +
			`tb:[0x00000800,0x00000020,0x00003000,0x00000000,0x00000000],tbh:[0x00000000,0x00000002,0x00000000,0x00000000,0x00000000],` +
			`rfcs:[0x00000001,0x00000000,0x00000000,0x00000000,0x00000000],rov:[0x00000000,0x00000003,0x00000000,0x00000000,0x00000000],` +
			`tur:[0x00000000,0x00000000,0x00000002,0x00000000,0x00000000]}`,
	}
}

// Server is the fake switch. The index page and redirects need no login;
// the .b files and POSTs do (HTTP Digest, like SwOS).
type Server struct {
	mu    sync.Mutex
	files map[string]string
	nonce string
	// Requests counts requests per path.
	requests map[string]int
	// Posts records the bodies of POST requests by path.
	posts map[string][]string
	// Auth records the Authorization header of every request.
	auth []string
}

// New returns a fake switch serving Files().
func New() *Server {
	return &Server{files: Files(), nonce: "5a5a5a5a", requests: map[string]int{}, posts: map[string][]string{}}
}

// SetFile replaces a file.
func (s *Server) SetFile(path, content string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.files[path] = content
}

// SetNonce changes the nonce (old signatures are refused).
func (s *Server) SetNonce(n string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nonce = n
}

// Requests returns how often a path was requested.
func (s *Server) Requests(path string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.requests[path]
}

// Posts returns the bodies posted to a path.
func (s *Server) Posts(path string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.posts[path]...)
}

// AuthHeaders returns the Authorization headers seen so far.
func (s *Server) AuthHeaders() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.auth...)
}

// Index is the page served at /index.html.
const Index = `<!doctype html><html><head><title>MikroTik SwOS</title></head><body><div id=status>Loading...</div>` +
	`<script>var x=new XMLHttpRequest();x.open("GET","/sys.b",true);x.send(null);</script></body></html>`

func md5hex(s string) string {
	h := md5.Sum([]byte(s)) //nolint:gosec // HTTP Digest
	return hex.EncodeToString(h[:])
}

func (s *Server) authorized(r *http.Request) bool {
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, "Digest ") {
		return false
	}
	p := map[string]string{}
	for _, part := range strings.Split(strings.TrimPrefix(h, "Digest "), ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		if ok {
			p[k] = strings.Trim(v, `"`)
		}
	}
	ha1 := md5hex(User + ":" + Realm + ":" + Password)
	ha2 := md5hex(r.Method + ":" + p["uri"])
	want := md5hex(ha1 + ":" + s.nonce + ":" + p["nc"] + ":" + p["cnonce"] + ":auth:" + ha2)
	return p["username"] == User && p["nonce"] == s.nonce && p["uri"] == r.URL.RequestURI() && p["response"] == want
}

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests[r.URL.Path]++
	s.auth = append(s.auth, r.Header.Get("Authorization"))
	switch r.URL.Path {
	case "/":
		w.Header().Set("Location", "/index.html")
		w.WriteHeader(http.StatusSeeOther)
		return
	case "/index.html":
		w.Header().Set("Content-Type", "text/html")
		_, _ = io.WriteString(w, Index)
		return
	}
	if !s.authorized(r) {
		w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Digest realm="%s", qop="auth", nonce="%s", stale=FALSE`, Realm, s.nonce))
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, "<h1>401 Unauthorized</h1>")
		return
	}
	if r.Method == http.MethodPost {
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		s.posts[r.URL.Path] = append(s.posts[r.URL.Path], string(body))
		w.WriteHeader(http.StatusOK)
		return
	}
	f, ok := s.files[r.URL.Path]
	if !ok {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/x-javascript")
	_, _ = io.WriteString(w, f)
}
