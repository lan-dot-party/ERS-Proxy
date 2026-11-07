package web

import (
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/lan-dot-party/ers-proxy/internal/app"
	"github.com/lan-dot-party/ers-proxy/internal/monitor"
	"github.com/lan-dot-party/ers-proxy/internal/monitor/poller"
	"github.com/lan-dot-party/ers-proxy/internal/store"
	"github.com/lan-dot-party/ers-proxy/internal/switchcfg"
)

// Everything in this file exists only when Server.Configure is set (see
// configure.go): without it no route is registered and configView returns nil,
// so the templates render nothing of it.

// How long the status banner reminds of a finished change.
const (
	unsavedBannerAge = time.Hour
	failedBannerAge  = 10 * time.Minute
)

type vlanOption struct {
	ID     int
	Name   string
	Tagged bool
}

// portFormView is the "Settings" form of the port page.
type portFormView struct {
	IfIndex   int
	Alias     string
	Mode      string // keep, access, trunk or hybrid: preselected mode
	Modes     []string
	Untagged  int
	VLANs     []vlanOption
	ShowPoE   bool
	ShowAdmin bool
}

type vlanManageRow struct {
	ID   int
	Name string
}

type bannerView struct {
	Kind       string // pending, busy, revert_failed, unsaved, failed
	Summary    string
	Actor      string
	Err        string
	Deadline   int64
	Remaining  string
	ConfirmURL string
	RevertURL  string
}

type historyRow struct {
	At      time.Time
	Actor   string
	State   string
	Summary string
	Err     string
}

// configView is what the switch and port page show of the configuration
// feature. A nil *configView means: show nothing.
type configView struct {
	Base    string
	Caps    switchcfg.Capabilities
	Banner  *bannerView
	CanEdit bool // no open change and the switch can be changed
	Port    *portFormView
	// VLANs lists the VLANs for the management card (switch page).
	VLANs    []vlanManageRow
	VLANAny  bool
	MaxAlias int
	MaxVLAN  int
	History  []historyRow
	ShowHist bool
}

// assignmentText shows an assignment of the preview. Port bitmaps (VLAN and
// MLT members) are shown as the ports they contain, not as hex octets.
func assignmentText(a switchcfg.Assignment, snap *monitor.Snapshot) string {
	if a.Value.Kind != monitor.KindBytes || !strings.HasSuffix(a.Name, "PortMembers") {
		return a.String()
	}
	var names []string
	for _, i := range monitor.BitsMSB(a.Value.Bytes) {
		name := "ifIndex " + strconv.Itoa(i)
		if snap != nil {
			if p := snap.Port(i); p != nil && p.Name != "" {
				name = p.Name
			}
		}
		names = append(names, name)
	}
	if len(names) == 0 {
		return a.Instance() + " = –"
	}
	return a.Instance() + " = " + strings.Join(names, ", ")
}

func clock(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	sec := int(d.Round(time.Second) / time.Second)
	return fmt.Sprintf("%d:%02d", sec/60, sec%60)
}

// configBase builds the part of the view both pages share.
func (s *Server) configBase(r *http.Request, p *Page, deviceID int64, base string) *configView {
	if s.Configure == nil || !p.Can(store.RoleAdmin) {
		return nil
	}
	ctx := r.Context()
	cv := &configView{Base: base + "/configure"}
	caps, capsOK := s.Configure.Capabilities(ctx, deviceID)
	cv.Caps = caps
	open, err := s.DB.OpenConfigChangeForDevice(ctx, deviceID)
	if err != nil {
		open = nil
	}
	now := time.Now()
	switch {
	case open != nil:
		b := &bannerView{Summary: open.Summary, Actor: open.Actor, Err: open.Error,
			ConfirmURL: fmt.Sprintf("%s/%d/confirm", cv.Base, open.ID), RevertURL: fmt.Sprintf("%s/%d/revert", cv.Base, open.ID)}
		switch open.State {
		case store.ChangePending:
			b.Kind = "pending"
			b.Deadline = open.Deadline.Unix()
			b.Remaining = clock(open.Deadline.Sub(now))
		case store.ChangeRevertFailed:
			b.Kind = "revert_failed"
		default:
			b.Kind = "busy"
		}
		cv.Banner = b
	default:
		if list, err := s.DB.ListConfigChanges(ctx, deviceID, 1); err == nil && len(list) == 1 {
			c := list[0]
			end := c.FinishedAt
			if end.IsZero() {
				end = c.CreatedAt
			}
			switch {
			case c.State == store.ChangeConfirmedUnsaved && now.Sub(end) < unsavedBannerAge:
				cv.Banner = &bannerView{Kind: "unsaved", Summary: c.Summary, Actor: c.Actor, Err: c.Error}
			case c.State == store.ChangeFailed && now.Sub(end) < failedBannerAge:
				cv.Banner = &bannerView{Kind: "failed", Summary: c.Summary, Actor: c.Actor, Err: c.Error}
			}
		}
	}
	cv.CanEdit = capsOK && open == nil
	cv.MaxAlias, cv.MaxVLAN = caps.MaxAlias, caps.MaxVLANName
	return cv
}

