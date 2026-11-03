package edmproxy

import (
	"context"
	"net/http"

	"github.com/lan-dot-party/ers-proxy/internal/devices"
	"github.com/lan-dot-party/ers-proxy/internal/edmlogin"
	"github.com/lan-dot-party/ers-proxy/internal/upstream"
)

// Errors of the server-side EDM login (see internal/edmlogin).
var (
	ErrLoginRejected  = edmlogin.ErrRejected
	ErrChangePassword = edmlogin.ErrChangePassword
	ErrNoNonce        = edmlogin.ErrNoNonce
)

// jarCookies adapts the proxy session's jar to edmlogin.Cookies.
type jarCookies struct{ j *jar }

func (c jarCookies) Store(resp *http.Response) { c.j.store(resp) }
func (c jarCookies) Header() string            { return c.j.header() }
func (c jarCookies) Value(name string) string  { return c.j.value(name) }

// autoLogin performs the EDM login with the stored credentials into the
// proxy session's cookie jar. Experimental, see internal/edmlogin.
func (p *Proxy) autoLogin(ctx context.Context, ps *proxySession, c *upstream.Client, cred devices.Credentials) error {
	return edmlogin.Login(ctx, c.Transport, c.Base().String(), jarCookies{ps.jar}, cred.Username, cred.Password)
}
