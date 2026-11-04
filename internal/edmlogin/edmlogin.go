// Package edmlogin performs the login to the EDM web interface of an ERS or
// VSP (VOSS) switch on the server side, exactly like the switch's login.html
// does it.
// It is shared by the EDM proxy (auto-login) and the EDM data source of the
// port dashboard. Experimental: derived from EDM's JavaScript, verified
// against the mock switch only.
package edmlogin

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
)

// Errors of the login.
var (
	ErrRejected       = errors.New("the switch rejected the stored login")
	ErrChangePassword = errors.New("the switch requires a password change; log in once in EDM yourself")
	ErrNoNonce        = errors.New("the switch did not set the auth cookie")
	ErrTooLong        = errors.New("EDM accepts at most 15 characters for user name and password")
)

// Cookies stores the switch's cookies between requests.
type Cookies interface {
	// Store takes the Set-Cookie headers of a response.
	Store(resp *http.Response)
	// Header returns the Cookie header for the next request ("" if none).
	Header() string
	// Value returns one cookie value ("" if not set).
	Value(name string) string
}

// Login logs in at base ("http(s)://switch") through rt. ERS switches
// (login form calls encode_ers2500):
//
//  1. GET /                      → Set-Cookie: auth=<nonce>
//  2. POST / (XHR, text/plain)   encoded=<len2><user>:<pass>&nonce=<nonce> → 200; 205 = change password.
//     A wrong password also gets 200, with authError.html ("Authentication Failed!") as body.
//  3. POST / (form)              encoded, nonce, URL=/ → session established
//
// VSP switches with VOSS (login form calls encode_vrf) skip the check and the
// length prefix: step 3 only, with encoded=<user>:<pass>. A wrong password
// gets authError.html there. VOSS 8 sets no auth cookie in step 1; the nonce
// is then empty.
//
// Afterwards the "auth" cookie identifies the session (EDM sends it as userId).
func Login(ctx context.Context, rt http.RoundTripper, base string, jar Cookies, user, password string) error {
	hc := &http.Client{Transport: rt, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	root := strings.TrimRight(base, "/") + "/"
	do := func(method, ctype, body string) (*http.Response, []byte, error) {
		req, err := http.NewRequestWithContext(ctx, method, root, strings.NewReader(body))
		if err != nil {
			return nil, nil, err
		}
		if ctype != "" {
			req.Header.Set("Content-Type", ctype)
		}
		req.Header.Set("User-Agent", "Mozilla/5.0 (ERS-Proxy)")
		if ck := jar.Header(); ck != "" {
			req.Header.Set("Cookie", ck)
		}
		resp, err := hc.Do(req)
		if err != nil {
			return nil, nil, err
		}
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		resp.Body.Close()
		jar.Store(resp)
		return resp, data, nil
	}

	_, page, err := do(http.MethodGet, "", "")
	if err != nil {
		return fmt.Errorf("load login page: %w", err)
	}
	nonce := jar.Value("auth")
	voss := Style(page) == StyleVOSS
	// VOSS 8 sets no auth cookie before the login; its form then sends an
	// empty nonce, and the switch accepts that.
	if nonce == "" && !voss {
		return ErrNoNonce
	}
	if voss {
		form := url.Values{"encoded": {user + ":" + password}, "nonce": {nonce}, "URL": {"/"}}
		resp, body, err := do(http.MethodPost, "application/x-www-form-urlencoded", form.Encode())
		if err != nil {
			return fmt.Errorf("login submit: %w", err)
		}
		switch {
		case resp.StatusCode == 205:
			return ErrChangePassword
		case resp.StatusCode != http.StatusOK || authFailed(body):
			return fmt.Errorf("%w (HTTP %d)", ErrRejected, resp.StatusCode)
		}
		return nil
	}
	if len(user) > 15 || len(password) > 15 {
		return ErrTooLong
	}
	encoded := fmt.Sprintf("%02d%s:%s", len(user), user, password)
	// The page sends this string unencoded with XMLHttpRequest.send().
	resp, body, err := do(http.MethodPost, "text/plain;charset=UTF-8", "encoded="+encoded+"&nonce="+nonce)
	if err != nil {
		return fmt.Errorf("login check: %w", err)
	}
	switch {
	case resp.StatusCode == 205:
		return ErrChangePassword
	case resp.StatusCode != http.StatusOK:
		return fmt.Errorf("%w (HTTP %d)", ErrRejected, resp.StatusCode)
	case authFailed(body):
		return fmt.Errorf("%w (authentication failed)", ErrRejected)
	}
	form := url.Values{"encoded": {encoded}, "nonce": {nonce}, "URL": {"/"}}
	resp, body, err = do(http.MethodPost, "application/x-www-form-urlencoded", form.Encode())
	if err != nil {
		return fmt.Errorf("login submit: %w", err)
	}
	if resp.StatusCode >= 400 || authFailed(body) {
		return fmt.Errorf("%w (HTTP %d)", ErrRejected, resp.StatusCode)
	}
	return nil
}

// Login page styles, see Style.
const (
	StyleERS  = "ers"
	StyleVOSS = "voss"
)

// loginHandler finds the function the login form calls on submit.
var loginHandler = regexp.MustCompile(`(?i)<form[^>]*\sonsubmit\s*=\s*["'](?:return\s+)?(\w+)\s*\(`)

// Style tells from the login page (plain or gzip-compressed) how a switch
// expects the login: StyleVOSS when the form calls encode_vrf (VSP with
// VOSS), StyleERS otherwise.
func Style(page []byte) string {
	if m := loginHandler.FindSubmatch(gunzip(page)); m != nil && strings.EqualFold(string(m[1]), "encode_vrf") {
		return StyleVOSS
	}
	return StyleERS
}

// gunzip returns body uncompressed if it is gzip data, else unchanged.
// The switch sends its pages gzip-compressed whatever the request asked for.
func gunzip(body []byte) []byte {
	if len(body) > 2 && body[0] == 0x1f && body[1] == 0x8b {
		if zr, err := gzip.NewReader(bytes.NewReader(body)); err == nil {
			if plain, err := io.ReadAll(io.LimitReader(zr, 4<<20)); err == nil {
				return plain
			}
		}
	}
	return body
}

// authFailed reports whether a login answer is the switch's authError.html.
func authFailed(body []byte) bool {
	return bytes.Contains(gunzip(body), []byte("Authentication Failed"))
}

// Jar is a simple Cookies implementation.
type Jar struct {
	cookies map[string]string
	order   []string
}

// NewJar returns an empty jar.
func NewJar() *Jar { return &Jar{cookies: map[string]string{}} }

// Store implements Cookies.
func (j *Jar) Store(resp *http.Response) {
	for _, c := range resp.Cookies() {
		if c.MaxAge < 0 {
			delete(j.cookies, c.Name)
			continue
		}
		if _, ok := j.cookies[c.Name]; !ok {
			j.order = append(j.order, c.Name)
		}
		j.cookies[c.Name] = c.Value
	}
}

// Header implements Cookies.
func (j *Jar) Header() string {
	var parts []string
	for _, n := range j.order {
		if v, ok := j.cookies[n]; ok {
			parts = append(parts, n+"="+v)
		}
	}
	return strings.Join(parts, "; ")
}

// Value implements Cookies.
func (j *Jar) Value(name string) string { return j.cookies[name] }