func sortedVLANs(snap *monitor.Snapshot) []monitor.VLAN {
	v := append([]monitor.VLAN(nil), snap.VLANs...)
	sort.Slice(v, func(i, j int) bool { return v[i].ID < v[j].ID })
	return v
}

// switchConfig adds VLAN management and the change history to the switch page.
func (s *Server) switchConfig(r *http.Request, p *Page, st poller.Status, base string) *configView {
	cv := s.configBase(r, p, st.DeviceID, base)
	if cv == nil {
		return nil
	}
	if cv.CanEdit && st.Snap != nil && (cv.Caps.VLANCreate || cv.Caps.VLANRename || cv.Caps.VLANDelete) {
		cv.VLANAny = true
		for _, v := range sortedVLANs(st.Snap) {
			cv.VLANs = append(cv.VLANs, vlanManageRow{ID: v.ID, Name: v.Name})
		}
	}
	cv.ShowHist = true
	if list, err := s.DB.ListConfigChanges(r.Context(), st.DeviceID, 20); err == nil {
		for _, c := range list {
			cv.History = append(cv.History, historyRow{At: c.CreatedAt, Actor: c.Actor, State: c.State, Summary: c.Summary, Err: c.Error})
		}
	}
	return cv
}

func hasInt(v []int, n int) bool {
	for _, x := range v {
		if x == n {
			return true
		}
	}
	return false
}

// portConfig adds the settings form to the port page.
func (s *Server) portConfig(r *http.Request, p *Page, st poller.Status, port *monitor.Port, base string) *configView {
	cv := s.configBase(r, p, st.DeviceID, base)
	if cv == nil {
		return nil
	}
	c := cv.Caps
	if !cv.CanEdit || st.Snap == nil || !(c.Admin || c.Alias || c.PoE && port.PoE != nil || c.PortVLANs) {
		return cv
	}
	f := &portFormView{IfIndex: port.IfIndex, Alias: port.Alias, Mode: "keep",
		ShowAdmin: c.Admin, ShowPoE: c.PoE && port.PoE != nil}
	if c.PortVLANs {
		f.Modes = []string{"keep", "access", "trunk"}
		if c.Hybrid {
			f.Modes = append(f.Modes, "hybrid")
		}
		f.Untagged = untaggedOf(port)
		switch {
		case c.Hybrid && port.Mode == "hybrid" && f.Untagged != 0:
			f.Mode = "hybrid"
		case len(port.Tagged) == 0 && f.Untagged != 0:
			f.Mode = "access"
		case len(port.Tagged) > 0 && f.Untagged == 0:
			f.Mode = "trunk"
		case c.Hybrid && len(port.Tagged) > 0:
			f.Mode = "hybrid"
		}
		for _, v := range sortedVLANs(st.Snap) {
			f.VLANs = append(f.VLANs, vlanOption{ID: v.ID, Name: v.Name, Tagged: hasInt(port.Tagged, v.ID)})
		}
	}
	cv.Port = f
	return cv
}

// untaggedOf is the untagged VLAN of a port (its PVID if that is a member).
func untaggedOf(p *monitor.Port) int {
	if hasInt(p.Untagged, p.PVID) {
		return p.PVID
	}
	if len(p.Untagged) > 0 {
		return p.Untagged[0]
	}
	return 0
}

func sameSet(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	x, y := append([]int(nil), a...), append([]int(nil), b...)
	sort.Ints(x)
	sort.Ints(y)
	for i := range x {
		if x[i] != y[i] {
			return false
		}
	}
	return true
}

// ---- messages ----------------------------------------------------------

