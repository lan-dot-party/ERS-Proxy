package web

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/lan-dot-party/ers-proxy/internal/monitor"
	"github.com/lan-dot-party/ers-proxy/internal/monitor/poller"
	"github.com/lan-dot-party/ers-proxy/internal/store"
)

// HistoryView gives the web UI read access to stored port history.
type HistoryView interface {
	// Series returns the samples of one port over the last span, at a
	// resolution that fits the span.
	Series(ctx context.Context, deviceID int64, ifIndex int, span time.Duration) ([]store.Sample, error)
}

// MonitorView gives the web UI read access to the port dashboard data.
type MonitorView interface {
	All() []poller.Status
	Status(id int64) (poller.Status, bool)
}

// dashI18n lists the keys dashboard.js needs (passed as JSON in a data attribute).
var dashI18n = []string{
	"dash.ports_up", "dash.poe", "dash.uptime", "dash.last_poll", "dash.no_data", "dash.error", "dash.unit",
	"dash.other_ifs", "dash.close", "dash.open_edm", "dash.details", "dash.speed", "dash.duplex", "dash.state",
	"dash.admin_down", "dash.down", "dash.pvid", "dash.untagged", "dash.tagged", "dash.mode", "dash.lag",
	"dash.neighbor", "dash.traffic", "dash.errors", "dash.last_change", "dash.alias", "dash.all_vlans",
	"dash.uplink", "dash.notes", "dash.in", "dash.out", "dash.utilization", "dash.days",
}

func (s *Server) dashboard(w http.ResponseWriter, r *http.Request) {
	p := s.page(r, "dash.title", "dashboard", nil)
	tr := map[string]string{}
	for _, k := range dashI18n {
		tr[k] = p.T(k)
	}
	b, _ := json.Marshal(tr)
	p.Data = struct {
		I18N      string
		Monitored bool
	}{I18N: string(b), Monitored: s.Monitor != nil && len(s.Monitor.All()) > 0}
	s.render(w, r, "dashboard", p)
}

// JSON shape of /ui/monitor.json (consumed by static/dashboard.js).
type jsonDash struct {
	Devices []jsonDevice `json:"devices"`
	Time    int64        `json:"time"`
}

type jsonDevice struct {
	ID       int64          `json:"id"`
	Name     string         `json:"name"`
	Driver   string         `json:"driver"`
	Source   string         `json:"source"`
	LastPoll int64          `json:"last_poll"`
	Error    string         `json:"error,omitempty"`
	System   *jsonSystem    `json:"system,omitempty"`
	Units    []monitor.Unit `json:"units,omitempty"`
	PoE      *jsonPoE       `json:"poe,omitempty"`
	VLANs    []jsonVLAN     `json:"vlans,omitempty"`
	Ports    []jsonPort     `json:"ports,omitempty"`
	Notes    []string       `json:"notes,omitempty"`
	Proxied  bool           `json:"proxied"`
}

type jsonSystem struct {
	Name   string `json:"name"`
	Descr  string `json:"descr"`
	Uptime int64  `json:"uptime"`
}

type jsonPoE struct {
	Total int `json:"total"`
	Used  int `json:"used"`
}

type jsonVLAN struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
}

type jsonPort struct {
	IfIndex    int           `json:"ifindex"`
	Unit       int           `json:"unit"`
	Number     int           `json:"number"`
	Label      string        `json:"label"`
	Name       string        `json:"name"`
	Alias      string        `json:"alias,omitempty"`
	Physical   bool          `json:"physical"`
	IsLAG      bool          `json:"is_lag,omitempty"`
	Admin      string        `json:"admin"`
	Oper       string        `json:"oper"`
	Speed      uint64        `json:"speed"`
	Duplex     string        `json:"duplex,omitempty"`
	PVID       int           `json:"pvid,omitempty"`
	Untagged   []int         `json:"untagged,omitempty"`
	Tagged     []int         `json:"tagged,omitempty"`
	Mode       string        `json:"mode,omitempty"`
	PoE        *monitor.PoE  `json:"poe,omitempty"`
	LAG        int           `json:"lag,omitempty"`
	LAGName    string        `json:"lag_name,omitempty"`
	Neighbor   *jsonNeighbor `json:"neighbor,omitempty"`
	InBps      float64       `json:"in_bps"`
	OutBps     float64       `json:"out_bps"`
	Util       float64       `json:"util"`
	InErrors   uint64        `json:"in_errors"`
	OutErrors  uint64        `json:"out_errors"`
	LastChange int64         `json:"last_change"` // seconds of uptime at the last change
}

type jsonNeighbor struct {
	SysName  string `json:"sys_name"`
	PortID   string `json:"port_id"`
	PortDesc string `json:"port_desc,omitempty"`
	Bridge   bool   `json:"bridge"`
}

func toJSONDevice(st poller.Status, proxied bool) jsonDevice {
	d := jsonDevice{ID: st.DeviceID, Name: st.Name, Driver: st.Driver, Source: st.Source, Error: st.Err, Proxied: proxied}
	if !st.LastPoll.IsZero() {
		d.LastPoll = st.LastPoll.Unix()
	}
	snap := st.Snap
	if snap == nil {
		return d
	}
	d.System = &jsonSystem{Name: snap.System.Name, Descr: snap.System.Descr, Uptime: int64(snap.System.Uptime / time.Second)}
	d.Units, d.Notes = snap.Units, snap.Notes
	if snap.PoE != nil {
		d.PoE = &jsonPoE{Total: snap.PoE.TotalW, Used: snap.PoE.UsedW}
	}
	for _, v := range snap.VLANs {
		d.VLANs = append(d.VLANs, jsonVLAN{ID: v.ID, Name: v.Name})
	}
	for _, p := range snap.Ports {
		if !p.Physical && !p.IsLAG {
			continue // VLAN interfaces, loopbacks, CPU ports
		}
		jp := jsonPort{IfIndex: p.IfIndex, Unit: p.Unit, Number: p.Number, Label: p.Label(), Name: p.Name, Alias: p.Alias,
			Physical: p.Physical, IsLAG: p.IsLAG, Admin: p.Admin, Oper: p.Oper, Speed: p.Speed, Duplex: p.Duplex, PVID: p.PVID,
			Untagged: p.Untagged, Tagged: p.Tagged, Mode: p.Mode, PoE: p.PoE, LAG: p.LAG, LAGName: p.LAGName,
			InBps: p.InBps, OutBps: p.OutBps, Util: p.Utilization, InErrors: p.InErrors, OutErrors: p.OutErrors,
			LastChange: int64(p.LastChange / time.Second)}
		if n := p.Neighbor; n != nil {
			jp.Neighbor = &jsonNeighbor{SysName: n.SysName, PortID: n.PortID, PortDesc: n.PortDesc, Bridge: n.Bridge}
		}
		d.Ports = append(d.Ports, jp)
	}
	return d
}

// monitorJSON serves the latest snapshots of all monitored devices.
func (s *Server) monitorJSON(w http.ResponseWriter, r *http.Request) {
	out := jsonDash{Devices: []jsonDevice{}, Time: time.Now().Unix()}
	if s.Monitor != nil {
		proxied := map[int64]bool{}
		if devs, err := s.DB.ListDevices(r.Context()); err == nil {
			for _, d := range devs {
				proxied[d.ID] = d.Proxied()
			}
		}
		for _, st := range s.Monitor.All() {
			out.Devices = append(out.Devices, toJSONDevice(st, proxied[st.DeviceID]))
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(out)
}
