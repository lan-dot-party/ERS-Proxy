package edmsrc_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
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

// fakeVOSS is a minimal VOSS EDM: VOSS login flow, the two jsp endpoints and
// /logout.html. Tables answer like a VSP: an object name in unknownNames
// yields Oid_Conversion_Fail and collapses the answer to one row.
type fakeVOSS struct {
	mu           sync.Mutex
	logins       int
	logouts      int
	logoutCookie string
	tableReqs    []string // varNames of getTableVariables requests
	scalarReqs   []string
	unknownNames map[string]bool
	table        map[string][]string // column -> value per row
	rowIdx       []string
	scalars      map[string]string
}

func (f *fakeVOSS) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case r.URL.Path == "/" && r.Method == http.MethodGet:
		http.SetCookie(w, &http.Cookie{Name: "auth", Value: "nonce1"})
		_, _ = w.Write([]byte(`<form method="POST" name="submitForm" onSubmit="encode_vrf()">`))
	case r.URL.Path == "/" && r.Method == http.MethodPost:
		f.logins++
		_, _ = w.Write([]byte("EDM"))
	case r.URL.Path == "/logout.html":
		f.logouts++
		if c, err := r.Cookie("auth"); err == nil {
			f.logoutCookie = c.Value
		}
	case r.URL.Path == "/jsp/getFormMibs.jsp":
		_ = r.ParseForm()
		f.scalarReqs = append(f.scalarReqs, r.Form.Get("varNames"))
		var b strings.Builder
		for _, n := range strings.Split(r.Form.Get("varNames"), ",") {
			if v, ok := f.scalars[n]; ok {
				b.WriteString(n + ".0:" + v + "\n")
			}
		}
		_, _ = w.Write([]byte(b.String()))
	case r.URL.Path == "/jsp/getTableMibs.jsp":
		_ = r.ParseForm()
		vn := r.Form.Get("varNames")
		f.tableReqs = append(f.tableReqs, vn)
		names := strings.Split(vn, ",")[1:] // first is ifIndex
		bad := false
		for _, n := range names {
			bad = bad || f.unknownNames[n]
		}
		rowsOut := []string{}
		for i, idx := range f.rowIdx {
			cells := []string{"'" + idx + "'"}
			for _, n := range names {
				if f.unknownNames[n] {
					cells = append(cells, "'Oid_Conversion_Fail'")
				} else {
					cells = append(cells, "'"+f.table[n][i]+"'")
				}
			}
			rowsOut = append(rowsOut, "["+strings.Join(cells, ",")+"]")
			if bad {
				break
			}
		}
		_, _ = w.Write([]byte("[" + strings.Join(rowsOut, ",") + "]"))
	default:
		http.NotFound(w, r)
	}
}

func newFakeVOSS() *fakeVOSS {
	return &fakeVOSS{
		unknownNames: map[string]bool{"ifName": true},
		rowIdx:       []string{"192", "193", "194"},
		table: map[string][]string{
			"ifHCInOctets": {"0", "5", "7"},
			"ifAlias":      {"a", "b", "c"},
			"ifName":       {"x", "y", "z"},
		},
		scalars: map[string]string{"sysName": "Oid_Conversion_Fail", "sysDescr": "VSP-4850GTS (7.1.10.0)"},
	}
}

