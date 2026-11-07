package edmwrite_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/lan-dot-party/ers-proxy/internal/edmsession"
	"github.com/lan-dot-party/ers-proxy/internal/mockswitch"
	"github.com/lan-dot-party/ers-proxy/internal/monitor"
	"github.com/lan-dot-party/ers-proxy/internal/monitor/drivers"
	"github.com/lan-dot-party/ers-proxy/internal/monitor/edmsrc"
	"github.com/lan-dot-party/ers-proxy/internal/switchcfg"
	"github.com/lan-dot-party/ers-proxy/internal/switchcfg/edmwrite"
	"github.com/lan-dot-party/ers-proxy/internal/switchcfg/rcplan"
)

var loginPage = fstest.MapFS{
	"login.html": {Data: []byte(`<form name="submitForm"></form>`)},
	"index.html": {Data: []byte(`EDM`)},
}

func TestEncode(t *testing.T) {
	as := []switchcfg.Assignment{
		{Name: "rcVlanPortMembers", Index: "10", Value: switchcfg.Octets([]byte{0x00, 0x40, 0x80})},
		{Name: "rcVlanPortMembers", Index: "11", Value: switchcfg.Octets(nil)},
		{Name: "rcVlanName", Index: "10", Value: switchcfg.Text("Office 1")},
		{Name: "ifAlias", Index: "5", Value: switchcfg.Octets([]byte{0xff, 'a'})},
		{Name: "rcVlanPortDefaultVlanId", Index: "5", Value: switchcfg.Int(20)},
		{Name: "someCounter", Index: "", Value: monitor.Value{Kind: monitor.KindUint, Uint: 7}},
		{Name: "x", Index: "1", Value: monitor.Value{Kind: monitor.KindOID, OID: "1.3.6.1"}},
		{Name: "ifAlias", Index: "6", Value: switchcfg.Text("")},
	}
	f, err := edmwrite.Encode(as)
	if err != nil {
		t.Fatal(err)
	}
	if f.Get("command") != "setVariables" || f.Get("varCount") != "8" {
		t.Fatalf("form %v", f)
	}
	wantNames := "rcVlanPortMembers.10\trcVlanPortMembers.11\trcVlanName.10\tifAlias.5\trcVlanPortDefaultVlanId.5\tsomeCounter.0\tx.1\tifAlias.6"
	wantValues := "0x00:40:80\t0x00:00\tOffice 1\t0xff:61\t20\t7\t1.3.6.1\t0x00:"
	if f.Get("varNames") != wantNames || f.Get("varValues") != wantValues {
		t.Fatalf("names %q\nvalues %q", f.Get("varNames"), f.Get("varValues"))
	}
}

