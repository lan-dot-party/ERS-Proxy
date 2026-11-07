package gwn_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/lan-dot-party/ers-proxy/internal/gwn"
	"github.com/lan-dot-party/ers-proxy/internal/gwn/gwntest"
)

func TestPortMap(t *testing.T) {
	cases := []struct {
		in   string
		want []int
	}{
		{"0x7FE", []int{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}},
		{"0x4", []int{2}},
		{"0x0", nil},
		{"", nil},
		{"zz", nil},
		{"0X1F8", []int{3, 4, 5, 6, 7, 8}},
		{"0x1000000000000000", []int{60}},
		{"0xFFFFFFFFFFFFFFE", func() []int {
			var l []int
			for i := 1; i <= 59; i++ {
				l = append(l, i)
			}
			return l
		}()},
	}
	for _, c := range cases {
		if got := gwn.PortMap(c.in); !reflect.DeepEqual(got, c.want) {
			t.Errorf("PortMap(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func newClient(t *testing.T, fake *gwntest.Server, pass string) *gwn.Client {
	t.Helper()
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	return &gwn.Client{Transport: http.DefaultTransport, Base: srv.URL, Username: gwntest.User, Password: pass}
}

func port(t *testing.T, st *gwn.State, id int) gwn.Port {
	t.Helper()
	for _, p := range st.Ports {
		if p.ID == id {
			return p
		}
	}
	t.Fatalf("port %d missing", id)
	return gwn.Port{}
}

func vlan(t *testing.T, st *gwn.State, id int) gwn.VLAN {
	t.Helper()
	for _, v := range st.VLANs {
		if v.ID == id {
			return v
		}
	}
	t.Fatalf("VLAN %d missing", id)
	return gwn.VLAN{}
}

func TestLoad(t *testing.T) {
	fake := gwntest.New()
	c := newClient(t, fake, gwntest.Password)
	st, err := c.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if st.Model != "GWN7721" || st.Firmware != "1.0.7.128" || st.Uptime != 27200 || st.ObjectID != "1.3.6.1.4.1.42397" {
		t.Errorf("system: %+v", st)
	}
	if len(st.Ports) != 10 {
		t.Fatalf("ports: %d", len(st.Ports))
	}
	p1 := port(t, st, 1)
	if p1.Label != "2.5GE1" || p1.Descr != "office" || !p1.Link || !p1.Enabled || p1.Speed != 2.5e9 || !p1.FullDuplex ||
		p1.PVID != 1 || p1.RxBad != 7 || p1.TxBad != 2 || !p1.HasStats {
		t.Errorf("port 1: %+v", p1)
	}
	if p2 := port(t, st, 2); p2.Speed != 1e9 || p2.PVID != 100 || p2.LAG != 0 {
		t.Errorf("port 2: %+v", p2)
	}
	if p3, p4 := port(t, st, 3), port(t, st, 4); p3.LAG != 1 || p4.LAG != 1 || p4.Link || p4.Speed != 0 || p4.FullDuplex {
		t.Errorf("LAG ports: %+v %+v", p3, p4)
	}
	if p5 := port(t, st, 5); p5.Enabled || p5.Link {
		t.Errorf("port 5: %+v", p5)
	}
	if p9 := port(t, st, 9); p9.Label != "SFP+9" || p9.Speed != 10e9 || p9.TxBad != 11 {
		t.Errorf("port 9: %+v", p9)
	}
	if p10 := port(t, st, 10); p10.Label != "SFP+10" || p10.Link {
		t.Errorf("port 10: %+v", p10)
	}
	if !st.VLANs802 || !st.HasVLANs || len(st.VLANs) != 2 {
		t.Fatalf("VLANs: %+v", st.VLANs)
	}
	if v := vlan(t, st, 1); v.Name != "default" || len(v.Tagged) != 0 || !reflect.DeepEqual(v.Untagged, []int{1, 3, 4, 5, 6, 7, 8, 9, 10}) {
		t.Errorf("VLAN 1: %+v", v)
	}
	if v := vlan(t, st, 100); v.Name != "office" || !reflect.DeepEqual(v.Tagged, []int{3, 9}) || !reflect.DeepEqual(v.Untagged, []int{2}) {
		t.Errorf("VLAN 100: %+v", v)
	}
	if len(st.Neighbors) != 1 || st.Neighbors[0].LocalPort != 9 || st.Neighbors[0].SysName != "core-1" || st.Neighbors[0].PortID != "1/47" {
		t.Errorf("neighbours: %+v", st.Neighbors)
	}
	if len(st.Notes) != 0 {
		t.Errorf("notes: %v", st.Notes)
	}
	if w := fake.Writes(); len(w) != 0 {
		t.Errorf("client wrote: %+v", w)
	}
}

func TestVLANPaging(t *testing.T) {
	fake := gwntest.New()
	fake.PageSize = 1
	st, err := newClient(t, fake, gwntest.Password).Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(st.VLANs) != 2 || st.VLANs[0].ID != 1 || st.VLANs[1].ID != 100 {
		t.Errorf("VLANs: %+v", st.VLANs)
	}
}

func TestSessionPerLoad(t *testing.T) {
	fake := gwntest.New()
	c := newClient(t, fake, gwntest.Password)
	for i := 1; i <= 2; i++ {
		if _, err := c.Load(context.Background()); err != nil {
			t.Fatal(err)
		}
		if fake.Logins() != i || fake.Logouts() != i || fake.Sessions() != 0 {
			t.Fatalf("load %d: logins %d, logouts %d, open sessions %d", i, fake.Logins(), fake.Logouts(), fake.Sessions())
		}
	}
}

func TestLoggedInElsewhere(t *testing.T) {
	fake := gwntest.New()
	fake.LoginElsewhere()
	c := newClient(t, fake, gwntest.Password)
	_, err := c.Load(context.Background())
	if !errors.Is(err, gwn.ErrBusy) || errors.Is(err, gwn.ErrLogin) {
		t.Fatalf("err = %v, want ErrBusy", err)
	}
	fake.ExpireSessions() // the other user logged out: the next poll works
	if _, err := c.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestWrongPassword(t *testing.T) {
	fake := gwntest.New()
	c := newClient(t, fake, "wrong")
	for i := 0; i < 3; i++ {
		_, err := c.Load(context.Background())
		if !errors.Is(err, gwn.ErrLogin) {
			t.Fatalf("load %d: err = %v, want ErrLogin", i, err)
		}
	}
	if fake.Logins() != 1 {
		t.Errorf("logins: %d, want 1 (no retry with a rejected login)", fake.Logins())
	}
}

func TestLogout(t *testing.T) {
	fake := gwntest.New()
	c := newClient(t, fake, gwntest.Password)
	ctx := context.Background()
	c.Logout(ctx) // no session: sends nothing
	if fake.Logouts() != 0 {
		t.Fatalf("logout without session sent")
	}
	if _, err := c.Load(ctx); err != nil {
		t.Fatal(err)
	}
	c.Logout(ctx) // Load has logged out already
	if fake.Logouts() != 1 {
		t.Errorf("logouts: %d", fake.Logouts())
	}
	if _, err := c.Load(ctx); err != nil {
		t.Fatal(err)
	}
	if fake.Logins() != 2 {
		t.Errorf("logins: %d, want 2", fake.Logins())
	}
	if w := fake.Writes(); len(w) != 0 {
		t.Errorf("client wrote: %+v", w)
	}
}

func TestPortBasedVLANs(t *testing.T) {
	fake := gwntest.New()
	fake.SetPortBased(true)
	st, err := newClient(t, fake, gwntest.Password).Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if st.VLANs802 || st.HasVLANs || len(st.VLANs) != 0 {
		t.Errorf("VLANs802=%v HasVLANs=%v VLANs=%v", st.VLANs802, st.HasVLANs, st.VLANs)
	}
	if len(st.Ports) != 10 {
		t.Errorf("ports: %d", len(st.Ports))
	}
}
