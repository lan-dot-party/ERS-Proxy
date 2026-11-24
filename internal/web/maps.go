package web

// Network maps (roadmap phase 10): devices placed on a background image, the
// links between them (LLDP, learned MAC addresses, drawn by hand) with their
// current rates. Everyone logged in can view maps; only admins change them.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	_ "image/gif"  // background images: GIF
	_ "image/jpeg" // background images: JPEG
	_ "image/png"  // background images: PNG
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/lan-dot-party/ers-proxy/internal/app"
	"github.com/lan-dot-party/ers-proxy/internal/monitor"
	"github.com/lan-dot-party/ers-proxy/internal/store"
	"github.com/lan-dot-party/ers-proxy/internal/topology"
)

// maxMapBackground is the largest background image accepted.
const maxMapBackground = 8 << 20

// maxMapPixels bounds width × height of a background image.
const maxMapPixels = 50_000_000

// Default size of a map without background image (its coordinate space).
const defaultMapWidth, defaultMapHeight = 1600, 900

type mapsData struct {
	Maps    []*store.Map
	Current *store.Map
	Admin   bool
}

func (s *Server) loadMap(w http.ResponseWriter, r *http.Request) (*store.Map, bool) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		s.errorPage(w, r, http.StatusNotFound, "error.not_found")
		return nil, false
	}
	m, err := s.DB.MapByID(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		s.errorPage(w, r, http.StatusNotFound, "error.not_found")
		return nil, false
	}
	if err != nil {
		s.fail(w, r, err)
		return nil, false
	}
	return m, true
}

