package web

import (
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/lan-dot-party/ers-proxy/internal/monitor"
	"github.com/lan-dot-party/ers-proxy/internal/monitor/poller"
	"github.com/lan-dot-party/ers-proxy/internal/store"
)

// Everything below is rendered by html/template: names, aliases and LLDP data
// from a switch are untrusted and escaped there. Only fixed class names and
// numbers are built here.

type tileView struct {
	Class string
	Num   int
	Title string
	Href  string
}

type unitView struct {
	Number int
	Label  bool
	Single bool
	Tiles  []tileView
}

type portRef struct {
	Label string
	Href  string
}

type portRow struct {
	Idx            int
	Port           *monitor.Port
	Href           string
	Label          string
	State          string
	StateClass     string
	Speed          string
	SpeedBps       uint64
	Untagged       string
	Tagged         string
	PoE            string
	LAG            string
	Neighbor       string
	In, Out        string
	InBps, OutBps  float64
	Util           string
	UtilVal        float64
	Errors         int64
	ErrorsText     string
	HasUtilization bool
}

type vlanRow struct {
	ID       int
	Name     string
	Untagged []portRef
	Tagged   []portRef
}

type eventRow struct {
	At       time.Time
	Port     string
	PortHref string
	Kind     string
	From, To string
}

type switchPage struct {
	ID       int64
	Name     string
	Driver   string
	Source   string
	Descr    string
	Uptime   string
	Units    []monitor.Unit
	PoE      string
	LastPoll time.Time
	Err      string
	EDM      bool
	HasSnap  bool
	Notes    []string
	Plates   []unitView
	Ports    []portRow
	VLANs    []vlanRow
	Events   []eventRow
}

func (s *Server) lookupStatus(w http.ResponseWriter, r *http.Request) (poller.Status, bool) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil || s.Monitor == nil {
		s.errorPage(w, r, http.StatusNotFound, "error.not_found")
		return poller.Status{}, false
	}
	st, ok := s.Monitor.Status(id)
	if !ok {
		s.errorPage(w, r, http.StatusNotFound, "error.not_found")
		return poller.Status{}, false
	}
	return st, true
}

func (s *Server) isEDM(r *http.Request, id int64) bool {
	d, err := s.DB.DeviceByID(r.Context(), id)
	return err == nil && d.EDM()
}

func (s *Server) switchPage(w http.ResponseWriter, r *http.Request) {
	st, ok := s.lookupStatus(w, r)
	if !ok {
		return
	}
	p := s.page(r, "", "dashboard", nil)
	p.Title = st.Name
	base := "/dashboard/" + strconv.FormatInt(st.DeviceID, 10)
	d := &switchPage{ID: st.DeviceID, Name: st.Name, Driver: firstNonEmpty(st.Driver, st.Source), Source: st.Source,
		Err: st.Err, LastPoll: st.LastPoll, EDM: s.isEDM(r, st.DeviceID)}
	if snap := st.Snap; snap != nil {
		d.HasSnap = true
		d.Descr = strings.Split(snap.System.Descr, "  ")[0]
		d.Uptime = durText(p, snap.System.Uptime)
		d.Units, d.Notes = snap.Units, snap.Notes
		if snap.PoE != nil {
			d.PoE = fmt.Sprintf("%d / %d W", snap.PoE.UsedW, snap.PoE.TotalW)
		}
		d.Plates = faceplates(p, snap, base)
		d.Ports = portRows(p, snap, base)
		d.VLANs = vlanRows(snap, base)
	}
	if evs, err := s.DB.ListEvents(r.Context(), st.DeviceID, 0, 50); err == nil {
		d.Events = eventRows(p, evs, base)
	}
	p.Data = d
	s.render(w, r, "switch", p)
}

type portPage struct {
	Switch   *switchPage
	Port     *monitor.Port
	Href     string
	Label    string
	Rows     []kvRow
	Events   []eventRow
	Span     string
	Spans    []string
	HasChart bool
	NoHist   bool
	Chart    chartData
	PeakText string
	Base     string
	Samples  int
}

