// Package mockswitch imitates the embedded web server of an ERS 5600 switch
// ("Rapid Logic/1.1") closely enough to test ERS-Proxy without hardware: the
// login with the auth cookie, the /jsp/ SNMP bridge and static files.
//
// It serves files from any fs.FS. Tests use small synthetic files; the
// browser test uses a local copy of a real EDM webroot, which is never part
// of this repository.
package mockswitch

import (
	"bytes"
	"compress/gzip"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"sync"
)

// Values answers getVariables requests by MIB name.
var Values = map[string]string{
	"sysDescr":                    "Ethernet Routing Switch 5698TFD-PWR   HW:07       FW:6.0.0.18   SW:v6.6.3.015 BN:15 (c) Avaya Networks",
	"sysObjectID":                 "1.3.6.1.4.1.45.3.80.1",
	"sysName":                     "mock-ers5698",
	"sysContact":                  "admin",
	"sysLocation":                 "lab",
	"sysUpTime":                   "123456",
	"bsEdmInactivityTimeout":      "900",
	"s5AgSysSpanningTreeOperMode": "1",
	"s5AgSysManagementVlanId":     "1",
	"dot1dBaseNumPorts":           "98",
	"authUserAccessLevel":         "ReadWrite",
	"s5ChasType":                  "1.3.6.1.4.1.45.1.6.1.3.80.1",
}

// Options change the behaviour of the mock.
type Options struct {
	// Gzip sends HTML, JS and CSS gzip-compressed regardless of Accept-Encoding,
	// as the firmware stores them compressed.
	Gzip bool
	// ChangePassword answers the login check with
	// "HTTP/1.1 205 Change Password" (the account must change its password).
	ChangePassword bool
	// Password, if set, is the only password the login accepts. Like the real
	// switch, a wrong one gets 200 with an "Authentication Failed!" page.
	Password string
	// Log receives one JSON line per request (optional).
	Log io.Writer
	// Data answers the SNMP bridge from an .snmprec file (optional; else the
	// small built-in Values).
	Data *SNMPData
	// MaxSessions limits the logins that are active at the same time
	// (0 = unlimited). Further logins get "503 Server Too Busy", as VSP
	// switches answer when their few web sessions are used up. A logout
	// frees one.
	MaxSessions int
}

// Mock is the fake switch web server.
type Mock struct {
	root fs.FS
	opts Options

	mu       sync.Mutex
	sessions map[string]bool
	logMu    sync.Mutex
	// Requests counts requests per path (for tests).
	Requests map[string]int
	// LastCookie is the Cookie header of the last request (for tests).
	LastCookie string
	// LastClose reports whether the last request asked to close the
	// connection ("Connection: close", for tests).
	LastClose bool
	// Sets records the varNames of setVariables requests (for tests);
	// SetValues the matching varValues.
	Sets      []string
	SetValues []string
	// SetError, if set, is called with the variable names of every
	// setVariables request. A non-empty result is sent as the answer and
	// nothing is applied, e.g. "snmpErrorStatus:10\r\nsnmpErrorIndex:1\r\n".
	SetError func(names []string) string
}

// New returns a mock serving root.
func New(root fs.FS, opts Options) *Mock {
	return &Mock{root: root, opts: opts, sessions: map[string]bool{}, Requests: map[string]int{}}
}

func nonce() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func (m *Mock) log(kv map[string]any) {
	if m.opts.Log == nil {
		return
	}
	m.logMu.Lock()
	defer m.logMu.Unlock()
	_ = json.NewEncoder(m.opts.Log).Encode(kv)
}

func (m *Mock) authCookie(r *http.Request) string {
	if c, err := r.Cookie("auth"); err == nil {
		return c.Value
	}
	return ""
}

func (m *Mock) loggedIn(r *http.Request) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.sessions[m.authCookie(r)]
}