// configMessage turns an error of the form handling or the manager into a
// translated text. The detail (which may come from the switch) is appended;
// templates escape it.
func (s *Server) configMessage(p *Page, lead string, err error) (status int, msg string) {
	var ref *switchcfg.Refused
	switch {
	case errors.As(err, &ref):
		key := "config.refuse." + ref.Code
		if !hasKey(key) {
			key = "config.refuse.other"
		}
		msg = p.T(key)
		if ref.Detail != "" {
			msg += " (" + ref.Detail + ")"
		}
		return http.StatusUnprocessableEntity, msg
	case errors.Is(err, switchcfg.ErrDisabled):
		return http.StatusConflict, p.T("config.err.disabled")
	case errors.Is(err, switchcfg.ErrBusy):
		return http.StatusConflict, p.T("config.err.busy")
	case errors.Is(err, switchcfg.ErrPreview):
		return http.StatusConflict, p.T("config.err.preview")
	case errors.Is(err, switchcfg.ErrAck):
		return http.StatusUnprocessableEntity, p.T("config.err.ack")
	case errors.Is(err, switchcfg.ErrStale):
		return http.StatusConflict, p.T("config.err.stale")
	case errors.Is(err, switchcfg.ErrNotPending):
		return http.StatusConflict, p.T("config.err.not_pending")
	case errors.Is(err, switchcfg.ErrNoPlanner):
		return http.StatusUnprocessableEntity, p.T("config.err.no_planner")
	case errors.Is(err, store.ErrNotFound):
		return http.StatusNotFound, p.T("error.not_found")
	}
	return http.StatusBadGateway, p.T(lead) + ": " + err.Error()
}

// configFail shows an error page with a link back.
func (s *Server) configFail(w http.ResponseWriter, r *http.Request, lead string, err error, back string) {
	p := s.page(r, "", "dashboard", back)
	status, msg := s.configMessage(p, lead, err)
	p.Error = msg
	s.renderStatus(w, r, status, "error", p)
}

func actor(r *http.Request) switchcfg.Actor {
	u := app.User(r.Context())
	return switchcfg.Actor{UserID: u.ID, Name: u.Username}
}

func (s *Server) deviceBase(r *http.Request) (id int64, base string, ok bool) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		return 0, "", false
	}
	return id, "/dashboard/" + strconv.FormatInt(id, 10), true
}

// ---- preview -----------------------------------------------------------

func (s *Server) configPreview(w http.ResponseWriter, r *http.Request) {
	id, base, ok := s.deviceBase(r)
	if !ok {
		s.errorPage(w, r, http.StatusNotFound, "error.not_found")
		return
	}
	st, ok := s.lookupStatus(w, r)
	if !ok {
		return
	}
	back := base
	var change switchcfg.Change
	var err error
	switch r.PostFormValue("kind") {
	case "port":
		back = base + "/port/" + strconv.Itoa(formInt(r, "ifindex"))
		change, err = s.portChange(r, st)
	case "vlan":
		change, err = vlanChange(r)
	default:
		err = switchcfg.Refuse("empty", "")
	}
	if err == nil && change.Empty() {
		err = switchcfg.Refuse("no_change", "")
	}
	if err != nil {
		s.configFail(w, r, "config.err.failed", err, back)
		return
	}
	pv, err := s.Configure.Preview(r.Context(), id, actor(r), change)
	if err != nil {
		s.configFail(w, r, "config.err.failed", err, back)
		return
	}
	redirect(w, r, base+"/configure/preview/"+pv.ID)
}

func formInt(r *http.Request, key string) int {
	n, _ := strconv.Atoi(strings.TrimSpace(r.PostFormValue(key)))
	return n
}

// vlanChange reads the VLAN forms (create, rename, delete).
func vlanChange(r *http.Request) (switchcfg.Change, error) {
	op := r.PostFormValue("vlan_op")
	switch op {
	case switchcfg.VLANCreate, switchcfg.VLANRename, switchcfg.VLANDelete:
	default:
		return switchcfg.Change{}, switchcfg.Refuse("vlan_op", "%q", op)
	}
	id := formInt(r, "vlan_id")
	if id < 1 || id > 4094 {
		return switchcfg.Change{}, switchcfg.Refuse("vlan_id", "")
	}
	vc := switchcfg.VLANChange{Op: op, ID: id}
	if op != switchcfg.VLANDelete {
		vc.Name = strings.TrimSpace(r.PostFormValue("vlan_name"))
	}
	return switchcfg.Change{VLANs: []switchcfg.VLANChange{vc}}, nil
}

