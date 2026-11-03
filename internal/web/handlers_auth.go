package web

import (
	"crypto/subtle"
	"net/http"
	"strings"

	"github.com/lan-dot-party/ers-proxy/internal/app"
	"github.com/lan-dot-party/ers-proxy/internal/auth"
	"github.com/lan-dot-party/ers-proxy/internal/config"
	"github.com/lan-dot-party/ers-proxy/internal/store"
)

const loginCSRFCookie = "ersp_login"

type loginData struct {
	CSRF     string
	Next     string
	Username string
}

func safeNext(next string) string {
	if !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") || strings.HasPrefix(next, "/\\") ||
		strings.HasPrefix(next, "/d/") {
		return "/"
	}
	return next
}

func (s *Server) loginPage(w http.ResponseWriter, r *http.Request) {
	if app.User(r.Context()) != nil {
		redirect(w, r, "/")
		return
	}
	s.renderLogin(w, r, http.StatusOK, "", safeNext(r.URL.Query().Get("next")), "")
}

func (s *Server) renderLogin(w http.ResponseWriter, r *http.Request, status int, errKey, next, username string) {
	// Double-submit token: there is no session yet to bind a CSRF token to.
	token := auth.RandomToken(16)
	http.SetCookie(w, &http.Cookie{Name: loginCSRFCookie, Value: token, Path: "/login", HttpOnly: true,
		SameSite: http.SameSiteStrictMode, Secure: s.Cfg.SecureCookies(), MaxAge: 900})
	p := s.page(r, "login.title", "", loginData{CSRF: token, Next: next, Username: username})
	if errKey != "" {
		p.Error = p.T(errKey)
	}
	s.renderStatus(w, r, status, "login", p)
}

func (s *Server) loginSubmit(w http.ResponseWriter, r *http.Request) {
	next := safeNext(r.PostFormValue("next"))
	username := strings.TrimSpace(r.PostFormValue("username"))
	password := r.PostFormValue("password")
	if !s.SameOrigin(r) {
		s.renderLogin(w, r, http.StatusForbidden, "error.origin", next, username)
		return
	}
	c, err := r.Cookie(loginCSRFCookie)
	if err != nil || subtle.ConstantTimeCompare([]byte(c.Value), []byte(r.PostFormValue("_login_csrf"))) != 1 {
		s.renderLogin(w, r, http.StatusForbidden, "error.csrf", next, username)
		return
	}
	ip := s.ClientIP(r)
	keys := []string{"ip:" + ip, "user:" + strings.ToLower(username)}
	if err := s.login.Check(keys...); err != nil {
		s.renderLogin(w, r, http.StatusTooManyRequests, "error.locked", next, username)
		return
	}
	u, err := s.DB.UserByName(r.Context(), username)
	if err != nil {
		auth.CheckDummy(password)
		s.loginFailed(w, r, keys, next, username)
		return
	}
	if !auth.CheckPassword(u.PasswordHash, password) || u.Disabled {
		s.loginFailed(w, r, keys, next, username)
		return
	}
	s.login.Success(keys...)

	// A new session ID on every login prevents session fixation.
	token := auth.RandomToken(32)
	sess := &store.Session{ID: auth.HashToken(token), UserID: u.ID, CSRFToken: auth.RandomToken(32),
		ExpiresAt: store.Now().Add(app.SessionTTL)}
	if err := s.DB.CreateSession(r.Context(), sess); err != nil {
		s.fail(w, r, err)
		return
	}
	if err := s.DB.TouchLogin(r.Context(), u.ID); err != nil {
		s.Log.Warn("record login time", "err", err)
	}
	if err := s.DB.PurgeSessions(r.Context()); err != nil {
		s.Log.Warn("purge expired sessions", "err", err)
	}
	// The server ends idle sessions after app.SessionTTL; the cookie may live longer.
	http.SetCookie(w, s.SessionCookieAttrs(token, 30*24*3600))
	http.SetCookie(w, &http.Cookie{Name: loginCSRFCookie, Value: "", Path: "/login", MaxAge: -1})
	r = r.WithContext(app.WithSession(r.Context(), sess, u))
	s.Audit(r, "login", u.Username, "")
	redirect(w, r, next)
}