// ServeHTTP implements the switch web server.
func (m *Mock) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Server", "Rapid Logic/1.1")
	w.Header().Set("Cache-Control", "no-cache")
	m.mu.Lock()
	m.Requests[r.URL.Path]++
	m.LastCookie = r.Header.Get("Cookie")
	m.LastClose = r.Close
	m.mu.Unlock()
	// Like the real web server (Rapid Logic/1.1, ERS 5600 firmware 6.6): any
	// request with "Cache-Control: max-age=0" gets an empty 304, even a POST
	// without validators. Browsers send this header on form submits and reloads.
	if strings.Contains(r.Header.Get("Cache-Control"), "max-age=0") {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		m.get(w, r)
	case http.MethodPost:
		m.post(w, r)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (m *Mock) get(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Path
	m.log(map[string]any{"t": "get", "path": p, "cookie": r.Header.Get("Cookie")})
	if (p == "/" || p == "/index.html") && !m.loggedIn(r) {
		n := nonce()
		http.SetCookie(w, &http.Cookie{Name: "auth", Value: n})
		m.file(w, r, "login.html")
		return
	}
	if p == "/logout.html" {
		m.logout(r)
	}
	if p == "/" {
		p = "/index.html"
	}
	m.file(w, r, strings.TrimPrefix(path.Clean(p), "/"))
}

func (m *Mock) file(w http.ResponseWriter, r *http.Request, name string) {
	data, err := fs.ReadFile(m.root, name)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	ctype := mime.TypeByExtension(path.Ext(name))
	if strings.HasSuffix(name, ".js") {
		ctype = "text/javascript"
	}
	if ctype == "" {
		ctype = "application/octet-stream"
	}
	w.Header().Set("Content-Type", ctype)
	if m.opts.Gzip && (strings.HasPrefix(ctype, "text/") || strings.Contains(ctype, "javascript")) {
		var buf bytes.Buffer
		zw := gzip.NewWriter(&buf)
		_, _ = zw.Write(data)
		_ = zw.Close()
		data = buf.Bytes()
		w.Header().Set("Content-Encoding", "gzip")
	}
	if r.Method == http.MethodHead {
		return
	}
	_, _ = w.Write(data)
}

func (m *Mock) post(w http.ResponseWriter, r *http.Request) {
	// Parse the body as form data whatever the Content-Type says: EDM's login
	// check sends it with XMLHttpRequest.send(string), i.e. as text/plain.
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	// Like the switch, accept what can be parsed (EDM does not encode the
	// password in the login check).
	q, _ := url.ParseQuery(string(raw))
	p := r.URL.Path
	m.log(map[string]any{"t": "post", "path": p, "command": q.Get("command"), "varNames": q.Get("varNames"),
		"nonce": q.Get("nonce"), "hasURL": q.Has("URL"), "cookie": r.Header.Get("Cookie")})
	if p == "/" {
		if q.Has("encoded") && !q.Has("URL") {
			// XHR login check: "encoded=<len2><user>:<pass>&nonce=<nonce>".
			if q.Get("nonce") == "" || q.Get("nonce") != m.authCookie(r) {
				http.Error(w, "Not Authorized", http.StatusUnauthorized)
				return
			}
			if m.opts.Password != "" {
				enc := q.Get("encoded")
				if i := strings.Index(enc, ":"); i < 0 || enc[i+1:] != m.opts.Password {
					reply(w, `<html><body><b>Authentication Failed!</b> <a href="login.html">Return to Login page</a></body></html>`)
					return
				}
			}
			if m.opts.ChangePassword && !q.Has("encoded2") {
				// Go's server cannot send a custom reason phrase; write it raw.
				if hj, ok := w.(http.Hijacker); ok {
					conn, buf, err := hj.Hijack()
					if err == nil {
						_, _ = buf.WriteString("HTTP/1.1 205 Change Password\r\nServer: Rapid Logic/1.1\r\n" +
							"Content-Length: 0\r\nConnection: close\r\n\r\n")
						_ = buf.Flush()
						_ = conn.Close()
						return
					}
				}
				w.WriteHeader(http.StatusResetContent)
				return
			}
			m.mu.Lock()
			if max := m.opts.MaxSessions; max > 0 && !m.sessions[q.Get("nonce")] && len(m.sessions) >= max {
				m.mu.Unlock()
				http.Error(w, "Server Too Busy", http.StatusServiceUnavailable)
				return
			}
			m.sessions[q.Get("nonce")] = true
			m.mu.Unlock()
			reply(w, "OK")
			return
		}
		if !m.loggedIn(r) {
			m.file(w, r, "login.html")
			return
		}
		m.file(w, r, "index.html")
		return
	}
	if strings.HasPrefix(p, "/jsp/") {
		if !m.loggedIn(r) || q.Get("userId") != m.authCookie(r) {
			http.Error(w, "Not Authorized", http.StatusUnauthorized)
			return
		}
		m.jsp(w, q)
		return
	}
	http.Error(w, "not found", http.StatusNotFound)
}

// logout ends the login of the request's auth cookie.
func (m *Mock) logout(r *http.Request) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.sessions, m.authCookie(r))
}

