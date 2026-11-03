package web

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/lan-dot-party/ers-proxy/internal/app"
	"github.com/lan-dot-party/ers-proxy/internal/auth"
	"github.com/lan-dot-party/ers-proxy/internal/devices"
	"github.com/lan-dot-party/ers-proxy/internal/snmp"
	"github.com/lan-dot-party/ers-proxy/internal/store"
)

func idParam(r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	return id, err == nil && id > 0
}

// --- devices ---

type devicesData struct {
	Devices []*store.Device
	Import  *devices.ImportResult
}

func (s *Server) devices(w http.ResponseWriter, r *http.Request) {
	devs, err := s.DB.ListDevices(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.render(w, r, "devices", s.page(r, "devices.title", "admin", devicesData{Devices: devs}))
}

type deviceFormData struct {
	Device  *store.Device
	Groups  []*store.Group
	New     bool
	ErrKey  string
	Pending string // fingerprint offered by a connection test, to confirm
}

func (s *Server) deviceForm(w http.ResponseWriter, r *http.Request, status int, d *store.Device, isNew bool, errKey string) {
	groups, err := s.DB.ListGroups(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	title := "devices.edit"
	if isNew {
		title = "devices.new"
	}
	p := s.page(r, title, "admin", deviceFormData{Device: d, Groups: groups, New: isNew, ErrKey: errKey})
	if errKey != "" {
		p.Error = p.T("device.err." + errKey)
	}
	s.renderStatus(w, r, status, "device_form", p)
}

func (s *Server) deviceNew(w http.ResponseWriter, r *http.Request) {
	d := &store.Device{Kind: store.KindERS, OpenMode: store.OpenInherit, Isolation: store.IsolationSandbox,
		TLSMode: store.TLSAuto, HealthEnabled: true, MonitorSource: store.MonitorNone, MonitorDriver: store.DriverAuto,
		MonitorInterval: devices.DefaultMonitorInterval, SNMPVersion: store.SNMPv2c, SNMPPort: 161}
	if k := r.URL.Query().Get("kind"); k == store.KindLink {
		d.Kind = k
	}
	s.deviceForm(w, r, http.StatusOK, d, true, "")
}

// readDevice fills d from the form.
func readDevice(r *http.Request, d *store.Device) {
	d.Name = r.PostFormValue("name")
	d.Kind = r.PostFormValue("kind")
	d.URL = r.PostFormValue("url")
	d.OpenMode = r.PostFormValue("open_mode")
	d.Isolation = r.PostFormValue("isolation")
	d.TLSMode = r.PostFormValue("tls_mode")
	d.Notes = r.PostFormValue("notes")
	d.HealthEnabled = r.PostFormValue("health_enabled") == "1"
	d.AutoLogin = r.PostFormValue("auto_login") == "1"
	d.GroupID, _ = strconv.ParseInt(r.PostFormValue("group_id"), 10, 64)
	d.Sort, _ = strconv.Atoi(r.PostFormValue("sort"))
	d.MonitorSource = r.PostFormValue("monitor_source")
	d.MonitorDriver = r.PostFormValue("monitor_driver")
	d.MonitorInterval, _ = strconv.Atoi(strings.TrimSpace(r.PostFormValue("monitor_interval")))
	d.SNMPVersion = r.PostFormValue("snmp_version")
	d.SNMPPort, _ = strconv.Atoi(strings.TrimSpace(r.PostFormValue("snmp_port")))
}

func fieldKey(err error) string {
	var fe *devices.FieldError
	if errors.As(err, &fe) {
		return fe.Key
	}
	return "invalid"
}

func (s *Server) deviceCreate(w http.ResponseWriter, r *http.Request) {
	d := &store.Device{}
	readDevice(r, d)
	if err := devices.Normalize(d); err != nil {
		s.deviceForm(w, r, http.StatusUnprocessableEntity, d, true, fieldKey(err))
		return
	}
	if err := s.DB.CreateDevice(r.Context(), d); err != nil {
		s.fail(w, r, err)
		return
	}
	s.Audit(r, "device.create", d.Name, d.Kind+" "+d.URL)
	s.UpdateFrameSources(r.Context())
	s.deviceChanged(d.ID)
	redirect(w, r, "/admin/devices?ok=saved")
}

func (s *Server) loadDevice(w http.ResponseWriter, r *http.Request) (*store.Device, bool) {
	id, ok := idParam(r)
	if !ok {
		s.errorPage(w, r, http.StatusNotFound, "error.not_found")
		return nil, false
	}
	d, err := s.DB.DeviceByID(r.Context(), id)
	if err != nil {
		s.fail(w, r, err)
		return nil, false
	}
	return d, true
}

func (s *Server) deviceEdit(w http.ResponseWriter, r *http.Request) {
	d, ok := s.loadDevice(w, r)
	if !ok {
		return
	}
	s.deviceForm(w, r, http.StatusOK, d, false, "")
}

func (s *Server) deviceUpdate(w http.ResponseWriter, r *http.Request) {
	before, ok := s.loadDevice(w, r)
	if !ok {
		return
	}
	d := *before
	readDevice(r, &d)
	if err := devices.Normalize(&d); err != nil {
		s.deviceForm(w, r, http.StatusUnprocessableEntity, &d, false, fieldKey(err))
		return
	}
	moved := devices.AddressChanged(before, &d)
	if err := s.DB.UpdateDevice(r.Context(), &d); err != nil {
		s.fail(w, r, err)
		return
	}
	if moved {
		// A new address must not inherit trust: drop the certificate pin and
		// the stored login (which is bound to the old address anyway).
		if err := s.DB.SetDeviceTLS(r.Context(), d.ID, "", ""); err != nil {
			s.fail(w, r, err)
			return
		}
		if before.HasCredentials() {
			if err := s.DB.SetDeviceCredentials(r.Context(), d.ID, nil); err != nil {
				s.fail(w, r, err)
				return
			}
		}
	}
	details := d.Kind + " " + d.URL
	if moved {
		details += " (address changed, pin and login cleared)"
	}
	// SNMP credentials are bound to host and SNMP port.
	if (moved || before.SNMPPort != d.SNMPPort) && before.HasSNMPCredentials() {
		if err := s.DB.SetDeviceSNMPCredentials(r.Context(), d.ID, nil); err != nil {
			s.fail(w, r, err)
			return
		}
		details += " (SNMP credentials cleared)"
	}
	s.Audit(r, "device.update", d.Name, details)
	s.UpdateFrameSources(r.Context())
	s.deviceChanged(d.ID)
	redirect(w, r, "/admin/devices?ok=saved")
}

func (s *Server) deviceDelete(w http.ResponseWriter, r *http.Request) {
	d, ok := s.loadDevice(w, r)
	if !ok {
		return
	}
	if err := s.DB.DeleteDevice(r.Context(), d.ID); err != nil {
		s.fail(w, r, err)
		return
	}
	s.Audit(r, "device.delete", d.Name, d.URL)
	s.UpdateFrameSources(r.Context())
	s.deviceChanged(d.ID)
	redirect(w, r, "/admin/devices?ok=deleted")
}

// deviceCredentials stores or clears the login used for auto-login.
func (s *Server) deviceCredentials(w http.ResponseWriter, r *http.Request) {
	d, ok := s.loadDevice(w, r)
	if !ok {
		return
	}
	back := fmt.Sprintf("/admin/devices/%d/edit", d.ID)
	if !s.confirmPassword(r) {
		redirect(w, r, back+"?err=wrong_password")
		return
	}
	if r.PostFormValue("clear") == "1" {
		if err := s.DB.SetDeviceCredentials(r.Context(), d.ID, nil); err != nil {
			s.fail(w, r, err)
			return
		}
		s.Audit(r, "device.credentials.clear", d.Name, "")
		redirect(w, r, back+"?ok=saved")
		return
	}
	c := devices.Credentials{Username: strings.TrimSpace(r.PostFormValue("username")), Password: r.PostFormValue("password")}
	if c.Username == "" || c.Password == "" {
		redirect(w, r, back+"?err=credentials_missing")
		return
	}
	enc, err := devices.EncryptCredentials(s.Key, d, c)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if err := s.DB.SetDeviceCredentials(r.Context(), d.ID, enc); err != nil {
		s.fail(w, r, err)
		return
	}
	s.Audit(r, "device.credentials.set", d.Name, "") // never the values
	s.deviceChanged(d.ID)
	redirect(w, r, back+"?ok=saved")
}

// deviceSNMP stores or clears the SNMP credentials. The form is write-only:
// stored values are never shown again.
func (s *Server) deviceSNMP(w http.ResponseWriter, r *http.Request) {
	d, ok := s.loadDevice(w, r)
	if !ok {
		return
	}
	back := fmt.Sprintf("/admin/devices/%d/edit", d.ID)
	if !s.confirmPassword(r) {
		redirect(w, r, back+"?err=wrong_password")
		return
	}
	if r.PostFormValue("clear") == "1" {
		if err := s.DB.SetDeviceSNMPCredentials(r.Context(), d.ID, nil); err != nil {
			s.fail(w, r, err)
			return
		}
		s.Audit(r, "device.snmp.clear", d.Name, "")
		s.deviceChanged(d.ID)
		redirect(w, r, back+"?ok=saved")
		return
	}
	var c snmp.Credentials
	if d.SNMPVersion == store.SNMPv3 {
		c = snmp.Credentials{
			Username:      strings.TrimSpace(r.PostFormValue("username")),
			SecurityLevel: r.PostFormValue("security_level"),
			ContextName:   strings.TrimSpace(r.PostFormValue("context_name")),
		}
		if c.SecurityLevel != "noAuthNoPriv" {
			c.AuthProtocol = r.PostFormValue("auth_protocol")
			c.AuthPassword = r.PostFormValue("auth_password")
		}
		if c.SecurityLevel == "authPriv" {
			c.PrivProtocol = r.PostFormValue("priv_protocol")
			c.PrivPassword = r.PostFormValue("priv_password")
		}
	} else {
		c.Community = r.PostFormValue("community")
	}
	host, _ := devices.HostPort(d.URL)
	if err := (snmp.Target{Host: host, Version: d.SNMPVersion, Creds: c}).Validate(); err != nil || len(c.Community) > 128 {
		redirect(w, r, back+"?err=snmp_invalid")
		return
	}
	enc, err := devices.EncryptSNMP(s.Key, d, c)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if err := s.DB.SetDeviceSNMPCredentials(r.Context(), d.ID, enc); err != nil {
		s.fail(w, r, err)
		return
	}
	s.Audit(r, "device.snmp.set", d.Name, "") // never the values
	s.deviceChanged(d.ID)
	redirect(w, r, back+"?ok=saved")
}

// deviceTLS confirms or clears the pinned certificate.
func (s *Server) deviceTLS(w http.ResponseWriter, r *http.Request) {
	d, ok := s.loadDevice(w, r)
	if !ok {
		return
	}
	back := fmt.Sprintf("/admin/devices/%d/edit", d.ID)
	if b := r.PostFormValue("back"); b == fmt.Sprintf("/open/%d", d.ID) {
		back = b // confirmed from the trust page: continue opening the device
	}
	pin := ""
	if r.PostFormValue("clear") != "1" {
		check := &store.Device{Name: d.Name, Kind: d.Kind, URL: d.URL, TLSPin: r.PostFormValue("pin")}
		if err := devices.Normalize(check); err != nil || check.TLSPin == "" {
			redirect(w, r, back+"?err=bad_request")
			return
		}
		pin = check.TLSPin
	}
	if err := s.DB.SetDeviceTLS(r.Context(), d.ID, pin, d.TLSProfile); err != nil {
		s.fail(w, r, err)
		return
	}
	if pin == "" {
		s.Audit(r, "device.tls.clear", d.Name, "")
	} else {
		s.Audit(r, "device.tls.pin", d.Name, pin)
	}
	s.deviceChanged(d.ID)
	if strings.HasPrefix(back, "/open/") {
		frameable(w)
		redirect(w, r, back)
		return
	}
	redirect(w, r, back+"?ok=saved")
}

func (s *Server) deviceExport(w http.ResponseWriter, r *http.Request) {
	format := chi.URLParam(r, "format")
	e, err := devices.Build(r.Context(), s.DB)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	name := "ers-proxy-devices-" + time.Now().Format("2006-01-02")
	var buf bytes.Buffer
	switch format {
	case "json":
		err = devices.WriteJSON(&buf, e)
		w.Header().Set("Content-Type", "application/json")
	case "csv":
		err = devices.WriteCSV(&buf, e)
		w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	default:
		s.errorPage(w, r, http.StatusNotFound, "error.not_found")
		return
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.Audit(r, "device.export", format, fmt.Sprintf("%d devices", len(e.Devices)))
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+"."+format+`"`)
	_, _ = w.Write(buf.Bytes())
}

func (s *Server) deviceImport(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(4 << 20); err != nil {
		redirect(w, r, "/admin/devices?err=import_failed")
		return
	}
	f, hdr, err := r.FormFile("file")
	if err != nil {
		redirect(w, r, "/admin/devices?err=import_failed")
		return
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, 4<<20))
	if err != nil {
		redirect(w, r, "/admin/devices?err=import_failed")
		return
	}
	var e *devices.Export
	if strings.HasSuffix(strings.ToLower(hdr.Filename), ".csv") {
		e, err = devices.ReadCSV(bytes.NewReader(data))
	} else {
		e, err = devices.ReadJSON(bytes.NewReader(data))
	}
	p := s.page(r, "devices.title", "admin", nil)
	if err != nil {
		p.Error = p.T("error.import_failed") + " " + err.Error()
		s.renderDevicesWith(w, r, p, nil)
		return
	}
	res, err := devices.Import(r.Context(), s.DB, e)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.Audit(r, "device.import", hdr.Filename, fmt.Sprintf("created=%d skipped=%d errors=%d", res.Created, res.Skipped, len(res.Errors)))
	s.UpdateFrameSources(r.Context())
	s.deviceChanged(0)
	p.Flash = p.T("flash.imported", res.Created, res.Skipped, res.GroupsCreated)
	s.renderDevicesWith(w, r, p, res)
}

func (s *Server) renderDevicesWith(w http.ResponseWriter, r *http.Request, p *Page, res *devices.ImportResult) {
	devs, err := s.DB.ListDevices(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	p.Data = devicesData{Devices: devs, Import: res}
	s.render(w, r, "devices", p)
}

// --- groups ---

type groupsData struct {
	Groups []*store.Group
	Counts map[int64]int
}

func (s *Server) groups(w http.ResponseWriter, r *http.Request) {
	groups, err := s.DB.ListGroups(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	devs, err := s.DB.ListDevices(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	counts := map[int64]int{}
	for _, d := range devs {
		counts[d.GroupID]++
	}
	s.render(w, r, "groups", s.page(r, "groups.title", "admin", groupsData{Groups: groups, Counts: counts}))
}

func (s *Server) groupCreate(w http.ResponseWriter, r *http.Request) {
	sort, _ := strconv.Atoi(r.PostFormValue("sort"))
	g := &store.Group{Name: r.PostFormValue("name"), Sort: sort}
	if err := s.DB.CreateGroup(r.Context(), g); err != nil {
		if errors.Is(err, store.ErrExists) {
			redirect(w, r, "/admin/groups?err=exists")
			return
		}
		redirect(w, r, "/admin/groups?err=bad_request")
		return
	}
	s.Audit(r, "group.create", g.Name, "")
	redirect(w, r, "/admin/groups?ok=saved")
}

func (s *Server) groupUpdate(w http.ResponseWriter, r *http.Request) {
	id, ok := idParam(r)
	if !ok {
		s.errorPage(w, r, http.StatusNotFound, "error.not_found")
		return
	}
	sort, _ := strconv.Atoi(r.PostFormValue("sort"))
	g := &store.Group{ID: id, Name: r.PostFormValue("name"), Sort: sort}
	if err := s.DB.UpdateGroup(r.Context(), g); err != nil {
		if errors.Is(err, store.ErrExists) {
			redirect(w, r, "/admin/groups?err=exists")
			return
		}
		redirect(w, r, "/admin/groups?err=bad_request")
		return
	}
	s.Audit(r, "group.update", g.Name, "")
	redirect(w, r, "/admin/groups?ok=saved")
}

func (s *Server) groupDelete(w http.ResponseWriter, r *http.Request) {
	id, ok := idParam(r)
	if !ok {
		s.errorPage(w, r, http.StatusNotFound, "error.not_found")
		return
	}
	if err := s.DB.DeleteGroup(r.Context(), id); err != nil {
		s.fail(w, r, err)
		return
	}
	s.Audit(r, "group.delete", strconv.FormatInt(id, 10), "")
	redirect(w, r, "/admin/groups?ok=deleted")
}

// --- users ---

func (s *Server) users(w http.ResponseWriter, r *http.Request) {
	users, err := s.DB.ListUsers(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.render(w, r, "users", s.page(r, "users.title", "admin", users))
}

func (s *Server) userCreate(w http.ResponseWriter, r *http.Request) {
	if !s.confirmPassword(r) {
		redirect(w, r, "/admin/users?err=wrong_password")
		return
	}
	hash, err := auth.HashPassword(r.PostFormValue("password"))
	if err != nil {
		redirect(w, r, "/admin/users?err=password_short")
		return
	}
	lang := r.PostFormValue("language")
	if lang != "de" {
		lang = "en"
	}
	u := &store.User{Username: r.PostFormValue("username"), PasswordHash: hash, Role: r.PostFormValue("role"), Language: lang}
	if err := s.DB.CreateUser(r.Context(), u); err != nil {
		if errors.Is(err, store.ErrExists) {
			redirect(w, r, "/admin/users?err=exists")
			return
		}
		redirect(w, r, "/admin/users?err=bad_request")
		return
	}
	s.Audit(r, "user.create", u.Username, "role="+u.Role)
	redirect(w, r, "/admin/users?ok=saved")
}

// lastAdminGuard refuses changes that would leave no enabled admin.
func (s *Server) lastAdminGuard(r *http.Request, target *store.User, newRole string, disable bool) bool {
	if target.Role != store.RoleAdmin || target.Disabled {
		return true
	}
	if newRole == store.RoleAdmin && !disable {
		return true
	}
	n, err := s.DB.CountAdmins(r.Context())
	return err == nil && n > 1
}

func (s *Server) loadUser(w http.ResponseWriter, r *http.Request) (*store.User, bool) {
	id, ok := idParam(r)
	if !ok {
		s.errorPage(w, r, http.StatusNotFound, "error.not_found")
		return nil, false
	}
	u, err := s.DB.UserByID(r.Context(), id)
	if err != nil {
		s.fail(w, r, err)
		return nil, false
	}
	return u, true
}

func (s *Server) userUpdate(w http.ResponseWriter, r *http.Request) {
	u, ok := s.loadUser(w, r)
	if !ok {
		return
	}
	role := r.PostFormValue("role")
	disabled := r.PostFormValue("disabled") == "1"
	if store.RoleLevel(role) == 0 {
		redirect(w, r, "/admin/users?err=bad_request")
		return
	}
	if !s.lastAdminGuard(r, u, role, disabled) {
		redirect(w, r, "/admin/users?err=last_admin")
		return
	}
	if err := s.DB.UpdateUser(r.Context(), u.ID, role, disabled); err != nil {
		s.fail(w, r, err)
		return
	}
	s.Audit(r, "user.update", u.Username, fmt.Sprintf("role=%s disabled=%v", role, disabled))
	redirect(w, r, "/admin/users?ok=saved")
}

func (s *Server) userPassword(w http.ResponseWriter, r *http.Request) {
	u, ok := s.loadUser(w, r)
	if !ok {
		return
	}
	if !s.confirmPassword(r) {
		redirect(w, r, "/admin/users?err=wrong_password")
		return
	}
	hash, err := auth.HashPassword(r.PostFormValue("password"))
	if err != nil {
		redirect(w, r, "/admin/users?err=password_short")
		return
	}
	if err := s.DB.SetPassword(r.Context(), u.ID, hash); err != nil {
		s.fail(w, r, err)
		return
	}
	s.Audit(r, "user.password", u.Username, "by admin")
	if u.ID == app.User(r.Context()).ID {
		redirect(w, r, "/login")
		return
	}
	redirect(w, r, "/admin/users?ok=saved")
}

func (s *Server) userDelete(w http.ResponseWriter, r *http.Request) {
	u, ok := s.loadUser(w, r)
	if !ok {
		return
	}
	if u.ID == app.User(r.Context()).ID {
		redirect(w, r, "/admin/users?err=self")
		return
	}
	if !s.lastAdminGuard(r, u, "", true) {
		redirect(w, r, "/admin/users?err=last_admin")
		return
	}
	if err := s.DB.DeleteUser(r.Context(), u.ID); err != nil {
		s.fail(w, r, err)
		return
	}
	s.Audit(r, "user.delete", u.Username, "")
	redirect(w, r, "/admin/users?ok=deleted")
}

// --- audit and diagnostics ---

func (s *Server) audit(w http.ResponseWriter, r *http.Request) {
	entries, err := s.DB.ListAudit(r.Context(), 300)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.render(w, r, "audit", s.page(r, "audit.title", "admin", entries))
}

type diagDevice struct {
	*store.Device
	Rows []DiagRow
}

type diagData struct {
	Version  string
	Origin   string
	Insecure bool
	Devices  []diagDevice
}

func (s *Server) diagnostics(w http.ResponseWriter, r *http.Request) {
	devs, err := s.DB.ListDevices(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	data := diagData{Origin: s.Cfg.Origin(), Insecure: !s.secureRequest(r)}
	for _, d := range devs {
		if !d.Proxied() {
			continue
		}
		dd := diagDevice{Device: d}
		if s.Diagnostics != nil {
			dd.Rows = s.Diagnostics(d)
		}
		data.Devices = append(data.Devices, dd)
	}
	p := s.page(r, "diag.title", "admin", data)
	data.Version = p.Version
	p.Data = data
	s.render(w, r, "diagnostics", p)
}