// portChange reads the port settings form. Only fields that differ from the
// port's current values go into the change.
func (s *Server) portChange(r *http.Request, st poller.Status) (switchcfg.Change, error) {
	ifi := formInt(r, "ifindex")
	if st.Snap == nil || st.Snap.Port(ifi) == nil {
		return switchcfg.Change{}, switchcfg.Refuse("port", "")
	}
	port := st.Snap.Port(ifi)
	pc := switchcfg.PortChange{IfIndex: ifi}
	switch r.PostFormValue("admin") {
	case "up":
		if port.Admin != "up" {
			t := true
			pc.Admin = &t
		}
	case "down":
		if port.Admin != "down" {
			f := false
			pc.Admin = &f
		}
	}
	if _, ok := r.PostForm["alias"]; ok {
		if a := r.PostFormValue("alias"); a != port.Alias {
			pc.Alias = &a
		}
	}
	if port.PoE != nil {
		on := port.PoE.Status != "disabled"
		switch r.PostFormValue("poe") {
		case "on":
			if !on {
				t := true
				pc.PoE = &t
			}
		case "off":
			if on {
				f := false
				pc.PoE = &f
			}
		}
	}
	if mode := r.PostFormValue("vlan_mode"); mode != "" && mode != "keep" {
		v := &switchcfg.PortVLANs{}
		if mode == "access" || mode == "hybrid" {
			v.Untagged = formInt(r, "untagged")
		}
		if mode == "trunk" || mode == "hybrid" {
			for _, t := range r.PostForm["tagged"] {
				if n, err := strconv.Atoi(t); err == nil {
					v.Tagged = append(v.Tagged, n)
				}
			}
			sort.Ints(v.Tagged)
		}
		switch mode {
		case "access":
			if v.Untagged == 0 {
				return switchcfg.Change{}, switchcfg.Refuse("no_untagged", "")
			}
		case "hybrid":
			// Tagging with an untagged VLAN, even without tagged VLANs yet.
			if v.Untagged == 0 {
				return switchcfg.Change{}, switchcfg.Refuse("no_untagged", "")
			}
			v.Hybrid = true
		case "trunk":
		default:
			return switchcfg.Change{}, switchcfg.Refuse("empty", "")
		}
		cur := port.Untagged
		hybridNow := port.Mode == "hybrid"
		if !(sameSet(cur, untaggedSet(v.Untagged)) && sameSet(port.Tagged, v.Tagged) &&
			(mode != "hybrid" || hybridNow) && (mode != "access" || !hybridNow)) {
			pc.VLANs = v
		}
	}
	if pc.Admin == nil && pc.Alias == nil && pc.PoE == nil && pc.VLANs == nil {
		return switchcfg.Change{}, nil
	}
	return switchcfg.Change{Ports: []switchcfg.PortChange{pc}}, nil
}

func untaggedSet(n int) []int {
	if n == 0 {
		return nil
	}
	return []int{n}
}

type stepRow struct {
	Description string
	Sets        []string
}

type previewPage struct {
	Name      string
	Base      string
	PreviewID string
	Hash      string
	Summary   []string
	Steps     []stepRow
	Warnings  []string
	SaveKnown bool
	Timeout   string
}

func (s *Server) warningText(p *Page, snap *monitor.Snapshot, w switchcfg.Warning) string {
	key := "config.warn." + w.Code
	if !hasKey(key) {
		key = "config.warn.other"
	}
	var lead []string
	if w.IfIndex != 0 {
		l := strconv.Itoa(w.IfIndex)
		if snap != nil {
			if port := snap.Port(w.IfIndex); port != nil {
				l = port.Label()
			}
		}
		lead = append(lead, l)
	}
	if w.VLAN != 0 {
		lead = append(lead, "VLAN "+strconv.Itoa(w.VLAN))
	}
	t := p.T(key)
	if len(lead) > 0 {
		t = strings.Join(lead, ", ") + ": " + t
	}
	if w.Detail != "" {
		t += " (" + w.Detail + ")"
	}
	return t
}

// previewData loads the preview of the URL for the current user.
func (s *Server) previewData(w http.ResponseWriter, r *http.Request, pid string) (*switchcfg.Preview, poller.Status, string, bool) {
	id, base, ok := s.deviceBase(r)
	if !ok {
		s.errorPage(w, r, http.StatusNotFound, "error.not_found")
		return nil, poller.Status{}, "", false
	}
	pv, ok := s.Configure.PreviewByID(pid, actor(r))
	if !ok || pv.DeviceID != id {
		s.configFail(w, r, "config.err.failed", switchcfg.ErrPreview, base)
		return nil, poller.Status{}, "", false
	}
	st, _ := s.Monitor.Status(id)
	return pv, st, base, true
}

