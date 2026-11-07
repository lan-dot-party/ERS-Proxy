// Package web serves the server-rendered management UI (html/template, a small
// app.js, no inline scripts).
//
// Everything except /login, /health and /static is behind a login. Device names
// and anything a switch reports are untrusted; html/template escapes all output
// and the CSP forbids inline scripts. Proxied EDM content is not served here but
// by internal/edmproxy under /d/.
package web

import (
	"context"
	"crypto/subtle"
	"embed"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
	"net/url"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/lan-dot-party/ers-proxy/internal/app"
	"github.com/lan-dot-party/ers-proxy/internal/auth"
	"github.com/lan-dot-party/ers-proxy/internal/config"
	"github.com/lan-dot-party/ers-proxy/internal/snmp"
	"github.com/lan-dot-party/ers-proxy/internal/store"
	"github.com/lan-dot-party/ers-proxy/internal/upstream"
	"github.com/lan-dot-party/ers-proxy/pkg/version"
)

//go:embed templates/*.html
var templateFS embed.FS

//go:embed static
var staticFS embed.FS

// Opener opens a proxied device for the current session (internal/edmproxy).
// It writes a redirect to the device's proxy path.
type Opener interface {
	Open(w http.ResponseWriter, r *http.Request, d *store.Device)
}

// StatusSource reports the health of devices (internal/health). It may be nil.
type StatusSource interface {
	// Status returns a state ("up", "down", "unknown", "off") and whether a
	// link device can be embedded in a frame.
	Status(id int64) (state string, embeddable bool)
}

// Server is the web UI.
type Server struct {
	*app.App
	pages  map[string]*template.Template
	login  *auth.LoginLimiter
	reauth *auth.LoginLimiter
	// Opener handles ers-edm devices; without it they cannot be opened.
	Opener Opener
	// Health provides status dots; optional.
	Health StatusSource
	// Monitor provides the port dashboard data; optional.
	Monitor MonitorView
	// History provides stored port traffic; optional.
	History HistoryView
	// Configure changes switch settings; nil unless configure.enabled is set
	// in config.yaml (see configure.go).
	Configure Configurator
	// Diagnostics renders extra rows on the diagnostics page; optional.
	Diagnostics func(d *store.Device) []DiagRow
	// OnDeviceChange is called after a device was changed or deleted (the
	// proxy then ends its sessions); optional.
	OnDeviceChange func(id int64)
	// Probe connects to an HTTPS device to show its certificate for
	// confirmation; optional (defaults to upstream.Probe).
	Probe func(ctx context.Context, d *store.Device) *upstream.ProbeResult
}

// DiagRow is one line of per-device diagnostics.
type DiagRow struct {
	Label string
	Value string
	Warn  bool
}

var pageFiles = []string{
	"login", "workspace", "account", "error", "trust", "dashboard",
	"devices", "device_form", "groups", "users", "audit", "diagnostics", "switch", "port", "config_preview",
}

// New parses all templates.
func New(a *app.App) (*Server, error) {
	s := &Server{App: a, pages: map[string]*template.Template{}, login: auth.NewLoginLimiter(), reauth: auth.NewLoginLimiter()}
	for _, name := range pageFiles {
		t, err := template.New(name).Funcs(funcs).ParseFS(templateFS,
			"templates/layout.html", "templates/"+name+".html")
		if err != nil {
			return nil, fmt.Errorf("parse template %s: %w", name, err)
		}
		s.pages[name] = t
	}
	return s, nil
}

var funcs = template.FuncMap{
	"themes":    func() []string { return store.Themes },
	"hasPrefix": strings.HasPrefix,
	"openModes": func() []string { return []string{config.OpenEmbedded, config.OpenTab, config.OpenWindow} },
	"tlsModes":  func() []string { return []string{store.TLSAuto, store.TLSModern, store.TLSLegacy} },
	"monitorSources": func() []string {
		return []string{store.MonitorNone, store.MonitorSNMP, store.MonitorEDM, store.MonitorSwOS, store.MonitorGWN}
	},
	"monitorDrivers": func() []string {
		return []string{store.DriverAuto, store.DriverGeneric, store.DriverERS, store.DriverCisco, store.DriverTPLink, store.DriverVOSS, store.DriverSwOS, store.DriverGWN}
	},
	"snmpVersions":   func() []string { return snmp.Versions },
	"securityLevels": func() []string { return snmp.SecurityLevels },
	"authProtocols":  func() []string { return snmp.AuthProtocols },
	"privProtocols":  func() []string { return snmp.PrivProtocols },
	"dict": func(kv ...any) map[string]any {
		m := make(map[string]any, len(kv)/2)
		for i := 0; i+1 < len(kv); i += 2 {
			k, _ := kv[i].(string)
			m[k] = kv[i+1]
		}
		return m
	},
}

