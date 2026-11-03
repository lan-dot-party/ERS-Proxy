// Package edmproxy serves the web interface (EDM) of ERS switches below
// /d/{token}/: it forwards requests to the switch, keeps the switch's cookies
// on the server, patches EDM for modern browsers and isolates the content in
// a browser sandbox. See docs/security.md and docs/architecture.md.
package edmproxy

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"html/template"
	"io"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/lan-dot-party/ers-proxy/internal/app"
	"github.com/lan-dot-party/ers-proxy/internal/devices"
	"github.com/lan-dot-party/ers-proxy/internal/edmproxy/patches"
	"github.com/lan-dot-party/ers-proxy/internal/store"
	"github.com/lan-dot-party/ers-proxy/internal/upstream"
)

// sandboxCSP isolates proxied documents: an opaque origin without access to
// the portal or to other devices. No allow-same-origin, ever.
const sandboxCSP = "sandbox allow-scripts allow-forms allow-popups allow-modals allow-downloads; frame-ancestors 'self'"

// Proxy is the EDM proxy.
type Proxy struct {
	app *app.App
	log *slog.Logger
	reg *registry

	mu      sync.Mutex
	clients map[int64]*clientEntry
	diag    map[int64]*Diag
	images  *imageCache
}

type clientEntry struct {
	sig string
	c   *upstream.Client
}

// Diag is what the proxy knows about a device since the start.
type Diag struct {
	LastOpened  time.Time
	Server      string
	Patches     map[string]int
	Ambiguous   map[string]bool
	TLSVersion  string
	TLSCipher   string
	TLSProfile  string
	LastError   string
	LastErrorAt time.Time
	Requests    int
	// AutoLogin is the time of the last successful automatic login.
	AutoLogin time.Time
}

// New creates the proxy and starts the expiry of idle proxy sessions.
func New(ctx context.Context, a *app.App) *Proxy {
	p := &Proxy{app: a, log: a.Log.With("component", "edmproxy"), reg: newRegistry(),
		clients: map[int64]*clientEntry{}, diag: map[int64]*Diag{}, images: newImageCache()}
	go func() {
		t := time.NewTicker(time.Minute)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				p.reg.expire(a.Cfg.Proxy.IdleTimeout)
			}
		}
	}()
	return p
}

// Open creates a proxy session for the logged-in user and redirects to it.
func (p *Proxy) Open(w http.ResponseWriter, r *http.Request, d *store.Device) {
	sess, u := app.Session(r.Context()), app.User(r.Context())
	if sess == nil || u == nil {
		http.Error(w, "login required", http.StatusUnauthorized)
		return
	}
	ps := p.reg.create(sess.ID, u, d)
	p.withDiag(d.ID, func(dg *Diag) { dg.LastOpened = time.Now() })
	if d.AutoLogin && d.HasCredentials() {
		p.tryAutoLogin(r.Context(), ps)
	}
	p.log.Debug("device opened", "device", d.Name, "user", u.Username)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	http.Redirect(w, r, "/d/"+ps.token+"/", http.StatusSeeOther)
}

// tryAutoLogin logs in with the stored credentials. A failure is recorded in
// the diagnostics; the user then simply sees the EDM login page.
func (p *Proxy) tryAutoLogin(ctx context.Context, ps *proxySession) {
	d := ps.device
	cred, err := devices.DecryptCredentials(p.app.Key, d)
	if err == nil {
		var c *upstream.Client
		if c, err = p.client(d); err == nil {
			ctx, cancel := context.WithTimeout(ctx, p.app.Cfg.Proxy.RequestTimeout)
			defer cancel()
			err = p.autoLogin(ctx, ps, c, cred)
		}
	}
	if err != nil {
		p.log.Warn("auto-login failed", "device", d.Name, "err", err)
		p.withDiag(d.ID, func(dg *Diag) { dg.LastError, dg.LastErrorAt = "auto-login: "+err.Error(), time.Now() })
		return
	}
	p.withDiag(d.ID, func(dg *Diag) { dg.AutoLogin = time.Now() })
}