func TestColumnsSkipsUnknownNames(t *testing.T) {
	f := newFakeVOSS()
	srv := httptest.NewServer(f)
	defer srv.Close()
	r := edmsrc.Reader{S: edmsrc.NewSession(&http.Transport{}, srv.URL, "RW", "secure")}
	ctx := context.Background()

	tab, err := r.Columns(ctx, "ifName", "ifHCInOctets", "ifAlias")
	if err != nil {
		t.Fatal(err)
	}
	if len(tab) != 3 {
		t.Fatalf("rows: %v", tab)
	}
	for idx, row := range tab {
		if _, ok := row["ifName"]; ok {
			t.Errorf("row %s has ifName", idx)
		}
		if _, ok := row["ifHCInOctets"]; !ok {
			t.Errorf("row %s lacks ifHCInOctets", idx)
		}
		if _, ok := row["ifAlias"]; !ok {
			t.Errorf("row %s lacks ifAlias", idx)
		}
	}
	if v, _ := tab["193"]["ifHCInOctets"].AsUint(); v != 5 {
		t.Errorf("ifHCInOctets 193 = %d", v)
	}
	f.mu.Lock()
	reqs := append([]string{}, f.tableReqs...)
	f.mu.Unlock()
	if len(reqs) != 2 || !strings.Contains(reqs[0], "ifName") || strings.Contains(reqs[1], "ifName") {
		t.Fatalf("requests %q", reqs)
	}

	if _, err := r.Columns(ctx, "ifName", "ifHCInOctets", "ifAlias"); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.tableReqs) != 3 || strings.Contains(f.tableReqs[2], "ifName") {
		t.Fatalf("second call must not ask for ifName: %q", f.tableReqs)
	}
}

func TestColumnsAllUnknown(t *testing.T) {
	f := newFakeVOSS()
	f.unknownNames["ifHCInOctets"] = true
	f.unknownNames["ifAlias"] = true
	srv := httptest.NewServer(f)
	defer srv.Close()
	r := edmsrc.Reader{S: edmsrc.NewSession(&http.Transport{}, srv.URL, "RW", "secure")}
	tab, err := r.Columns(context.Background(), "ifName", "ifHCInOctets", "ifAlias")
	if err != nil || len(tab) != 0 {
		t.Fatalf("table %v, err %v", tab, err)
	}
	f.mu.Lock()
	n := len(f.tableReqs)
	f.mu.Unlock()
	if n != 1 {
		t.Fatalf("%d requests, want 1", n)
	}
	// Nothing is requested at all any more.
	if tab, err := r.Columns(context.Background(), "ifName"); err != nil || len(tab) != 0 {
		t.Fatalf("second call: %v %v", tab, err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.tableReqs) != 1 {
		t.Fatalf("requests %q", f.tableReqs)
	}
}

func TestScalarsSkipConversionFail(t *testing.T) {
	f := newFakeVOSS()
	srv := httptest.NewServer(f)
	defer srv.Close()
	r := edmsrc.Reader{S: edmsrc.NewSession(&http.Transport{}, srv.URL, "RW", "secure")}
	got, err := r.Scalars(context.Background(), "sysName", "sysDescr")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := got["sysName"]; ok {
		t.Errorf("sysName must be skipped: %v", got)
	}
	if got["sysDescr"].String() != "VSP-4850GTS (7.1.10.0)" {
		t.Errorf("sysDescr: %v", got)
	}
}

func TestLogout(t *testing.T) {
	f := newFakeVOSS()
	srv := httptest.NewServer(f)
	defer srv.Close()
	ctx := context.Background()
	s := edmsrc.NewSession(&http.Transport{}, srv.URL, "RW", "secure")
	s.Logout(ctx) // not logged in: nothing is sent
	f.mu.Lock()
	if f.logouts != 0 || f.logins != 0 {
		t.Fatalf("fresh session sent requests: logins %d logouts %d", f.logins, f.logouts)
	}
	f.mu.Unlock()

	r := edmsrc.Reader{S: s}
	if _, err := r.Scalars(ctx, "sysDescr"); err != nil {
		t.Fatal(err)
	}
	s.Logout(ctx)
	f.mu.Lock()
	if f.logouts != 1 || f.logoutCookie != "nonce1" || f.logins != 1 {
		t.Fatalf("logins %d logouts %d cookie %q", f.logins, f.logouts, f.logoutCookie)
	}
	f.mu.Unlock()
	s.Logout(ctx) // already logged out
	if _, err := r.Scalars(ctx, "sysDescr"); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.logins != 2 || f.logouts != 1 {
		t.Fatalf("logins %d logouts %d, want 2 and 1", f.logins, f.logouts)
	}
}