func (s *Server) loginFailed(w http.ResponseWriter, r *http.Request, keys []string, next, username string) {
	s.login.Fail(keys...)
	if err := s.DB.Audit(r.Context(), store.AuditEntry{Actor: "anonymous", Action: "login.failed", Target: username,
		IP: s.ClientIP(r)}); err != nil {
		s.Log.Error("audit log", "err", err, "action", "login.failed")
	}
	s.renderLogin(w, r, http.StatusUnauthorized, "error.login", next, username)
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if sess := app.Session(r.Context()); sess != nil {
		if err := s.DB.DeleteSession(r.Context(), sess.ID); err != nil {
			s.Log.Warn("delete session", "err", err)
		}
	}
	http.SetCookie(w, s.SessionCookieAttrs("", -1))
	redirect(w, r, "/login")
}

// confirmPassword checks the current user's password for sensitive actions
// (creating users, storing switch passwords). Failures count towards the
// lockout like login failures.
func (s *Server) confirmPassword(r *http.Request) bool {
	u := app.User(r.Context())
	key := "reauth:" + strings.ToLower(u.Username)
	if s.reauth.Check(key) != nil {
		return false
	}
	if !auth.CheckPassword(u.PasswordHash, r.PostFormValue("confirm_password")) {
		s.reauth.Fail(key)
		return false
	}
	s.reauth.Success(key)
	return true
}

type accountData struct {
	OpenMode    string
	DefaultMode string
}

func (s *Server) account(w http.ResponseWriter, r *http.Request) {
	u := app.User(r.Context())
	s.render(w, r, "account", s.page(r, "account.title", "account",
		accountData{OpenMode: u.OpenMode, DefaultMode: s.Cfg.UI.DefaultOpenMode}))
}

func (s *Server) accountPassword(w http.ResponseWriter, r *http.Request) {
	u := app.User(r.Context())
	if !auth.CheckPassword(u.PasswordHash, r.PostFormValue("current")) {
		redirect(w, r, "/account?err=wrong_password")
		return
	}
	hash, err := auth.HashPassword(r.PostFormValue("password"))
	if err != nil {
		redirect(w, r, "/account?err=password_short")
		return
	}
	if err := s.DB.SetPassword(r.Context(), u.ID, hash); err != nil {
		s.fail(w, r, err)
		return
	}
	s.Audit(r, "user.password", u.Username, "self")
	// SetPassword ended all sessions, including this one.
	redirect(w, r, "/login")
}

func (s *Server) accountPrefs(w http.ResponseWriter, r *http.Request) {
	u := app.User(r.Context())
	lang := r.PostFormValue("language")
	if lang != "de" && lang != "en" {
		lang = "en"
	}
	mode := r.PostFormValue("open_mode")
	if mode != "" && !config.ValidOpenMode(mode) {
		s.errorPage(w, r, http.StatusBadRequest, "error.bad_request")
		return
	}
	if err := s.DB.SetUserPrefs(r.Context(), u.ID, lang, u.Theme, mode); err != nil {
		s.fail(w, r, err)
		return
	}
	redirect(w, r, "/account?ok=saved")
}

// accountTheme stores the color theme and returns to the page the user came from.
func (s *Server) accountTheme(w http.ResponseWriter, r *http.Request) {
	u := app.User(r.Context())
	t := r.PostFormValue("theme")
	if !store.ValidTheme(t) {
		s.errorPage(w, r, http.StatusBadRequest, "error.bad_request")
		return
	}
	if err := s.DB.SetUserPrefs(r.Context(), u.ID, u.Language, t, u.OpenMode); err != nil {
		s.fail(w, r, err)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: themeCookie, Value: t, Path: "/", HttpOnly: true,
		SameSite: http.SameSiteLaxMode, Secure: s.Cfg.SecureCookies(), MaxAge: 365 * 24 * 3600})
	redirect(w, r, safeNext(r.PostFormValue("back")))
}