// Routes returns the UI router.
func (s *Server) Routes() http.Handler {
	r := chi.NewRouter()
	static, _ := fs.Sub(staticFS, "static")
	r.Handle("/static/*", http.StripPrefix("/static/", cacheStatic(http.FileServerFS(static))))
	r.Get("/login", s.loginPage)
	r.Post("/login", s.loginSubmit)

	r.Group(func(r chi.Router) {
		r.Use(s.requireLogin, s.csrf)
		r.Post("/logout", s.logout)
		r.Get("/", s.workspace)
		r.Get("/open/{id}", s.open)
		r.Get("/ui/status", s.statusJSON)
		r.Get("/dashboard", s.dashboard)
		r.Get("/ui/monitor.json", s.monitorJSON)
		r.Get("/dashboard/{id}", s.switchPage)
		r.Get("/dashboard/{id}/port/{ifindex}", s.portPage)
		r.Get("/account", s.account)
		r.Post("/account/password", s.accountPassword)
		r.Post("/account/prefs", s.accountPrefs)
		r.Post("/account/theme", s.accountTheme)

		r.Group(func(r chi.Router) {
			r.Use(s.requireRole(store.RoleAdmin))
			r.Get("/admin/devices", s.devices)
			r.Get("/admin/devices/new", s.deviceNew)
			r.Post("/admin/devices", s.deviceCreate)
			r.Get("/admin/devices/export.{format}", s.deviceExport)
			r.Post("/admin/devices/import", s.deviceImport)
			r.Get("/admin/devices/{id}/edit", s.deviceEdit)
			r.Post("/admin/devices/{id}", s.deviceUpdate)
			r.Post("/admin/devices/{id}/delete", s.deviceDelete)
			r.Post("/admin/devices/{id}/credentials", s.deviceCredentials)
			r.Post("/admin/devices/{id}/snmp", s.deviceSNMP)
			r.Post("/admin/devices/{id}/tls", s.deviceTLS)
			r.Get("/admin/devices/{id}/trust", s.deviceTrust)
			r.Get("/admin/groups", s.groups)
			r.Post("/admin/groups", s.groupCreate)
			r.Post("/admin/groups/{id}", s.groupUpdate)
			r.Post("/admin/groups/{id}/delete", s.groupDelete)
			r.Get("/admin/users", s.users)
			r.Post("/admin/users", s.userCreate)
			r.Post("/admin/users/{id}", s.userUpdate)
			r.Post("/admin/users/{id}/password", s.userPassword)
			r.Post("/admin/users/{id}/delete", s.userDelete)
			r.Get("/admin/audit", s.audit)
			r.Get("/admin/diagnostics", s.diagnostics)
			if s.Configure != nil { // without it none of these exists (404)
				r.Post("/admin/devices/{id}/snmp-write", s.deviceSNMPWrite)
				r.Post("/dashboard/{id}/configure/preview", s.configPreview)
				r.Get("/dashboard/{id}/configure/preview/{pid}", s.configPreviewPage)
				r.Post("/dashboard/{id}/configure/apply", s.configApply)
				r.Post("/dashboard/{id}/configure/{change}/confirm", s.configConfirm)
				r.Post("/dashboard/{id}/configure/{change}/revert", s.configRevert)
			}
		})
	})
	r.NotFound(func(w http.ResponseWriter, r *http.Request) {
		s.errorPage(w, r, http.StatusNotFound, "error.not_found")
	})
	return r
}

// frameable lets the current response be shown inside the workspace (pages
// that /open returns instead of a redirect).
func frameable(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Content-Security-Policy", strings.Replace(h.Get("Content-Security-Policy"), "frame-ancestors 'none'", "frame-ancestors 'self'", 1))
	h.Set("X-Frame-Options", "SAMEORIGIN")
}

func (s *Server) deviceChanged(id int64) {
	if s.OnDeviceChange != nil {
		s.OnDeviceChange(id)
	}
}

func cacheStatic(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=3600")
		next.ServeHTTP(w, r)
	})
}

