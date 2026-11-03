package edmsrc_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"testing/fstest"

	"github.com/lan-dot-party/ers-proxy/internal/edmlogin"
	"github.com/lan-dot-party/ers-proxy/internal/mockswitch"
	"github.com/lan-dot-party/ers-proxy/internal/monitor"
	"github.com/lan-dot-party/ers-proxy/internal/monitor/drivers"
	"github.com/lan-dot-party/ers-proxy/internal/monitor/edmsrc"
)

var loginPage = fstest.MapFS{
	"login.html": {Data: []byte(`<form name="submitForm"></form>`)},
	"index.html": {Data: []byte(`EDM`)},
}

func TestParseArray(t *testing.T) {
	rows, err := edmsrc.ParseArray(`[["1","Port \"A\""],['2','it\'s'], [3, "x,y"]]`)
	if err != nil {
		t.Fatal(err)
	}
	want := [][]string{{"1", `Port "A"`}, {"2", "it's"}, {"3", "x,y"}}
	if !reflect.DeepEqual(rows, want) {
		t.Fatalf("rows %q", rows)
	}
	if rows, err := edmsrc.ParseArray("[]"); err != nil || len(rows) != 0 {
		t.Fatalf("empty: %v %v", rows, err)
	}
	if _, err := edmsrc.ParseArray(`[["unterminated]]`); err == nil {
		t.Fatal("broken answer accepted")
	}
}

func TestERSOverEDM(t *testing.T) {
	f, err := os.Open(filepath.Join("..", "..", "..", "testdata", "snmpsim", "ers5600.snmprec"))
	if err != nil {
		t.Fatal(err)
	}
	data, err := mockswitch.LoadSNMPRec(f)
	f.Close()
	if err != nil {
		t.Fatal(err)
	}
	mock := mockswitch.New(loginPage, mockswitch.Options{Data: data})
	srv := httptest.NewServer(mock)
	defer srv.Close()

	s := edmsrc.NewSession(&http.Transport{}, srv.URL, "RW", "secure")
	r := edmsrc.Reader{S: s}
	ctx := context.Background()
	sys, err := monitor.ReadSystem(ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	d, err := monitor.NewRegistry(drivers.All()...).Detect(sys)
	if err != nil || d.Name() != "ers" {
		t.Fatalf("detect: %v %v", d, err)
	}
	snap, err := d.Collect(ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Units) != 2 || snap.Units[1].Model != "5650TD" {
		t.Fatalf("units %+v", snap.Units)
	}
	p := snap.Port(2)
	if p == nil || p.Unit != 1 || p.Number != 2 || !reflect.DeepEqual(p.Untagged, []int{1}) ||
		!reflect.DeepEqual(p.Tagged, []int{20}) || p.Mode != "hybrid" || p.Speed != 1_000_000_000 || p.Duplex != "full" {
		t.Fatalf("port 1/2: %+v", p)
	}
	if p := snap.Port(129); p == nil || p.Unit != 2 || p.Number != 1 {
		t.Fatalf("unit 2 port 1: %+v", p)
	}
	if p := snap.Port(3); p.LAGName != "to-core" {
		t.Fatalf("MLT: %+v", p)
	}
	// LLDP index objects are not-accessible; the reader must use "indexNA".
	if p := snap.Port(1); p.Neighbor == nil || p.Neighbor.SysName != "core-sw" || !p.Neighbor.Bridge {
		t.Fatalf("LLDP neighbour on 1/1: %+v", p.Neighbor)
	}
	if snap.System.Uptime.Hours() != 24 {
		t.Fatalf("uptime %v", snap.System.Uptime)
	}

	// The switch forgets the web session: the reader logs in again.
	mock.ResetSessions()
	if _, err := r.Scalars(ctx, "sysName"); err != nil {
		t.Fatalf("no re-login after the session expired: %v", err)
	}
	if v, _ := r.Scalars(ctx, "sysName"); v["sysName"].String() != "ers-stack" {
		t.Fatalf("sysName %+v", v)
	}
}

func TestRejectedLoginIsNotRepeated(t *testing.T) {
	// A wrong stored password must not be tried on every poll: the switch
	// locks the account after a few failures.
	mock := mockswitch.New(loginPage, mockswitch.Options{Password: "right-pw"})
	srv := httptest.NewServer(mock)
	defer srv.Close()
	r := edmsrc.Reader{S: edmsrc.NewSession(&http.Transport{}, srv.URL, "RW", "wrong-pw")}
	for i := 0; i < 3; i++ {
		if _, err := r.Scalars(context.Background(), "sysName"); !errors.Is(err, edmlogin.ErrRejected) {
			t.Fatalf("poll %d: %v", i, err)
		}
	}
	if n := mock.Requests["/"]; n != 2 { // one GET and one POST of a single login attempt
		t.Fatalf("login requests: %d", n)
	}
}
