package edmproxy

import (
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lan-dot-party/ers-proxy/internal/auth"
	"github.com/lan-dot-party/ers-proxy/internal/store"
)

// maxPerSession limits proxy sessions per login session (every opened or
// reloaded tab creates one).
const maxPerSession = 64

// proxySession is one opened device: the token in /d/{token}/ and the
// switch's cookies. It belongs to one login session and one device.
type proxySession struct {
	token    string
	loginID  string // store.Session.ID (hash of the portal cookie)
	userID   int64
	username string
	device   *store.Device
	created  time.Time
	jar      *jar
	// lastUsed and validated (last check that the login session still exists)
	// are Unix nanoseconds, updated by concurrent requests.
	lastUsed  atomic.Int64
	validated atomic.Int64
}

func (s *proxySession) idle(now time.Time) time.Duration {
	return now.Sub(time.Unix(0, s.lastUsed.Load()))
}

// registry holds the proxy sessions in memory.
type registry struct {
	mu    sync.Mutex
	byTok map[string]*proxySession
}

func newRegistry() *registry { return &registry{byTok: map[string]*proxySession{}} }

// create adds a session and evicts the oldest ones of the same login session
// beyond maxPerSession.
func (r *registry) create(loginID string, u *store.User, d *store.Device) *proxySession {
	now := time.Now()
	s := &proxySession{token: auth.RandomToken(16), loginID: loginID, userID: u.ID, username: u.Username,
		device: d, created: now, jar: newJar()}
	s.lastUsed.Store(now.UnixNano())
	s.validated.Store(now.UnixNano())
	r.mu.Lock()
	defer r.mu.Unlock()
	r.byTok[s.token] = s
	var mine []*proxySession
	for _, o := range r.byTok {
		if o.loginID == loginID {
			mine = append(mine, o)
		}
	}
	if len(mine) > maxPerSession {
		sort.Slice(mine, func(i, j int) bool { return mine[i].lastUsed.Load() < mine[j].lastUsed.Load() })
		for _, o := range mine[:len(mine)-maxPerSession] {
			delete(r.byTok, o.token)
		}
	}
	return s
}

func (r *registry) get(token string) *proxySession {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.byTok[token]
}

func (r *registry) remove(token string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.byTok, token)
}

// expire removes sessions idle for longer than idle.
func (r *registry) expire(idle time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	for t, s := range r.byTok {
		if s.idle(now) > idle {
			delete(r.byTok, t)
		}
	}
}

// removeDevice ends all sessions of a device (deleted or changed).
func (r *registry) removeDevice(id int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for t, s := range r.byTok {
		if s.device.ID == id {
			delete(r.byTok, t)
		}
	}
}

func (r *registry) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.byTok)
}

// jar keeps the cookies a switch sets, on the server. They are never sent to
// the browser; the shim exposes non-HttpOnly ones to document.cookie.
type jar struct {
	mu      sync.Mutex
	cookies map[string]*http.Cookie
}

func newJar() *jar { return &jar{cookies: map[string]*http.Cookie{}} }

// store takes the Set-Cookie headers of a response.
func (j *jar) store(resp *http.Response) {
	j.mu.Lock()
	defer j.mu.Unlock()
	now := time.Now()
	for _, c := range resp.Cookies() {
		if c.MaxAge < 0 || (!c.Expires.IsZero() && c.Expires.Before(now)) {
			delete(j.cookies, c.Name)
			continue
		}
		j.cookies[c.Name] = &http.Cookie{Name: c.Name, Value: c.Value, HttpOnly: c.HttpOnly}
	}
}

// value returns the value of one cookie ("" if not set).
func (j *jar) value(name string) string {
	j.mu.Lock()
	defer j.mu.Unlock()
	if c := j.cookies[name]; c != nil {
		return c.Value
	}
	return ""
}

// header returns the Cookie header for a request to the switch.
func (j *jar) header() string {
	j.mu.Lock()
	defer j.mu.Unlock()
	names := make([]string, 0, len(j.cookies))
	for n := range j.cookies {
		names = append(names, n)
	}
	sort.Strings(names)
	parts := make([]string, 0, len(names))
	for _, n := range names {
		parts = append(parts, n+"="+j.cookies[n].Value)
	}
	return strings.Join(parts, "; ")
}

// script returns what document.cookie shows: cookies without HttpOnly.
func (j *jar) script() string {
	j.mu.Lock()
	defer j.mu.Unlock()
	names := make([]string, 0, len(j.cookies))
	for n, c := range j.cookies {
		if !c.HttpOnly {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	parts := make([]string, 0, len(names))
	for _, n := range names {
		parts = append(parts, n+"="+j.cookies[n].Value)
	}
	return strings.Join(parts, "; ")
}

// set stores a cookie written by page script (document.cookie = …), so the
// switch sees it on the next request like it would without the proxy.
func (j *jar) set(raw string) {
	first := strings.TrimSpace(strings.SplitN(raw, ";", 2)[0])
	name, value, ok := strings.Cut(first, "=")
	name = strings.TrimSpace(name)
	if !ok || name == "" || len(name) > 64 || len(value) > 4096 {
		return
	}
	lower := strings.ToLower(raw)
	j.mu.Lock()
	defer j.mu.Unlock()
	if strings.Contains(lower, "max-age=0") || strings.Contains(lower, "max-age=-") || strings.Contains(lower, "expires=thu, 01 jan 1970") {
		delete(j.cookies, name)
		return
	}
	if len(j.cookies) >= 50 {
		return
	}
	j.cookies[name] = &http.Cookie{Name: name, Value: value}
}
