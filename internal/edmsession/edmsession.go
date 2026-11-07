// Package edmsession is the logged-in EDM session at one switch: login on
// demand, the userId taken from the auth cookie, one re-login when the session
// has expired, and logout. It is only transport; what is sent over it (read or
// write commands) is up to the callers.
//
// There must be exactly one Session per device and every user of the device's
// EDM (the monitor's reader, a configuration writer) shares it: VSP switches
// allow only about five web sessions. Requests are serialized because the
// switch's web server is slow and handles few connections.
package edmsession

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/lan-dot-party/ers-proxy/internal/edmlogin"
	"github.com/lan-dot-party/ers-proxy/internal/upstream"
)

// Session is a logged-in EDM session at one switch. Requests are serialized:
// the switch's web server is slow and handles few connections.
type Session struct {
	rt        http.RoundTripper
	base      string
	user, pwd string

	mu     sync.Mutex
	jar    *edmlogin.Jar
	logged bool
	// refused is set when the switch rejected the login. The session does not
	// try again: ERS switches lock the account after a few failed logins, and
	// a poll every minute would keep it locked for people, too. Storing new
	// credentials creates a new session.
	refused error
}

// New prepares a session; it logs in on the first request.
func New(rt http.RoundTripper, base, user, password string) *Session {
	return &Session{rt: rt, base: strings.TrimRight(base, "/"), user: user, pwd: password, jar: edmlogin.NewJar()}
}

// ErrSession is returned when the switch rejects the session.
var ErrSession = errors.New("EDM session not accepted")

// Post sends one command, logging in first if needed and once more if the
// session has expired.
func (s *Session) Post(ctx context.Context, path string, form url.Values) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for attempt := 0; attempt < 2; attempt++ {
		if !s.logged {
			if s.refused != nil {
				return "", fmt.Errorf("%w (not retried until the stored login changes)", s.refused)
			}
			s.jar = edmlogin.NewJar()
			if err := edmlogin.Login(ctx, s.rt, s.base, s.jar, s.user, s.pwd); err != nil {
				if errors.Is(err, edmlogin.ErrRejected) || errors.Is(err, edmlogin.ErrChangePassword) {
					s.refused = err
				}
				return "", err
			}
			s.logged = true
		}
		f := url.Values{}
		for k, v := range form {
			f[k] = v
		}
		f.Set("userId", s.jar.Value("auth"))
		rctx := ctx
		if upstream.EDMReadCommand(f.Get("command")) {
			rctx = upstream.WithRetry(ctx)
		}
		req, err := http.NewRequestWithContext(rctx, http.MethodPost, s.base+path, strings.NewReader(f.Encode()))
		if err != nil {
			return "", err
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=UTF-8")
		req.Header.Set("X-Requested-With", "XMLHttpRequest")
		req.Header.Set("Cookie", s.jar.Header())
		resp, err := (&http.Client{Transport: s.rt, Timeout: time.Minute}).Do(req)
		if err != nil {
			return "", err
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		resp.Body.Close()
		if err != nil {
			return "", err
		}
		s.jar.Store(resp)
		text := string(body)
		if resp.StatusCode == http.StatusUnauthorized || strings.Contains(text, "Not Authorized") ||
			strings.Contains(text, "submitForm") { // the login page instead of data
			s.logged = false
			continue
		}
		if resp.StatusCode != http.StatusOK {
			return "", fmt.Errorf("%s: HTTP %d", path, resp.StatusCode)
		}
		return text, nil
	}
	return "", ErrSession
}

// Logout ends the switch's session (GET /logout.html, like EDM's "Log out"
// link). VSP switches allow only a few web sessions at a time, so a session
// that is replaced should not wait for the switch's idle timeout.
func (s *Session) Logout(ctx context.Context) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.logged {
		return
	}
	s.logged = false
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.base+"/logout.html", nil)
	if err != nil {
		return
	}
	req.Header.Set("Cookie", s.jar.Header())
	resp, err := (&http.Client{Transport: s.rt, Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
}