func (s *Server) requireLogin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if app.User(r.Context()) == nil {
			if r.Header.Get("Accept") == "application/json" {
				http.Error(w, `{"error":"login required"}`, http.StatusUnauthorized)
				return
			}
			// A link from another site arrives without the SameSite=Strict
			// cookie. Reload the start page once from our own origin so that the
			// cookie is sent; only for "/", never for /open (see docs/security.md).
			if r.URL.Path == "/" && r.Method == http.MethodGet && r.Header.Get("Sec-Fetch-Site") == "cross-site" &&
				r.URL.Query().Get("r") == "" {
				w.Header().Set("Content-Type", "text/html; charset=utf-8")
				w.Header().Set("Cache-Control", "no-store")
				fmt.Fprint(w, `<!doctype html><meta http-equiv="refresh" content="0;url=/?r=1"><title>ERS-Proxy</title>`)
				return
			}
			next := r.URL.RequestURI()
			http.Redirect(w, r, "/login?next="+url.QueryEscape(next), http.StatusSeeOther)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) requireRole(role string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !app.User(r.Context()).Can(role) {
				s.errorPage(w, r, http.StatusForbidden, "error.forbidden")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// csrf checks the session's token and the Origin on every state-changing request.
func (s *Server) csrf(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			if !s.SameOrigin(r) {
				s.Log.Warn("rejected cross-origin request", "path", r.URL.Path, "origin", r.Header.Get("Origin"),
					"expected", s.Cfg.Origin(), "hint", "check public_url if this is a legitimate request")
				s.errorPage(w, r, http.StatusForbidden, "error.origin")
				return
			}
			sess := app.Session(r.Context())
			token := r.Header.Get("X-CSRF-Token")
			if token == "" {
				token = r.PostFormValue("_csrf")
			}
			if sess == nil || subtle.ConstantTimeCompare([]byte(token), []byte(sess.CSRFToken)) != 1 {
				s.errorPage(w, r, http.StatusForbidden, "error.csrf")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// Page is passed to every template.
type Page struct {
	Lang    string
	User    *store.User
	CSRF    string
	Title   string
	Nav     string
	Flash   string
	Error   string
	Version string
	// Theme is the color theme: auto, light or dark.
	Theme string
	// Path is the current request URI, to return to after a theme change.
	Path string
	// Insecure is set when the portal is used without HTTPS.
	Insecure bool
	// Wide pages use the full width (workspace).
	Wide bool
	// Embedded pages are shown inside a workspace tab: no top bar or footer.
	Embedded bool
	Data     any
}

// NextTheme is the theme the toggle in the top bar switches to.
func (p *Page) NextTheme() string {
	switch p.Theme {
	case "auto":
		return "light"
	case "light":
		return "dark"
	}
	return "auto"
}

// T translates a key.
func (p *Page) T(key string, args ...any) string { return translate(p.Lang, key, args...) }

// Can reports whether the user has a role (for templates).
func (p *Page) Can(role string) bool { return p.User.Can(role) }

func (s *Server) page(r *http.Request, titleKey, nav string, data any) *Page {
	lang := language(r)
	p := &Page{Lang: lang, User: app.User(r.Context()), Nav: nav, Version: version.Version,
		Theme: theme(r), Path: r.URL.RequestURI(), Data: data, Insecure: !s.secureRequest(r),
		Embedded: r.Header.Get("Sec-Fetch-Dest") == "iframe"}
	if titleKey != "" {
		p.Title = translate(lang, titleKey)
	}
	if sess := app.Session(r.Context()); sess != nil {
		p.CSRF = sess.CSRFToken
	}
	if ok := r.URL.Query().Get("ok"); ok != "" && hasKey("flash."+ok) {
		p.Flash = p.T("flash." + ok)
	}
	if e := r.URL.Query().Get("err"); e != "" && hasKey("error."+e) {
		p.Error = p.T("error." + e)
	}
	return p
}

// secureRequest reports whether the browser talks HTTPS to us: directly, or
// through a trusted reverse proxy that says so.
func (s *Server) secureRequest(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	if s.Cfg.SecureCookies() {
		return true // public_url is https; a reverse proxy terminates TLS
	}
	return false
}

func (s *Server) render(w http.ResponseWriter, r *http.Request, name string, p *Page) {
	s.renderStatus(w, r, http.StatusOK, name, p)
}

func (s *Server) renderStatus(w http.ResponseWriter, r *http.Request, status int, name string, p *Page) {
	t, ok := s.pages[name]
	if !ok {
		http.Error(w, "unknown template "+name, http.StatusInternalServerError)
		return
	}
	var buf strings.Builder
	if err := t.ExecuteTemplate(&buf, "layout", p); err != nil {
		s.Log.Error("render", "template", name, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(buf.String())) // the client may have gone away
}

func (s *Server) errorPage(w http.ResponseWriter, r *http.Request, status int, key string) {
	p := s.page(r, "", "", nil)
	p.Error = p.T(key)
	s.renderStatus(w, r, status, "error", p)
}

func (s *Server) fail(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, store.ErrNotFound) {
		s.errorPage(w, r, http.StatusNotFound, "error.not_found")
		return
	}
	s.Log.Error("request failed", "path", app.RedactPath(r.URL.Path), "err", err)
	s.errorPage(w, r, http.StatusInternalServerError, "error.internal")
}

func redirect(w http.ResponseWriter, r *http.Request, to string) {
	http.Redirect(w, r, to, http.StatusSeeOther)
}

// themeCookie keeps the theme for the login page after logout.
const themeCookie = "ersp_theme"

// theme picks the user's theme, else the cookie's, else auto.
func theme(r *http.Request) string {
	if u := app.User(r.Context()); u != nil && store.ValidTheme(u.Theme) {
		return u.Theme
	}
	if c, err := r.Cookie(themeCookie); err == nil && store.ValidTheme(c.Value) {
		return c.Value
	}
	return "auto"
}

// language picks the user's language, else the browser's, else English.
func language(r *http.Request) string {
	if u := app.User(r.Context()); u != nil && (u.Language == "de" || u.Language == "en") {
		return u.Language
	}
	if strings.HasPrefix(strings.ToLower(r.Header.Get("Accept-Language")), "de") {
		return "de"
	}
	return "en"
}
