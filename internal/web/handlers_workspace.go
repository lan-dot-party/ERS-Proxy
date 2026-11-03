package web

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/lan-dot-party/ers-proxy/internal/app"
	"github.com/lan-dot-party/ers-proxy/internal/config"
	"github.com/lan-dot-party/ers-proxy/internal/devices"
	"github.com/lan-dot-party/ers-proxy/internal/store"
	"github.com/lan-dot-party/ers-proxy/internal/upstream"
)

// sidebarDevice is a device as the sidebar shows it.
type sidebarDevice struct {
	*store.Device
	// Mode is the effective open mode for the current user.
	Mode string
	// Href is what the link points to: /open/{id} for proxied devices, the
	// device URL for links.
	Href string
	// Warn marks devices that need attention (isolation off, plain HTTP).
	Warn string
}

type sidebarGroup struct {
	Name    string
	Devices []sidebarDevice
}

type workspaceData struct {
	Groups []sidebarGroup
	Count  int
}

// EffectiveOpenMode resolves device → user → global default. Link devices
// that cannot be embedded open in a new browser tab instead.
func (s *Server) EffectiveOpenMode(u *store.User, d *store.Device) string {
	mode := d.OpenMode
	if mode == "" || mode == store.OpenInherit {
		mode = ""
		if u != nil && config.ValidOpenMode(u.OpenMode) {
			mode = u.OpenMode
		}
		if mode == "" {
			mode = s.Cfg.UI.DefaultOpenMode
		}
	}
	if mode == config.OpenEmbedded && d.Kind == store.KindLink {
		if !s.linkEmbeddable(d) {
			mode = config.OpenTab
		}
	}
	return mode
}

// linkEmbeddable reports whether a link device can be shown in a frame: no
// mixed content, and the health check has not seen a framing ban.
func (s *Server) linkEmbeddable(d *store.Device) bool {
	if s.Cfg.SecureCookies() && strings.HasPrefix(d.URL, "http://") {
		return false // browsers block http frames in https pages
	}
	if s.Health != nil {
		if state, ok := s.Health.Status(d.ID); state != "unknown" && !ok {
			return false
		}
	}
	return true
}

func (s *Server) sidebar(r *http.Request) (*workspaceData, error) {
	devs, err := s.DB.ListDevices(r.Context())
	if err != nil {
		return nil, err
	}
	u := app.User(r.Context())
	data := &workspaceData{Count: len(devs)}
	idx := map[string]int{}
	for _, d := range devs {
		sd := sidebarDevice{Device: d, Mode: s.EffectiveOpenMode(u, d), Href: "/open/" + strconv.FormatInt(d.ID, 10)}
		if d.Kind == store.KindLink {
			sd.Href = d.URL
		}
		switch {
		case d.Proxied() && d.Isolation == store.IsolationOff:
			sd.Warn = "isolation_off"
		case d.Proxied() && strings.HasPrefix(d.URL, "http://"):
			sd.Warn = "plain_http"
		}
		i, ok := idx[d.GroupName]
		if !ok {
			i = len(data.Groups)
			idx[d.GroupName] = i
			data.Groups = append(data.Groups, sidebarGroup{Name: d.GroupName})
		}
		data.Groups[i].Devices = append(data.Groups[i].Devices, sd)
	}
	return data, nil
}

func (s *Server) workspace(w http.ResponseWriter, r *http.Request) {
	data, err := s.sidebar(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	p := s.page(r, "", "workspace", data)
	p.Wide = true
	s.render(w, r, "workspace", p)
}

// open sends the browser to a device: proxied devices through the EDM proxy
// (which creates a proxy session), links directly.
func (s *Server) open(w http.ResponseWriter, r *http.Request) {
	frameable(w)
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		s.errorPage(w, r, http.StatusNotFound, "error.not_found")
		return
	}
	// Only the portal itself (or a typed URL/bookmark) may open a device. A
	// sandboxed EDM document navigating here would be "cross-site".
	switch r.Header.Get("Sec-Fetch-Site") {
	case "", "same-origin", "none":
	default:
		s.errorPage(w, r, http.StatusForbidden, "error.origin")
		return
	}
	d, err := s.DB.DeviceByID(r.Context(), id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if d.Kind == store.KindLink {
		http.Redirect(w, r, d.URL, http.StatusSeeOther)
		return
	}
	if s.Opener == nil {
		s.errorPage(w, r, http.StatusServiceUnavailable, "error.proxy_unavailable")
		return
	}
	if strings.HasPrefix(d.URL, "https://") && d.TLSPin == "" {
		s.trust(w, r, d)
		return
	}
	s.Opener.Open(w, r, d)
}

type trustData struct {
	Device *store.Device
	Probe  *upstream.ProbeResult
}

// trust shows the certificate of an HTTPS device for confirmation (trust on
// first use). Only admins can confirm it.
func (s *Server) trust(w http.ResponseWriter, r *http.Request, d *store.Device) {
	probe := s.Probe
	if probe == nil {
		probe = func(ctx context.Context, d *store.Device) *upstream.ProbeResult {
			host, port := devices.HostPort(d.URL)
			return upstream.Probe(ctx, net.JoinHostPort(host, port), host, s.Cfg.Health.Timeout)
		}
	}
	res := probe(r.Context(), d)
	p := s.page(r, "trust.title", "", trustData{Device: d, Probe: res})
	s.render(w, r, "trust", p)
}

// statusJSON returns the health state of all devices for the sidebar dots.
func (s *Server) statusJSON(w http.ResponseWriter, r *http.Request) {
	devs, err := s.DB.ListDevices(r.Context())
	if err != nil {
		http.Error(w, `{"error":"internal"}`, http.StatusInternalServerError)
		return
	}
	out := map[string]string{}
	for _, d := range devs {
		state := "unknown"
		if !d.HealthEnabled || !s.Cfg.Health.Enabled {
			state = "off"
		} else if s.Health != nil {
			state, _ = s.Health.Status(d.ID)
		}
		out[strconv.FormatInt(d.ID, 10)] = state
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(out)
}

// UpdateFrameSources recomputes the CSP frame-src list from the link devices.
func (s *Server) UpdateFrameSources(ctx context.Context) {
	devs, err := s.DB.ListDevices(ctx)
	if err != nil {
		s.Log.Warn("list devices for frame-src", "err", err)
		return
	}
	seen := map[string]bool{}
	var origins []string
	for _, d := range devs {
		if d.Kind != store.KindLink {
			continue
		}
		if o := devices.Origin(d.URL); o != "" && !seen[o] {
			seen[o] = true
			origins = append(origins, o)
		}
	}
	s.SetFrameSources(origins)
}