// ForgetDevice ends all proxy sessions of a device and drops its client
// (after the device was changed or deleted).
func (p *Proxy) ForgetDevice(id int64) {
	p.reg.removeDevice(id)
	p.images.forget(id)
	p.mu.Lock()
	delete(p.clients, id)
	p.mu.Unlock()
}

// Sessions returns the number of open proxy sessions.
func (p *Proxy) Sessions() int { return p.reg.count() }

// Diagnostics returns a copy of the device's diagnostics (nil if never opened).
func (p *Proxy) Diagnostics(id int64) *Diag {
	p.mu.Lock()
	defer p.mu.Unlock()
	d := p.diag[id]
	if d == nil {
		return nil
	}
	c := *d
	c.Patches = map[string]int{}
	for k, v := range d.Patches {
		c.Patches[k] = v
	}
	c.Ambiguous = map[string]bool{}
	for k, v := range d.Ambiguous {
		c.Ambiguous[k] = v
	}
	return &c
}

// PatchNames returns the applied patches of a Diag in a stable order.
func (d *Diag) PatchNames() []string {
	out := make([]string, 0, len(d.Patches))
	for k := range d.Patches {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func (p *Proxy) withDiag(id int64, fn func(*Diag)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	d := p.diag[id]
	if d == nil {
		d = &Diag{Patches: map[string]int{}, Ambiguous: map[string]bool{}}
		p.diag[id] = d
	}
	fn(d)
}

// Client returns the upstream client of a device (TLS profile, pinning,
// connection limit), shared with the dashboard's EDM data source.
func (p *Proxy) Client(d *store.Device) (*upstream.Client, error) { return p.client(d) }

func (p *Proxy) client(d *store.Device) (*upstream.Client, error) {
	sig := d.URL + "|" + d.TLSMode + "|" + d.TLSPin
	p.mu.Lock()
	defer p.mu.Unlock()
	if e := p.clients[d.ID]; e != nil && e.sig == sig {
		return e.c, nil
	}
	id := d.ID
	c, err := upstream.New(upstream.Options{
		BaseURL:  d.URL,
		TLSMode:  d.TLSMode,
		Pin:      d.TLSPin,
		Profile:  d.TLSProfile,
		MaxConns: p.app.Cfg.Proxy.MaxConnsPerDevice,
		Timeout:  p.app.Cfg.Proxy.RequestTimeout,
		OnProfile: func(profile string) {
			p.log.Info("TLS profile selected", "device", d.Name, "profile", profile)
			if err := p.app.DB.SetDeviceTLSProfile(context.Background(), id, profile); err != nil {
				p.log.Warn("store TLS profile", "err", err)
			}
		},
	})
	if err != nil {
		return nil, err
	}
	p.clients[d.ID] = &clientEntry{sig: sig, c: c}
	return c, nil
}

// isolationHeaders sets the security headers of every response below /d/.
func isolationHeaders(h http.Header, sandbox bool) {
	if sandbox {
		h.Set("Content-Security-Policy", sandboxCSP)
		// The sandboxed document has an opaque origin ("null"); its XHR requests
		// to its own token path are cross-origin. No credentials are involved:
		// the unguessable token in the path is the only key.
		h.Set("Access-Control-Allow-Origin", "null")
		h.Set("Access-Control-Expose-Headers", "*")
	} else {
		h.Set("Content-Security-Policy", "frame-ancestors 'self'")
		h.Del("Access-Control-Allow-Origin")
		h.Del("Access-Control-Expose-Headers")
	}
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Del("X-Frame-Options")
	h.Del("Content-Security-Policy-Report-Only")
	h.Del("Strict-Transport-Security")
	h.Del("Clear-Site-Data")
	h.Del("Alt-Svc")
	h.Del("Access-Control-Allow-Credentials")
	h.Del("Access-Control-Allow-Methods")
	h.Del("Access-Control-Allow-Headers")
}

// sessionCheckInterval is how often a proxy session re-checks that the login
// session still exists (logout, password change, disabled user).
const sessionCheckInterval = 30 * time.Second

// ServeHTTP handles /d/{token}/….
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rawPath := r.URL.EscapedPath()
	rest := strings.TrimPrefix(rawPath, "/d/")
	token, sub, _ := strings.Cut(rest, "/")
	sub = "/" + sub
	ps := p.reg.get(token)
	if ps == nil || len(token) != 32 {
		isolationHeaders(w.Header(), true)
		p.message(w, r, http.StatusNotFound, "This device session has ended. Open the device again from the ERS-Proxy portal.")
		return
	}
	now := time.Now()
	if ps.idle(now) > p.app.Cfg.Proxy.IdleTimeout {
		p.reg.remove(token)
		isolationHeaders(w.Header(), true)
		p.message(w, r, http.StatusNotFound, "This device session has expired. Open the device again from the ERS-Proxy portal.")
		return
	}
	if now.Sub(time.Unix(0, ps.validated.Load())) > sessionCheckInterval {
		if !p.app.DB.SessionExists(r.Context(), ps.loginID) {
			p.reg.remove(token)
			isolationHeaders(w.Header(), true)
			p.message(w, r, http.StatusUnauthorized, "You are logged out of ERS-Proxy. Log in again and reopen the device.")
			return
		}
		ps.validated.Store(now.UnixNano())
	}
	ps.lastUsed.Store(now.UnixNano())
	sandbox := ps.device.Isolation != store.IsolationOff
	isolationHeaders(w.Header(), sandbox)
	base := "/d/" + token + "/"

	switch {
	case r.Method == http.MethodOptions:
		// CORS preflight of the sandboxed document (Ext sets X-Requested-With).
		if sandbox {
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, HEAD")
			if h := r.Header.Get("Access-Control-Request-Headers"); h != "" {
				w.Header().Set("Access-Control-Allow-Headers", h)
			}
			w.Header().Set("Access-Control-Max-Age", "600")
		}
		w.WriteHeader(http.StatusNoContent)
		return
	case sub == shimPath:
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = io.WriteString(w, shimJS(ps.jar.script(), base))
		return
	case sub == cookiePath && r.Method == http.MethodPost:
		data, err := io.ReadAll(io.LimitReader(r.Body, 4096))
		if err == nil {
			ps.jar.set(string(data))
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}

	if cacheableImage(r, sub) {
		if img, ok := p.images.get(ps.device.ID, sub); ok {
			w.Header().Set("Content-Type", img.ctype)
			w.Header().Set("Content-Length", strconv.Itoa(len(img.body)))
			w.Header().Set("Cache-Control", "private, max-age=86400")
			_, _ = w.Write(img.body)
			return
		}
	}

	c, err := p.client(ps.device)
	if err != nil {
		p.message(w, r, http.StatusBadGateway, err.Error())
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, p.app.Cfg.Proxy.MaxBodyBytes)
	if r.Method == http.MethodPost && strings.HasPrefix(sub, "/jsp/") {
		var err error
		if r, err = markEDMRead(r); err != nil {
			p.message(w, r, http.StatusRequestEntityTooLarge, "request body too large")
			return
		}
	}
	// The reverse proxy adds the (rewritten) upstream headers with Add; clear
	// what was set above so that no header appears twice (a duplicated
	// Access-Control-Allow-Origin makes the browser reject the response).
	for k := range w.Header() {
		w.Header().Del(k)
	}
	target := c.Base()
	rp := &httputil.ReverseProxy{
		Transport: c.Transport,
		Rewrite: func(pr *httputil.ProxyRequest) {
			u := *target
			u.RawPath = sub
			u.Path, _ = url.PathUnescape(sub)
			u.RawQuery = pr.In.URL.RawQuery
			pr.Out.URL = &u
			pr.Out.Host = target.Host
			h := pr.Out.Header
			for _, k := range []string{"Cookie", "Origin", "Referer", "Accept-Encoding", "Forwarded", "X-Forwarded-For",
				"X-Forwarded-Host", "X-Forwarded-Proto", "X-Real-Ip", "Sec-Fetch-Site", "Sec-Fetch-Mode", "Sec-Fetch-Dest",
				"Sec-Fetch-User", "Upgrade-Insecure-Requests", "Priority",
				// The switch answers every request carrying "Cache-Control: max-age=0"
				// (form submits, reloads) with an empty 304, even a POST.
				"Cache-Control", "Pragma"} {
				h.Del(k)
			}
			if ck := ps.jar.header(); ck != "" {
				h.Set("Cookie", ck)
			}
		},
		ModifyResponse: func(resp *http.Response) error {
			return p.modifyResponse(resp, r, ps, c, base, sandbox, sub)
		},
		ErrorHandler: func(w http.ResponseWriter, req *http.Request, err error) {
			isolationHeaders(w.Header(), sandbox)
			p.upstreamError(w, req, ps, err)
		},
		ErrorLog: slog.NewLogLogger(p.log.Handler(), slog.LevelDebug),
	}
	rp.ServeHTTP(w, r)
}

// markEDMRead buffers the body of a request to the EDM bridge and, if the
// command only reads, marks it as safe to repeat when the switch drops the
// connection (see upstream/retry.go). Writes are never repeated.
func markEDMRead(r *http.Request) (*http.Request, error) {
	data, err := io.ReadAll(r.Body)
	r.Body.Close()
	if err != nil {
		return r, err
	}
	r.Body = io.NopCloser(bytes.NewReader(data))
	r.ContentLength = int64(len(data))
	r.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(data)), nil }
	q, _ := url.ParseQuery(string(data))
	if upstream.EDMReadCommand(q.Get("command")) {
		r = r.WithContext(upstream.WithRetry(r.Context()))
	}
	return r, nil
}