func (s *Server) configPreviewPage(w http.ResponseWriter, r *http.Request) {
	pv, st, base, ok := s.previewData(w, r, chi.URLParam(r, "pid"))
	if !ok {
		return
	}
	s.renderPreview(w, r, http.StatusOK, pv, st, base, "")
}

func (s *Server) renderPreview(w http.ResponseWriter, r *http.Request, status int, pv *switchcfg.Preview, st poller.Status, base, errText string) {
	p := s.page(r, "config.preview_title", "dashboard", nil)
	pp := &previewPage{Name: st.Name, Base: base, PreviewID: pv.ID, Hash: pv.Hash, Summary: pv.Plan.Summary,
		SaveKnown: pv.Plan.SaveKnown, Timeout: clock(s.Cfg.Configure.ConfirmTimeout) + " min"}
	for _, step := range pv.Plan.Steps {
		row := stepRow{Description: step.Description}
		for _, a := range step.Set {
			row.Sets = append(row.Sets, assignmentText(a, st.Snap))
		}
		pp.Steps = append(pp.Steps, row)
	}
	for _, wn := range pv.Plan.Warnings {
		pp.Warnings = append(pp.Warnings, s.warningText(p, st.Snap, wn))
	}
	if errText != "" {
		p.Error = errText
	}
	p.Data = pp
	s.renderStatus(w, r, status, "config_preview", p)
}

// ---- apply, confirm, revert -------------------------------------------

func (s *Server) configApply(w http.ResponseWriter, r *http.Request) {
	pid, hash := r.PostFormValue("preview"), r.PostFormValue("hash")
	ack := r.PostFormValue("ack") == "1"
	pv, st, base, ok := s.previewData(w, r, pid)
	if !ok {
		return
	}
	if len(pv.Plan.Warnings) > 0 && !ack {
		s.renderPreview(w, r, http.StatusUnprocessableEntity, pv, st, base, translate(language(r), "config.ack_required"))
		return
	}
	name := st.Name
	if d, err := s.DB.DeviceByID(r.Context(), pv.DeviceID); err == nil {
		name = d.Name
	}
	c, err := s.Configure.Apply(r.Context(), pid, hash, ack, actor(r))
	if err == nil && c != nil && c.State == store.ChangeFailed {
		err = errors.New(c.Error)
	}
	if err != nil {
		s.Audit(r, "config.apply_failed", name, err.Error())
		s.configFail(w, r, "config.err.apply_failed", err, base)
		return
	}
	s.Audit(r, "config.apply", name, c.Summary)
	redirect(w, r, base+"?ok=config_applied")
}

// changeOf loads the change of the URL and checks that it belongs to the device.
func (s *Server) changeOf(w http.ResponseWriter, r *http.Request) (*store.ConfigChange, string, string, bool) {
	id, base, ok := s.deviceBase(r)
	cid, err := strconv.ParseInt(chi.URLParam(r, "change"), 10, 64)
	if !ok || err != nil {
		s.errorPage(w, r, http.StatusNotFound, "error.not_found")
		return nil, "", "", false
	}
	c, err := s.DB.GetConfigChange(r.Context(), cid)
	if err != nil || c.DeviceID != id {
		s.errorPage(w, r, http.StatusNotFound, "error.not_found")
		return nil, "", "", false
	}
	name := strconv.FormatInt(id, 10)
	if d, err := s.DB.DeviceByID(r.Context(), id); err == nil {
		name = d.Name
	}
	return c, base, name, true
}

func (s *Server) configConfirm(w http.ResponseWriter, r *http.Request) {
	c, base, name, ok := s.changeOf(w, r)
	if !ok {
		return
	}
	res, err := s.Configure.Confirm(r.Context(), c.ID, actor(r))
	if err != nil {
		s.configFail(w, r, "config.err.confirm_failed", err, base)
		return
	}
	if res == nil {
		res = c
	}
	s.Audit(r, "config.confirm", name, res.Summary)
	redirect(w, r, base+"?ok=config_confirmed")
}

func (s *Server) configRevert(w http.ResponseWriter, r *http.Request) {
	c, base, name, ok := s.changeOf(w, r)
	if !ok {
		return
	}
	res, err := s.Configure.Revert(r.Context(), c.ID, actor(r))
	if err != nil {
		s.configFail(w, r, "config.err.revert_failed", err, base)
		return
	}
	if res == nil {
		res = c
	}
	s.Audit(r, "config.revert", name, res.Summary)
	redirect(w, r, base+"?ok=config_reverted")
}
