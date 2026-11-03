// Package app holds the shared services and request helpers used by the web
// UI and the EDM proxy.
package app

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync/atomic"
	"time"

	"github.com/lan-dot-party/ers-proxy/internal/auth"
	"github.com/lan-dot-party/ers-proxy/internal/config"
	"github.com/lan-dot-party/ers-proxy/internal/secret"
	"github.com/lan-dot-party/ers-proxy/internal/store"
)

// App bundles the long-lived services.
type App struct {
	Cfg *config.Config
	DB  *store.DB
	Key *secret.Key
	Log *slog.Logger

	trusted []netip.Prefix
	// frameSrc holds the extra CSP frame-src sources (origins of link devices
	// that may be embedded).
	frameSrc atomic.Value
}

// New creates the app. TrustedProxies are parsed once.
func New(cfg *config.Config, db *store.DB, key *secret.Key, log *slog.Logger) *App {
	a := &App{Cfg: cfg, DB: db, Key: key, Log: log}
	for _, p := range cfg.TrustedProxies {
		if pref, err := netip.ParsePrefix(p); err == nil {
			a.trusted = append(a.trusted, pref)
		} else if addr, err := netip.ParseAddr(p); err == nil {
			a.trusted = append(a.trusted, netip.PrefixFrom(addr, addr.BitLen()))
		}
	}
	a.frameSrc.Store("")
	return a
}

// ClientIP returns the client address, honoring X-Real-IP / X-Forwarded-For
// only from trusted proxies.
func (a *App) ClientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	remote, err := netip.ParseAddr(host)
	if err != nil {
		return host
	}
	remote = remote.Unmap()
	for _, p := range a.trusted {
		if p.Contains(remote) {
			if v := strings.TrimSpace(r.Header.Get("X-Real-IP")); v != "" {
				return v
			}
			if v := r.Header.Get("X-Forwarded-For"); v != "" {
				parts := strings.Split(v, ",")
				return strings.TrimSpace(parts[len(parts)-1])
			}
		}
	}
	return remote.String()
}

// SessionCookie is the name of the login cookie.
const SessionCookie = "ersp_session"

// SessionTTL is the sliding session lifetime.
const SessionTTL = 12 * time.Hour

type ctxKey int

const (
	keyUser ctxKey = iota
	keySession
)

// WithSession stores the user and session in the context.
func WithSession(ctx context.Context, s *store.Session, u *store.User) context.Context {
	return context.WithValue(context.WithValue(ctx, keySession, s), keyUser, u)
}

// User returns the logged-in user or nil.
func User(ctx context.Context) *store.User {
	u, _ := ctx.Value(keyUser).(*store.User)
	return u
}

// Session returns the session or nil.
func Session(ctx context.Context) *store.Session {
	s, _ := ctx.Value(keySession).(*store.Session)
	return s
}

// SessionCookieAttrs returns a session cookie with the security attributes
// set. SameSite=Strict keeps the cookie out of every request that a sandboxed
// (opaque origin) EDM document makes, see docs/security.md.
func (a *App) SessionCookieAttrs(value string, maxAge int) *http.Cookie {
	return &http.Cookie{Name: SessionCookie, Value: value, Path: "/", HttpOnly: true,
		SameSite: http.SameSiteStrictMode, Secure: a.Cfg.SecureCookies(), MaxAge: maxAge}
}

// LoadSession resolves the session cookie and extends the session when it
// is past half of its lifetime.
func (a *App) LoadSession(r *http.Request) (*store.Session, *store.User) {
	c, err := r.Cookie(SessionCookie)
	if err != nil || c.Value == "" {
		return nil, nil
	}
	s, u, err := a.DB.GetSession(r.Context(), auth.HashToken(c.Value))
	if err != nil {
		return nil, nil
	}
	if time.Until(s.ExpiresAt) < SessionTTL/2 {
		s.ExpiresAt = store.Now().Add(SessionTTL)
		if err := a.DB.ExtendSession(r.Context(), s.ID, s.ExpiresAt); err != nil {
			a.Log.Warn("extend session", "err", err)
		}
	}
	return s, u
}

// SessionMiddleware attaches the session (if any) to the request context.
func (a *App) SessionMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s, u := a.LoadSession(r); u != nil {
			r = r.WithContext(WithSession(r.Context(), s, u))
		}
		next.ServeHTTP(w, r)
	})
}

// SameOrigin reports whether a request may change state: a present Origin
// header must equal the public URL's origin, and "null" (sandboxed documents,
// privacy-sensitive redirects) is never accepted. Fetch Metadata, if sent,
// must not say cross-site.
func (a *App) SameOrigin(r *http.Request) bool {
	if o := r.Header.Get("Origin"); o != "" && o != a.Cfg.Origin() {
		return false
	}
	switch r.Header.Get("Sec-Fetch-Site") {
	case "", "same-origin", "none":
		return true
	}
	return false
}

// Audit writes an audit log entry for the current user.
func (a *App) Audit(r *http.Request, action, target, details string) {
	e := store.AuditEntry{Action: action, Target: target, Details: details, IP: a.ClientIP(r), Actor: "system"}
	if u := User(r.Context()); u != nil {
		e.UserID, e.Actor = u.ID, u.Username
	}
	if err := a.DB.Audit(context.WithoutCancel(r.Context()), e); err != nil {
		a.Log.Error("audit log", "err", err, "action", action)
	}
}

// SetFrameSources replaces the origins that portal pages may embed in
// addition to 'self' (link devices that allow framing).
func (a *App) SetFrameSources(origins []string) {
	a.frameSrc.Store(strings.Join(origins, " "))
}

// SecurityHeaders sets CSP and friends for portal pages. No inline scripts or
// styles anywhere. Proxied EDM content gets its own headers (internal/edmproxy).
func (a *App) SecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		frame := "'self'"
		if extra, _ := a.frameSrc.Load().(string); extra != "" {
			frame += " " + extra
		}
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; "+
			"connect-src 'self'; font-src 'self'; frame-src "+frame+"; frame-ancestors 'none'; base-uri 'none'; "+
			"form-action 'self'; object-src 'none'")
		h.Set("X-Frame-Options", "DENY")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "same-origin")
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		next.ServeHTTP(w, r)
	})
}

// RequestLog logs requests at debug level and server errors at warn. Proxy
// paths are logged without the token.
func (a *App) RequestLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: 200}
		next.ServeHTTP(sw, r)
		level := slog.LevelDebug
		if sw.status >= 500 {
			level = slog.LevelWarn
		}
		a.Log.Log(r.Context(), level, "http", "method", r.Method, "path", RedactPath(r.URL.Path), "status", sw.status,
			"duration", time.Since(start).Round(time.Millisecond), "ip", a.ClientIP(r))
	})
}

// RedactPath hides the proxy token in /d/{token}/… paths.
func RedactPath(p string) string {
	if !strings.HasPrefix(p, "/d/") {
		return p
	}
	rest := p[3:]
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		return "/d/…" + rest[i:]
	}
	return "/d/…"
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

// Flush passes through to the underlying writer.
func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