// document reports whether the browser loads the response as a page (not XHR).
func document(r *http.Request, sub string) bool {
	switch r.Header.Get("Sec-Fetch-Dest") {
	case "document", "iframe", "frame":
		return true
	case "":
		return sub == "/" || strings.HasSuffix(sub, ".html") || strings.HasSuffix(sub, ".htm")
	}
	return false
}

func (p *Proxy) modifyResponse(resp *http.Response, in *http.Request, ps *proxySession, c *upstream.Client, base string,
	sandbox bool, sub string) error {
	ps.jar.store(resp)
	h := resp.Header
	h.Del("Set-Cookie")
	if loc := h.Get("Location"); loc != "" {
		h.Set("Location", rewriteLocation(loc, c.Base(), base))
	}
	if reason := upstream.ReasonPhrase(resp); reason != "" {
		h.Set("X-ERSProxy-Status-Text", sanitizeHeader(reason))
	}
	isolationHeaders(h, sandbox)

	server := h.Get("Server")
	p.withDiag(ps.device.ID, func(d *Diag) {
		d.Requests++
		if server != "" {
			d.Server = server
		}
		if st := c.ConnectionState(); st != nil {
			d.TLSVersion = tls.VersionName(st.Version)
			d.TLSCipher = tls.CipherSuiteName(st.CipherSuite)
			d.TLSProfile = c.Profile()
		}
	})

	ctype := h.Get("Content-Type")
	if resp.StatusCode == http.StatusOK && cacheableImage(in, sub) && h.Get("Content-Encoding") == "" &&
		resp.ContentLength >= 0 && resp.ContentLength <= maxCachedImage {
		raw, err := io.ReadAll(io.LimitReader(resp.Body, maxCachedImage+1))
		resp.Body.Close()
		if err != nil {
			return fmt.Errorf("read image: %w", err)
		}
		p.images.put(ps.device.ID, sub, ctype, raw)
		resp.Body = io.NopCloser(bytes.NewReader(raw))
		return nil
	}
	isDoc := document(in, sub)
	dest := in.Header.Get("Sec-Fetch-Dest")
	patchIt := patches.Patchable(ctype, sub) && (isDoc || dest == "script" || (dest == "" && strings.HasSuffix(sub, ".js")))
	if !patchIt || resp.StatusCode == http.StatusNotModified || in.Method == http.MethodHead {
		return nil
	}
	limit := p.app.Cfg.Proxy.MaxBodyBytes
	raw, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	resp.Body.Close()
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}
	if int64(len(raw)) > limit {
		return errors.New("response too large to patch")
	}
	if strings.EqualFold(h.Get("Content-Encoding"), "gzip") || (len(raw) > 2 && raw[0] == 0x1f && raw[1] == 0x8b) {
		zr, err := gzip.NewReader(bytes.NewReader(raw))
		if err != nil {
			return fmt.Errorf("gunzip: %w", err)
		}
		raw, err = io.ReadAll(io.LimitReader(zr, limit+1))
		if err != nil {
			return fmt.Errorf("gunzip: %w", err)
		}
		if int64(len(raw)) > limit {
			return errors.New("response too large to patch")
		}
		h.Del("Content-Encoding")
	}
	// EDM files are Latin-1 or ASCII; patching works on bytes via string
	// conversion without decoding, so no characters are changed.
	body, res := patches.Apply(string(raw), base)
	if isDoc && patches.IsHTML(ctype, sub) {
		body = patches.InjectScript(body, base+strings.TrimPrefix(shimPath, "/"))
		h.Set("Cache-Control", "no-store")
	}
	if len(res.Applied) > 0 || len(res.Ambiguous) > 0 {
		p.withDiag(ps.device.ID, func(d *Diag) {
			for k, v := range res.Applied {
				d.Patches[k] += v
			}
			for _, k := range res.Ambiguous {
				d.Ambiguous[k] = true
			}
		})
		p.log.Debug("patched", "device", ps.device.Name, "path", sub, "applied", res.Names(), "ambiguous", res.Ambiguous)
	}
	resp.Body = io.NopCloser(strings.NewReader(body))
	resp.ContentLength = int64(len(body))
	h.Set("Content-Length", strconv.Itoa(len(body)))
	h.Del("Etag")
	h.Del("Last-Modified")
	return nil
}