func TestEncodeRejects(t *testing.T) {
	for name, a := range map[string]switchcfg.Assignment{
		"tab":     {Name: "ifAlias", Index: "1", Value: switchcfg.Text("a\tb")},
		"cr":      {Name: "ifAlias", Index: "1", Value: switchcfg.Text("a\rb")},
		"lf":      {Name: "ifAlias", Index: "1", Value: switchcfg.Text("a\nb")},
		"none":    {Name: "ifAlias", Index: "1", Value: switchcfg.Absent},
		"badname": {Name: "if\tAlias", Index: "1", Value: switchcfg.Int(1)},
	} {
		if _, err := edmwrite.Encode([]switchcfg.Assignment{a}); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := edmwrite.Encode(nil); err == nil {
		t.Error("empty step accepted")
	}
}

func TestParseAnswer(t *testing.T) {
	for _, ok := range []string{"", "snmpErrorStatus:0\r\n", "OK", "snmpErrorStatus:+0"} {
		if err := edmwrite.ParseAnswer(ok); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	err := edmwrite.ParseAnswer("snmpErrorStatus:10\r\n\r\n2:wrongValue")
	var se *edmwrite.SetError
	if !errors.As(err, &se) || se.Status != 10 || se.Index != 2 || se.Message != "wrongValue" || se.OWD {
		t.Fatalf("err %#v", err)
	}
	if !strings.Contains(err.Error(), "10") || !strings.Contains(err.Error(), "wrongValue") {
		t.Fatalf("text %q", err)
	}
	// The answers of a VSP 8404 with VOSS 8.10.1.
	if err := edmwrite.ParseAnswer("ifAlias:ersproxy-test\r\n"); err != nil {
		t.Errorf("VOSS success answer: %v", err)
	}
	err = edmwrite.ParseAnswer("snmpErrorStatus:10\r\nsnmpErrorIndex:1\r\n")
	if !errors.As(err, &se) || se.Status != 10 || se.Index != 1 || !strings.Contains(err.Error(), "wrongValue") {
		t.Fatalf("VOSS error answer: %#v", err)
	}
	if err := edmwrite.ParseAnswer("snmpErrorStatus:-3"); !errors.As(err, &se) || se.Status != -3 || se.Index != 0 {
		t.Fatalf("err %#v", err)
	}
	if err := edmwrite.ParseAnswer("owdErrorStatus:5\r\n"); !errors.As(err, &se) || !se.OWD || se.Status != 5 {
		t.Fatalf("err %#v", err)
	}
}

func TestBinaryObjects(t *testing.T) {
	if !edmwrite.Binary["rcVlanPortMembers"] || !edmwrite.Binary["rcMltPortMembers"] {
		t.Fatal("PortSet objects must be binary")
	}
}

// vossMock starts a mock over the VOSS test data.
func vossMock(t *testing.T, opts mockswitch.Options) (*mockswitch.Mock, *mockswitch.SNMPData, string) {
	t.Helper()
	f, err := os.Open(filepath.Join("..", "..", "..", "testdata", "snmpsim", "voss.snmprec"))
	if err != nil {
		t.Fatal(err)
	}
	data, err := mockswitch.LoadSNMPRec(f)
	f.Close()
	if err != nil {
		t.Fatal(err)
	}
	// Objects the planner reads and the recording lacks.
	for vid, stg := range map[string]string{"1": "1", "10": "1", "20": "1"} {
		if err := data.Put("rcVlanType", vid, "1"); err != nil {
			t.Fatal(err)
		}
		if err := data.Put("rcVlanStgId", vid, stg); err != nil {
			t.Fatal(err)
		}
	}
	opts.Data = data
	mock := mockswitch.New(loginPage, opts)
	srv := httptest.NewServer(mock)
	t.Cleanup(srv.Close)
	return mock, data, srv.URL
}

func TestSetAgainstMock(t *testing.T) {
	mock, _, base := vossMock(t, mockswitch.Options{})
	s := edmsession.New(&http.Transport{}, base, "RW", "secure")
	w := edmwrite.Writer{S: s}
	r := edmsrc.Reader{S: &edmsrc.Session{Session: s}}
	ctx := context.Background()

	err := w.Set(ctx, []switchcfg.Assignment{
		{Name: "rcVlanName", Index: "10", Value: switchcfg.Text("Office 2")},
		{Name: "rcVlanPortMembers", Index: "20", Value: switchcfg.Octets([]byte{0x80})},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(mock.Sets) != 1 || mock.Sets[0] != "rcVlanName.10\trcVlanPortMembers.20" || mock.SetValues[0] != "Office 2\t0x80" {
		t.Fatalf("recorded %q %q", mock.Sets, mock.SetValues)
	}
	ms, err := switchcfg.Verify(ctx, r, []switchcfg.Assignment{
		{Name: "rcVlanName", Index: "10", Value: switchcfg.Text("Office 2")},
		{Name: "rcVlanPortMembers", Index: "20", Value: switchcfg.Octets([]byte{0x80})},
	})
	if err != nil || len(ms) != 0 {
		t.Fatalf("verify: %v %v", ms, err)
	}

	// A rejected step: nothing is applied, the error carries the status.
	mock.SetError = func(names []string) string {
		if len(names) == 1 && names[0] == "rcVlanName.10" {
			return "snmpErrorStatus:10\r\n\r\n1:wrongValue"
		}
		return ""
	}
	err = w.Set(ctx, []switchcfg.Assignment{{Name: "rcVlanName", Index: "10", Value: switchcfg.Text("nope")}})
	var se *edmwrite.SetError
	if !errors.As(err, &se) || se.Status != 10 || se.Index != 1 || se.Message != "wrongValue" {
		t.Fatalf("err %#v", err)
	}
	ms, _ = switchcfg.Verify(ctx, r, []switchcfg.Assignment{{Name: "rcVlanName", Index: "10", Value: switchcfg.Text("Office 2")}})
	if len(ms) != 0 {
		t.Fatalf("rejected write changed the switch: %v", ms)
	}

	// The mock itself rejects a value of the wrong type, and unknown objects.
	mock.SetError = nil
	err = w.Set(ctx, []switchcfg.Assignment{{Name: "rcVlanPortDefaultVlanId", Index: "192", Value: switchcfg.Text("abc")}})
	if !errors.As(err, &se) || se.Status != 7 {
		t.Fatalf("err %#v", err)
	}
	err = w.Set(ctx, []switchcfg.Assignment{{Name: "noSuchObject", Index: "1", Value: switchcfg.Int(1)}})
	if !errors.As(err, &se) || se.Status != 2 {
		t.Fatalf("err %#v", err)
	}
}

func TestSetAfterSessionExpired(t *testing.T) {
	mock, _, base := vossMock(t, mockswitch.Options{})
	s := edmsession.New(&http.Transport{}, base, "RW", "secure")
	w := edmwrite.Writer{S: s}
	ctx := context.Background()
	step := []switchcfg.Assignment{{Name: "rcVlanName", Index: "10", Value: switchcfg.Text("A")}}
	if err := w.Set(ctx, step); err != nil {
		t.Fatal(err)
	}
	mock.ResetSessions() // the switch forgot the login
	step[0].Value = switchcfg.Text("B")
	if err := w.Set(ctx, step); err != nil {
		t.Fatalf("after expiry: %v", err)
	}
	// Both writes arrived once: the first POST of the second Set got a
	// "Not Authorized" (nothing executed), then it was sent again.
	if len(mock.Sets) != 2 || mock.SetValues[1] != "B" {
		t.Fatalf("recorded %q %q", mock.Sets, mock.SetValues)
	}
}

func TestMaxSessions(t *testing.T) {
	mock, _, base := vossMock(t, mockswitch.Options{MaxSessions: 1})
	ctx := context.Background()
	step := []switchcfg.Assignment{{Name: "rcVlanName", Index: "10", Value: switchcfg.Text("A")}}
	s1 := edmsession.New(&http.Transport{}, base, "RW", "secure")
	if err := (edmwrite.Writer{S: s1}).Set(ctx, step); err != nil {
		t.Fatal(err)
	}
	s2 := edmsession.New(&http.Transport{}, base, "RW", "secure")
	if err := (edmwrite.Writer{S: s2}).Set(ctx, step); err == nil {
		t.Fatal("second session accepted")
	}
	if len(mock.Sets) != 1 {
		t.Fatalf("sets %q", mock.Sets)
	}
	s1.Logout(ctx) // frees the slot
	// s2 stays refused (edmsession does not retry a rejected login); a new
	// session gets in.
	s3 := edmsession.New(&http.Transport{}, base, "RW", "secure")
	if err := (edmwrite.Writer{S: s3}).Set(ctx, step); err != nil {
		t.Fatalf("after logout: %v", err)
	}
}

func TestPlanApplyAndRevertOverEDM(t *testing.T) {
	_, data, base := vossMock(t, mockswitch.Options{})
	_ = data
	ctx := context.Background()
	es := edmsrc.NewSession(&http.Transport{}, base, "RW", "secure")
	r := edmsrc.Reader{S: es}
	w := edmwrite.Writer{S: es.EDM()}

	sys, err := monitor.ReadSystem(ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	d, err := monitor.NewRegistry(drivers.All()...).Detect(sys)
	if err != nil || d.Name() != "voss" {
		t.Fatalf("detect: %v %v", d, err)
	}
	snap, err := d.Collect(ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	var move *monitor.Port
	for _, p := range snap.Ports {
		if p.Physical && reflect.DeepEqual(p.Untagged, []int{10}) && len(p.Tagged) == 0 && p.Neighbor == nil {
			move = p
			break
		}
	}
	if move == nil {
		t.Fatal("no access port in VLAN 10 in the test data")
	}

	before := captureState(t, ctx, r)
	change := switchcfg.Change{
		VLANs: []switchcfg.VLANChange{{Op: switchcfg.VLANCreate, ID: 30, Name: "Cameras"}},
		Ports: []switchcfg.PortChange{{IfIndex: move.IfIndex, VLANs: &switchcfg.PortVLANs{Untagged: 20}}},
	}
	plan, err := rcplan.VOSS().Plan(ctx, r, snap, change)
	if err != nil {
		t.Fatal(err)
	}
	for _, st := range plan.Steps {
		if err := w.Set(ctx, st.Set); err != nil {
			t.Fatalf("%s: %v", st.Description, err)
		}
	}
	if ms, err := switchcfg.Verify(ctx, r, plan.Expected); err != nil || len(ms) != 0 {
		t.Fatalf("expected values differ: %v %v", ms, err)
	}
	rows, err := r.Columns(ctx, "rcVlanName", "rcVlanType", "rcVlanStgId")
	if err != nil {
		t.Fatal(err)
	}
	if rows["30"]["rcVlanName"].String() != "Cameras" {
		t.Fatalf("VLAN 30 missing: %v", rows["30"])
	}
	for _, st := range plan.Inverse {
		if err := w.Set(ctx, st.Set); err != nil {
			t.Fatalf("inverse %s: %v", st.Description, err)
		}
	}
	if ms, err := switchcfg.Verify(ctx, r, plan.Before); err != nil || len(ms) != 0 {
		t.Fatalf("restored values differ: %v %v", ms, err)
	}
	if after := captureState(t, ctx, r); !reflect.DeepEqual(before, after) {
		t.Fatalf("state after revert differs:\nbefore %v\nafter  %v", before, after)
	}
}

// captureState reads the VLAN and port settings, with trailing zero octets of
// PortSets removed.
func captureState(t *testing.T, ctx context.Context, r monitor.Reader) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, names := range [][]string{{"rcVlanName", "rcVlanPortMembers", "rcVlanType", "rcVlanStgId"}, {"rcVlanPortDefaultVlanId", "rcVlanPortPerformTagging"}} {
		tb, err := r.Columns(ctx, names...)
		if err != nil {
			t.Fatal(err)
		}
		for idx, row := range tb {
			for n, v := range row {
				s := v.String()
				if v.Kind == monitor.KindBytes && n == "rcVlanPortMembers" {
					s = strings.TrimRight(string(v.Bytes), "\x00")
				}
				out[n+"."+idx] = s
			}
		}
	}
	return out
}