// set answers setVariables: variables are joined with a TAB in varNames and
// varValues.
func (m *Mock) set(w http.ResponseWriter, q url.Values) {
	names, values := q.Get("varNames"), q.Get("varValues")
	m.mu.Lock()
	m.Sets = append(m.Sets, names)
	m.SetValues = append(m.SetValues, values)
	fail := m.SetError
	m.mu.Unlock()
	nl, vl := strings.Split(names, "\t"), strings.Split(values, "\t")
	if fail != nil {
		if ans := fail(nl); ans != "" {
			reply(w, ans)
			return
		}
	}
	if m.opts.Data == nil {
		reply(w, setAnswer(nl, vl))
		return
	}
	if len(nl) != len(vl) || q.Get("varCount") != strconv.Itoa(len(nl)) {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	vars := make([]SetVar, len(nl))
	for i := range nl {
		vars[i] = SetVar{Name: nl[i], Value: vl[i]}
	}
	if err := m.opts.Data.Apply(vars); err != nil {
		var f *SetFailure
		if errors.As(err, &f) {
			// As a VSP answers (VOSS 8.10): status and index, no message.
			reply(w, fmt.Sprintf("snmpErrorStatus:%d\r\nsnmpErrorIndex:%d\r\n", f.Status, f.Index))
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	reply(w, setAnswer(nl, vl))
}

// setAnswer is the answer of a VSP to a successful setVariables: each name
// without its index and the value written, "ifAlias:uplink\r\n".
func setAnswer(names, values []string) string {
	var sb strings.Builder
	for i, n := range names {
		base, _, _ := strings.Cut(n, ".")
		v := ""
		if i < len(values) {
			v = values[i]
		}
		sb.WriteString(base + ":" + v + "\r\n")
	}
	return sb.String()
}

func (m *Mock) jsp(w http.ResponseWriter, q url.Values) {
	cmd, names := q.Get("command"), q.Get("varNames")
	if d := m.opts.Data; d != nil {
		switch cmd {
		case "getVariables", "getNextVariables":
			reply(w, d.scalars(names))
			return
		case "getTableVariables":
			if out, ok := d.table(names); ok {
				reply(w, out)
				return
			}
		}
	}
	switch cmd {
	case "getVariables", "getNextVariables":
		var out []string
		for _, nm := range strings.Split(names, ",") {
			if nm == "" {
				continue
			}
			base := strings.SplitN(nm, ".", 2)[0]
			switch {
			case base == "s5ChasComType" && !strings.HasPrefix(nm, "s5ChasComType.3.10."):
				out = append(out, base+":noSuchInstance")
			case base == "s5ChasComType":
				out = append(out, base+":1.3.6.1.4.1.45.1.6.1.3.781")
			default:
				v, ok := Values[base]
				if !ok {
					v = "0"
				}
				out = append(out, base+":"+v)
			}
		}
		reply(w, strings.Join(out, "\r\n")+"\r\n")
	case "getTableVariables":
		if strings.HasPrefix(names, "s5ChasComType.3.10") {
			reply(w, `[["s5ChasComType.3.10.0","1.3.6.1.4.1.45.3.80.781"]]`)
			return
		}
		reply(w, "[]")
	case "setVariables":
		m.set(w, q)
	case "logout":
		reply(w, "")
	default:
		reply(w, "")
	}
}

func reply(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "text/html")
	_, _ = io.WriteString(w, body)
}

// ResetSessions forgets all logins, like a switch whose web sessions timed out.
func (m *Mock) ResetSessions() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sessions = map[string]bool{}
}