// rewriteLocation maps redirects of the switch into the proxy path.
func rewriteLocation(loc string, target *url.URL, base string) string {
	u, err := url.Parse(loc)
	if err != nil {
		return base
	}
	if u.IsAbs() {
		if !strings.EqualFold(u.Host, target.Host) && !strings.EqualFold(u.Hostname(), target.Hostname()) {
			return loc // somewhere else entirely
		}
		u.Scheme, u.Host = "", ""
	}
	if strings.HasPrefix(u.Path, "/") {
		u.Path = strings.TrimSuffix(base, "/") + u.Path
		u.RawPath = ""
		return u.String()
	}
	return u.String() // relative: stays below the proxy path
}

func sanitizeHeader(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= 0x20 && r < 0x7f && b.Len() < 100 {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func (p *Proxy) upstreamError(w http.ResponseWriter, r *http.Request, ps *proxySession, err error) {
	msg := "The device did not answer: " + err.Error()
	var pm *upstream.PinMismatchError
	switch {
	case errors.As(err, &pm):
		msg = "The device presented a different HTTPS certificate than the confirmed one. " +
			"If the certificate was renewed on purpose, an admin can remove the pin in the device settings and confirm the new one."
	case errors.Is(err, upstream.ErrNotPinned):
		msg = "The HTTPS certificate of this device has not been confirmed. Open the device again from the portal."
	case errors.Is(err, context.DeadlineExceeded):
		msg = "The device did not answer in time."
	}
	p.withDiag(ps.device.ID, func(d *Diag) { d.LastError, d.LastErrorAt = err.Error(), time.Now() })
	p.log.Warn("upstream error", "device", ps.device.Name, "path", app.RedactPath(r.URL.Path), "err", err)
	p.message(w, r, http.StatusBadGateway, msg)
}

var messageTmpl = template.Must(template.New("m").Parse(`<!doctype html><html><head><meta charset="utf-8">` +
	`<title>ERS-Proxy</title></head><body><h3>ERS-Proxy</h3><p>{{.}}</p></body></html>`))

// message writes an error. Pages get a small HTML document, XHR plain text.
func (p *Proxy) message(w http.ResponseWriter, r *http.Request, status int, msg string) {
	w.Header().Set("Cache-Control", "no-store")
	if document(r, strings.TrimPrefix(r.URL.Path, "/d/")) || r.Header.Get("Sec-Fetch-Dest") == "document" {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(status)
		_ = messageTmpl.Execute(w, msg)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, msg)
}