type kvRow struct{ K, V string }

func (s *Server) portPage(w http.ResponseWriter, r *http.Request) {
	st, ok := s.lookupStatus(w, r)
	if !ok {
		return
	}
	ifi, err := strconv.Atoi(chi.URLParam(r, "ifindex"))
	if err != nil || st.Snap == nil || st.Snap.Port(ifi) == nil {
		s.errorPage(w, r, http.StatusNotFound, "error.not_found")
		return
	}
	port := st.Snap.Port(ifi)
	p := s.page(r, "", "dashboard", nil)
	base := "/dashboard/" + strconv.FormatInt(st.DeviceID, 10)
	spanKey, span := spanDuration(r.URL.Query().Get("span"))
	pp := &portPage{Switch: &switchPage{ID: st.DeviceID, Name: st.Name, EDM: s.isEDM(r, st.DeviceID)},
		Port: port, Label: port.Label(), Base: base, Span: spanKey, Rows: portRowsKV(p, st.Snap, port)}
	for _, sp := range chartSpans {
		pp.Spans = append(pp.Spans, sp.Key)
	}
	p.Title = st.Name + " · " + pp.Label
	if evs, err := s.DB.ListEvents(r.Context(), st.DeviceID, ifi, 100); err == nil {
		pp.Events = eventRows(p, evs, base)
	}
	if s.History != nil {
		samples, err := s.History.Series(r.Context(), st.DeviceID, ifi, span)
		if err != nil {
			s.Log.Warn("history query failed", "device", st.DeviceID, "err", err)
		}
		sort.Slice(samples, func(i, j int) bool { return samples[i].TS.Before(samples[j].TS) })
		if len(samples) > 0 {
			pp.Chart = buildChart(samples, span, time.Now(), p.T("sw.chart"))
			pp.HasChart = true
			pp.PeakText = fmtBps(pp.Chart.Max)
			pp.Samples = len(samples)
		}
	}
	pp.NoHist = !pp.HasChart
	p.Data = pp
	s.render(w, r, "port", p)
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func durText(p *Page, d time.Duration) string {
	sec := int64(d / time.Second)
	days, h, m := sec/86400, sec%86400/3600, sec%3600/60
	if days > 0 {
		return fmt.Sprintf("%d %s %dh %dm", days, p.T("dash.days"), h, m)
	}
	return fmt.Sprintf("%dh %dm", h, m)
}

func speedLabel(bps uint64) string {
	if bps == 0 {
		return "–"
	}
	return monitor.SpeedText(bps)
}

func speedClass(p *monitor.Port) string {
	switch {
	case p.Admin == "down":
		return "sadmin"
	case p.Oper != "up":
		return "sdown"
	case p.Speed >= 1e10:
		return "s10g"
	case p.Speed >= 1e9:
		return "s1g"
	case p.Speed >= 1e8:
		return "s100m"
	}
	return "s10m"
}

func shown(p *monitor.Port) bool { return p.Physical || p.IsLAG }

func stateText(pg *Page, p *monitor.Port) (text, class string) {
	switch {
	case p.Admin == "down":
		return pg.T("dash.admin_down"), "sadmin"
	case p.Oper == "up":
		return pg.T("sw.state_up"), "sup"
	}
	return pg.T("dash.down"), "sdown"
}

func faceplates(pg *Page, snap *monitor.Snapshot, base string) []unitView {
	byUnit := map[int][]*monitor.Port{}
	for _, p := range snap.Ports {
		if p.Physical && p.Number > 0 {
			byUnit[p.Unit] = append(byUnit[p.Unit], p)
		}
	}
	var nums []int
	for u := range byUnit {
		nums = append(nums, u)
	}
	sort.Ints(nums)
	var out []unitView
	for _, u := range nums {
		ports := byUnit[u]
		sort.Slice(ports, func(i, j int) bool { return ports[i].Number < ports[j].Number })
		uv := unitView{Number: u, Label: len(nums) > 1, Single: len(ports) <= 12}
		for _, p := range ports {
			cls := "tile " + speedClass(p)
			if p.PoE != nil && p.PoE.Status == "delivering" {
				cls += " poe"
			}
			if p.Mode == "trunk" || p.Mode == "hybrid" {
				cls += " trunk"
			}
			if p.Neighbor != nil && p.Neighbor.Bridge {
				cls += " uplink"
			}
			if p.InErrors+p.OutErrors > 0 {
				cls += " err"
			}
			if p.LAG != 0 {
				cls += " lagm"
			}
			title := p.Label()
			if p.Alias != "" {
				title += " – " + p.Alias
			}
			st, _ := stateText(pg, p)
			title += "\n" + st
			if p.Oper == "up" {
				title += " " + speedLabel(p.Speed)
			}
			if p.Neighbor != nil {
				title += "\n→ " + p.Neighbor.SysName
			}
			uv.Tiles = append(uv.Tiles, tileView{Class: cls, Num: p.Number, Title: title, Href: portHref(base, p.IfIndex)})
		}
		out = append(out, uv)
	}
	return out
}

func portHref(base string, ifIndex int) string { return base + "/port/" + strconv.Itoa(ifIndex) }

func vlanList(ids []int) string {
	if len(ids) == 0 {
		return ""
	}
	s := append([]int(nil), ids...)
	sort.Ints(s)
	var parts []string
	for i := 0; i < len(s); {
		j := i
		for j+1 < len(s) && s[j+1] == s[j]+1 {
			j++
		}
		if j > i {
			parts = append(parts, fmt.Sprintf("%d-%d", s[i], s[j]))
		} else {
			parts = append(parts, strconv.Itoa(s[i]))
		}
		i = j + 1
	}
	return strings.Join(parts, ", ")
}

func neighborText(n *monitor.Neighbor) string {
	if n == nil {
		return ""
	}
	if n.PortID != "" {
		return n.SysName + " (" + n.PortID + ")"
	}
	return n.SysName
}

func poeText(p *monitor.Port) string {
	if p.PoE == nil {
		return ""
	}
	t := p.PoE.Status
	if p.PoE.Class > 0 {
		t += ", class " + strconv.Itoa(p.PoE.Class)
	}
	if p.PoE.PowerMW > 0 {
		t += fmt.Sprintf(", %.1f W", float64(p.PoE.PowerMW)/1000)
	}
	return t
}

func portRows(pg *Page, snap *monitor.Snapshot, base string) []portRow {
	var out []portRow
	for _, p := range snap.Ports {
		if !shown(p) {
			continue
		}
		st, cls := stateText(pg, p)
		r := portRow{Idx: len(out), Port: p, Href: portHref(base, p.IfIndex), Label: p.Label(), State: st, StateClass: cls,
			SpeedBps: p.Speed, Untagged: vlanList(p.Untagged), Tagged: vlanList(p.Tagged), PoE: poeText(p), LAG: p.LAGName,
			Neighbor: neighborText(p.Neighbor), InBps: p.InBps, OutBps: p.OutBps, UtilVal: p.Utilization,
			Errors: int64(p.InErrors + p.OutErrors)}
		if p.Oper == "up" {
			r.Speed = speedLabel(p.Speed)
			if p.Duplex != "" {
				r.Speed += " " + p.Duplex
			}
			r.In, r.Out = fmtBps(p.InBps), fmtBps(p.OutBps)
			if p.Speed > 0 {
				r.Util, r.HasUtilization = fmt.Sprintf("%.1f %%", p.Utilization), true
			}
		}
		if r.LAG == "" && p.LAG != 0 {
			r.LAG = strconv.Itoa(p.LAG)
		}
		r.ErrorsText = fmt.Sprintf("%d / %d", p.InErrors, p.OutErrors)
		out = append(out, r)
	}
	return out
}

func vlanRows(snap *monitor.Snapshot, base string) []vlanRow {
	vlans := append([]monitor.VLAN(nil), snap.VLANs...)
	sort.Slice(vlans, func(i, j int) bool { return vlans[i].ID < vlans[j].ID })
	out := make([]vlanRow, len(vlans))
	idx := map[int]int{}
	for i, v := range vlans {
		out[i] = vlanRow{ID: v.ID, Name: v.Name}
		idx[v.ID] = i
	}
	for _, p := range snap.Ports {
		if !shown(p) {
			continue
		}
		ref := portRef{Label: p.Label(), Href: portHref(base, p.IfIndex)}
		for _, id := range p.Untagged {
			if i, ok := idx[id]; ok {
				out[i].Untagged = append(out[i].Untagged, ref)
			}
		}
		for _, id := range p.Tagged {
			if i, ok := idx[id]; ok {
				out[i].Tagged = append(out[i].Tagged, ref)
			}
		}
	}
	return out
}

func eventRows(pg *Page, evs []store.PortEvent, base string) []eventRow {
	out := make([]eventRow, 0, len(evs))
	for _, e := range evs {
		kind := e.Kind
		if hasKey("sw.ev_" + kind) {
			kind = pg.T("sw.ev_" + kind)
		}
		out = append(out, eventRow{At: e.At, Port: e.Port, PortHref: portHref(base, e.IfIndex), Kind: kind, From: e.From, To: e.To})
	}
	return out
}

func portRowsKV(pg *Page, snap *monitor.Snapshot, p *monitor.Port) []kvRow {
	var rows []kvRow
	add := func(k, v string) {
		if v != "" {
			rows = append(rows, kvRow{pg.T(k), v})
		}
	}
	st, _ := stateText(pg, p)
	add("sw.if_index", strconv.Itoa(p.IfIndex))
	add("sw.col_name", p.Name)
	add("sw.descr", p.Descr)
	add("dash.alias", p.Alias)
	add("dash.state", st+" ("+p.Admin+"/"+p.Oper+")")
	if p.Oper == "up" {
		sp := speedLabel(p.Speed)
		if p.Duplex != "" {
			sp += ", " + p.Duplex
		}
		add("dash.speed", sp)
	}
	add("dash.mode", p.Mode)
	if p.PVID > 0 {
		add("dash.pvid", strconv.Itoa(p.PVID))
	}
	add("dash.untagged", vlanList(p.Untagged))
	add("dash.tagged", vlanList(p.Tagged))
	add("PoE", poeText(p))
	lag := p.LAGName
	if lag == "" && p.LAG != 0 {
		lag = strconv.Itoa(p.LAG)
	}
	add("dash.lag", lag)
	if n := p.Neighbor; n != nil {
		v := n.SysName
		if n.PortID != "" {
			v += " (" + n.PortID + ")"
		}
		if n.PortDesc != "" {
			v += " – " + n.PortDesc
		}
		add("dash.neighbor", v)
	}
	if p.Oper == "up" {
		add("dash.traffic", pg.T("dash.in")+" "+fmtBps(p.InBps)+" · "+pg.T("dash.out")+" "+fmtBps(p.OutBps))
		if p.Speed > 0 {
			add("dash.utilization", fmt.Sprintf("%.1f %%", p.Utilization))
		}
	}
	add("dash.errors", fmt.Sprintf("%s %d · %s %d", pg.T("dash.in"), p.InErrors, pg.T("dash.out"), p.OutErrors))
	add("sw.discards", fmt.Sprintf("%s %d · %s %d", pg.T("dash.in"), p.InDiscards, pg.T("dash.out"), p.OutDiscards))
	if snap.System.Uptime > 0 && p.LastChange > 0 && snap.System.Uptime >= p.LastChange {
		add("dash.last_change", durText(pg, snap.System.Uptime-p.LastChange))
	}
	return rows
}