// mapsPage shows the first map (or none).
func (s *Server) mapsPage(w http.ResponseWriter, r *http.Request) {
	maps, err := s.DB.ListMaps(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if len(maps) > 0 {
		redirect(w, r, fmt.Sprintf("/maps/%d", maps[0].ID))
		return
	}
	s.renderMap(w, r, maps, nil)
}

func (s *Server) mapPage(w http.ResponseWriter, r *http.Request) {
	m, ok := s.loadMap(w, r)
	if !ok {
		return
	}
	maps, err := s.DB.ListMaps(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.renderMap(w, r, maps, m)
}

func (s *Server) renderMap(w http.ResponseWriter, r *http.Request, maps []*store.Map, m *store.Map) {
	frameable(w)
	p := s.page(r, "maps.title", "maps", mapsData{Maps: maps, Current: m, Admin: app.User(r.Context()).Can(store.RoleAdmin)})
	if m != nil {
		p.Title = m.Name + " · " + p.T("maps.title")
	}
	p.Wide = true
	s.render(w, r, "maps", p)
}

// mapBackground serves a map's background image.
func (s *Server) mapBackground(w http.ResponseWriter, r *http.Request) {
	m, ok := s.loadMap(w, r)
	if !ok {
		return
	}
	data, mediaType, hash, err := s.DB.MapBackground(r.Context(), m.ID)
	if errors.Is(err, store.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	h := w.Header()
	h.Set("Content-Type", mediaType)
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Security-Policy", "default-src 'none'; sandbox")
	h.Set("Cache-Control", "private, max-age=3600")
	h.Set("ETag", `"`+hash+`"`)
	http.ServeContent(w, r, "", m.Updated, bytes.NewReader(data))
}

// --- state for the browser ---

type mapEndJSON struct {
	Device  int64   `json:"device"`
	IfIndex int     `json:"ifindex"`
	Port    string  `json:"port"`
	Speed   uint64  `json:"speed"`
	Up      bool    `json:"up"`
	InBps   float64 `json:"-"`
	OutBps  float64 `json:"-"`
	Known   bool    `json:"-"`
}

type mapLinkJSON struct {
	Source string     `json:"source"` // lldp, fdb, manual
	ID     int64      `json:"id,omitempty"`
	A      mapEndJSON `json:"a"`
	B      mapEndJSON `json:"b"`
	AB     float64    `json:"ab_bps"` // A → B
	BA     float64    `json:"ba_bps"` // B → A
	Util   float64    `json:"util"`   // percent of the link speed, higher direction
	Rates  bool       `json:"rates"`  // rates known
}

type mapPortJSON struct {
	IfIndex int    `json:"ifindex"`
	Name    string `json:"name"`
}

type mapNodeJSON struct {
	ID        int64         `json:"id"`
	Name      string        `json:"name"`
	X         float64       `json:"x"`
	Y         float64       `json:"y"`
	Status    string        `json:"status"` // up, down, unknown, unmonitored
	Monitored bool          `json:"monitored"`
	Error     string        `json:"error,omitempty"`
	Ports     []mapPortJSON `json:"ports,omitempty"` // admins only, for drawing links
}

type mapStateJSON struct {
	Map struct {
		ID      int64  `json:"id"`
		Name    string `json:"name"`
		Refresh int    `json:"refresh"`
		Width   int    `json:"width"`
		Height  int    `json:"height"`
		BG      string `json:"bg,omitempty"`
	} `json:"map"`
	Nodes   []mapNodeJSON `json:"nodes"`
	Links   []mapLinkJSON `json:"links"`
	Hidden  []mapLinkJSON `json:"hidden,omitempty"`  // admins only
	Devices []mapPortJSON `json:"devices,omitempty"` // admins only: devices to add (ifindex = device ID)
	Time    time.Time     `json:"time"`
}

// mapStateJSON returns nodes, links and rates of a map.
func (s *Server) mapState(w http.ResponseWriter, r *http.Request) {
	m, ok := s.loadMap(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	admin := app.User(ctx).Can(store.RoleAdmin)
	nodes, err := s.DB.MapNodes(ctx, m.ID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	stored, err := s.DB.MapLinks(ctx, m.ID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	devs, err := s.DB.ListDevices(ctx)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	byID := map[int64]*store.Device{}
	for _, d := range devs {
		byID[d.ID] = d
	}
	st := mapStateJSON{Nodes: []mapNodeJSON{}, Links: []mapLinkJSON{}}
	st.Map.ID, st.Map.Name, st.Map.Refresh = m.ID, m.Name, m.Refresh
	st.Map.Width, st.Map.Height = defaultMapWidth, defaultMapHeight
	if m.HasBackground() {
		st.Map.Width, st.Map.Height = m.BGWidth, m.BGHeight
		st.Map.BG = fmt.Sprintf("/maps/%d/background?v=%s", m.ID, m.BGHash)
	}
	st.Time = time.Now().UTC()

	// Snapshots of all monitored devices: the topology needs every device
	// to recognise neighbours, even those not on this map.
	snaps := map[int64]*monitor.Snapshot{}
	var topo []topology.Device
	if s.Monitor != nil {
		for _, ps := range s.Monitor.All() {
			if ps.Snap == nil {
				continue
			}
			snaps[ps.DeviceID] = ps.Snap
			topo = append(topo, topology.Device{ID: ps.DeviceID, Name: ps.Name, Snap: ps.Snap})
		}
	}
	onMap := map[int64]bool{}
	for _, n := range nodes {
		d := byID[n.DeviceID]
		if d == nil {
			continue
		}
		onMap[d.ID] = true
		nj := mapNodeJSON{ID: d.ID, Name: d.Name, X: n.X, Y: n.Y, Status: "unmonitored"}
		if s.Monitor != nil {
			if ps, ok := s.Monitor.Status(d.ID); ok {
				nj.Monitored = true
				switch {
				case ps.Err != "" && (ps.Snap == nil || ps.ErrAt.After(ps.LastPoll)):
					nj.Status, nj.Error = "down", ps.Err
				case ps.Snap != nil:
					nj.Status = "up"
				default:
					nj.Status = "unknown"
				}
			}
		}
		if !nj.Monitored && s.Health != nil {
			if state, _ := s.Health.Status(d.ID); state == "up" || state == "down" {
				nj.Status = state
			}
		}
		if admin {
			nj.Ports = mapPorts(snaps[d.ID])
		}
		st.Nodes = append(st.Nodes, nj)
	}
	end := func(dev int64, ifIndex int, port string) mapEndJSON {
		e := mapEndJSON{Device: dev, IfIndex: ifIndex, Port: port}
		if snap := snaps[dev]; snap != nil && ifIndex != 0 {
			if p := snap.Port(ifIndex); p != nil {
				e.Known, e.Speed, e.Up, e.InBps, e.OutBps = true, p.Speed, p.Up(), p.InBps, p.OutBps
				if e.Port == "" {
					e.Port = p.Name
				}
			}
		}
		return e
	}
	hidden := func(a, b mapEndJSON) bool {
		for i := range stored {
			if stored[i].Kind == store.MapLinkHidden && stored[i].Same(a.Device, a.IfIndex, b.Device, b.IfIndex) {
				return true
			}
		}
		return false
	}
	for _, l := range topology.Build(topo) {
		if !onMap[l.A.DeviceID] || !onMap[l.B.DeviceID] {
			continue
		}
		lj := mapLinkJSON{Source: l.Source, A: end(l.A.DeviceID, l.A.IfIndex, l.A.Port), B: end(l.B.DeviceID, l.B.IfIndex, l.B.Port)}
		if hidden(lj.A, lj.B) {
			continue
		}
		st.Links = append(st.Links, rates(lj))
	}
	for _, sl := range stored {
		if !onMap[sl.ADevice] || !onMap[sl.BDevice] {
			continue
		}
		lj := mapLinkJSON{Source: sl.Kind, ID: sl.ID, A: end(sl.ADevice, sl.AIf, ""), B: end(sl.BDevice, sl.BIf, "")}
		switch sl.Kind {
		case store.MapLinkManual:
			lj.Source = "manual"
			st.Links = append(st.Links, rates(lj))
		case store.MapLinkHidden:
			if admin {
				st.Hidden = append(st.Hidden, lj)
			}
		}
	}
	if admin {
		for _, d := range devs {
			if !onMap[d.ID] {
				st.Devices = append(st.Devices, mapPortJSON{IfIndex: int(d.ID), Name: d.Name})
			}
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(st)
}

// rates fills the rates of a link from the port at A, else from the port at B
// (reversed).
func rates(l mapLinkJSON) mapLinkJSON {
	speed := l.A.Speed
	switch {
	case l.A.Known:
		l.AB, l.BA, l.Rates = l.A.OutBps, l.A.InBps, true
	case l.B.Known:
		l.AB, l.BA, l.Rates, speed = l.B.InBps, l.B.OutBps, true, l.B.Speed
	}
	if speed == 0 {
		speed = max(l.A.Speed, l.B.Speed)
	}
	if l.Rates && speed > 0 {
		l.Util = 100 * max(l.AB, l.BA) / float64(speed)
	}
	return l
}

// mapPorts lists the ports and LAGs of a snapshot for the link editor.
func mapPorts(snap *monitor.Snapshot) []mapPortJSON {
	if snap == nil {
		return nil
	}
	var out []mapPortJSON
	for _, p := range snap.Ports {
		if p.Physical || p.IsLAG {
			out = append(out, mapPortJSON{IfIndex: p.IfIndex, Name: p.Name})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].IfIndex < out[j].IfIndex })
	return out
}

// --- admin actions ---

func (s *Server) mapCreate(w http.ResponseWriter, r *http.Request) {
	m := &store.Map{Name: r.PostFormValue("name")}
	if err := s.DB.CreateMap(r.Context(), m); err != nil {
		s.mapFormError(w, r, err)
		return
	}
	s.Audit(r, "map.create", m.Name, "")
	redirect(w, r, fmt.Sprintf("/maps/%d?edit=1", m.ID))
}

func (s *Server) mapUpdate(w http.ResponseWriter, r *http.Request) {
	m, ok := s.loadMap(w, r)
	if !ok {
		return
	}
	m.Name = r.PostFormValue("name")
	m.Refresh, _ = strconv.Atoi(strings.TrimSpace(r.PostFormValue("refresh")))
	m.Sort, _ = strconv.Atoi(strings.TrimSpace(r.PostFormValue("sort")))
	if err := s.DB.UpdateMap(r.Context(), m); err != nil {
		s.mapFormError(w, r, err)
		return
	}
	s.Audit(r, "map.update", m.Name, fmt.Sprintf("refresh=%ds", m.Refresh))
	redirect(w, r, fmt.Sprintf("/maps/%d?edit=1", m.ID))
}

func (s *Server) mapDelete(w http.ResponseWriter, r *http.Request) {
	m, ok := s.loadMap(w, r)
	if !ok {
		return
	}
	if err := s.DB.DeleteMap(r.Context(), m.ID); err != nil {
		s.fail(w, r, err)
		return
	}
	s.Audit(r, "map.delete", m.Name, "")
	redirect(w, r, "/maps")
}

func (s *Server) mapFormError(w http.ResponseWriter, r *http.Request, err error) {
	key := "maps.err.invalid"
	if errors.Is(err, store.ErrExists) {
		key = "maps.err.exists"
	}
	s.errorPage(w, r, http.StatusUnprocessableEntity, key)
}

// mapBackgroundUpload stores or removes the background image. Only PNG,
// JPEG and GIF are accepted (decoded header, not the file name); SVG is
// refused because it can carry script.
func (s *Server) mapBackgroundUpload(w http.ResponseWriter, r *http.Request) {
	m, ok := s.loadMap(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	if r.PostFormValue("remove") == "1" {
		if err := s.DB.SetMapBackground(ctx, m.ID, nil, "", 0, 0); err != nil {
			s.fail(w, r, err)
			return
		}
		s.Audit(r, "map.background", m.Name, "removed")
		redirect(w, r, fmt.Sprintf("/maps/%d?edit=1", m.ID))
		return
	}
	if err := r.ParseMultipartForm(1 << 20); err != nil { // usually done by the CSRF check already
		s.errorPage(w, r, http.StatusRequestEntityTooLarge, "maps.err.image_size")
		return
	}
	f, _, err := r.FormFile("image")
	if err != nil {
		s.errorPage(w, r, http.StatusUnprocessableEntity, "maps.err.image")
		return
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxMapBackground+1))
	if err != nil || len(data) > maxMapBackground {
		s.errorPage(w, r, http.StatusRequestEntityTooLarge, "maps.err.image_size")
		return
	}
	cfg, format, err := image.DecodeConfig(bytes.NewReader(data))
	types := map[string]string{"png": "image/png", "jpeg": "image/jpeg", "gif": "image/gif"}
	mediaType := types[format]
	if err != nil || mediaType == "" || cfg.Width <= 0 || cfg.Height <= 0 || cfg.Width*cfg.Height > maxMapPixels {
		s.errorPage(w, r, http.StatusUnprocessableEntity, "maps.err.image")
		return
	}
	if err := s.DB.SetMapBackground(ctx, m.ID, data, mediaType, cfg.Width, cfg.Height); err != nil {
		s.fail(w, r, err)
		return
	}
	s.Audit(r, "map.background", m.Name, fmt.Sprintf("%s %dx%d %d bytes", format, cfg.Width, cfg.Height, len(data)))
	redirect(w, r, fmt.Sprintf("/maps/%d?edit=1", m.ID))
}

// mapNodes replaces the devices on a map (JSON from map.js).
func (s *Server) mapNodes(w http.ResponseWriter, r *http.Request) {
	m, ok := s.loadMap(w, r)
	if !ok {
		return
	}
	var in []struct {
		ID int64   `json:"id"`
		X  float64 `json:"x"`
		Y  float64 `json:"y"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&in); err != nil || len(in) > 2000 {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	nodes := make([]store.MapNode, 0, len(in))
	seen := map[int64]bool{}
	for _, n := range in {
		if seen[n.ID] {
			continue
		}
		seen[n.ID] = true
		nodes = append(nodes, store.MapNode{DeviceID: n.ID, X: n.X, Y: n.Y})
	}
	if err := s.DB.SetMapNodes(r.Context(), m.ID, nodes); err != nil {
		http.Error(w, "unknown device", http.StatusUnprocessableEntity)
		return
	}
	s.Audit(r, "map.nodes", m.Name, fmt.Sprintf("%d devices", len(nodes)))
	w.WriteHeader(http.StatusNoContent)
}

// mapLinkAdd stores a manual link or hides an automatic one.
func (s *Server) mapLinkAdd(w http.ResponseWriter, r *http.Request) {
	m, ok := s.loadMap(w, r)
	if !ok {
		return
	}
	num := func(k string) int64 { v, _ := strconv.ParseInt(r.PostFormValue(k), 10, 64); return v }
	// Interface indexes are 32-bit in SNMP; larger input parses to 0.
	ifNum := func(k string) int { v, _ := strconv.ParseInt(r.PostFormValue(k), 10, 32); return int(v) }
	l := &store.MapLink{Kind: r.PostFormValue("kind"), ADevice: num("a_device"), AIf: ifNum("a_if"),
		BDevice: num("b_device"), BIf: ifNum("b_if")}
	if err := s.DB.AddMapLink(r.Context(), m.ID, l); err != nil {
		http.Error(w, "invalid link", http.StatusUnprocessableEntity)
		return
	}
	s.Audit(r, "map.link."+l.Kind, m.Name, fmt.Sprintf("%d/%d–%d/%d", l.ADevice, l.AIf, l.BDevice, l.BIf))
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) mapLinkDelete(w http.ResponseWriter, r *http.Request) {
	m, ok := s.loadMap(w, r)
	if !ok {
		return
	}
	lid, _ := strconv.ParseInt(chi.URLParam(r, "lid"), 10, 64)
	if err := s.DB.DeleteMapLink(r.Context(), m.ID, lid); err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	s.Audit(r, "map.link.delete", m.Name, strconv.FormatInt(lid, 10))
	w.WriteHeader(http.StatusNoContent)
}
