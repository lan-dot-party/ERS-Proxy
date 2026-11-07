package edmsession_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/lan-dot-party/ers-proxy/internal/edmlogin"
	"github.com/lan-dot-party/ers-proxy/internal/edmsession"
	"github.com/lan-dot-party/ers-proxy/internal/mockswitch"
)

var loginPage = fstest.MapFS{
	"login.html": {Data: []byte(`<form name="submitForm"></form>`)},
	"index.html": {Data: []byte(`EDM`)},
}

var readForm = url.Values{"command": {"getVariables"}, "varNames": {"sysName"}}

func TestPostLogsInOnDemandAndReusesSession(t *testing.T) {
	mock := mockswitch.New(loginPage, mockswitch.Options{})
	srv := httptest.NewServer(mock)
	defer srv.Close()
	s := edmsession.New(&http.Transport{}, srv.URL+"/", "RW", "secure")
	var first int
	for i := 0; i < 2; i++ {
		if _, err := s.Post(context.Background(), "/jsp/getFormMibs.jsp", readForm); err != nil {
			t.Fatalf("post %d: %v", i, err)
		}
		if i == 0 {
			first = mock.Requests["/"]
		}
	}
	if first == 0 || mock.Requests["/"] != first { // logged in once, not per request
		t.Fatalf("login requests: %d after the first post, %d after the second", first, mock.Requests["/"])
	}
}

func TestRejectedLoginIsNotRepeated(t *testing.T) {
	mock := mockswitch.New(loginPage, mockswitch.Options{Password: "right-pw"})
	srv := httptest.NewServer(mock)
	defer srv.Close()
	s := edmsession.New(&http.Transport{}, srv.URL, "RW", "wrong-pw")
	for i := 0; i < 3; i++ {
		if _, err := s.Post(context.Background(), "/jsp/getFormMibs.jsp", readForm); !errors.Is(err, edmlogin.ErrRejected) {
			t.Fatalf("post %d: %v", i, err)
		}
	}
	if n := mock.Requests["/"]; n != 2 {
		t.Fatalf("login requests: %d", n)
	}
}

// expiring answers the login page to the first data request, like a switch
// whose session has timed out, and then serves data.
type expiring struct {
	logins, posts, logouts int
	always                 bool
}

func (e *expiring) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/logout.html":
		e.logouts++
	case r.URL.Path == "/jsp/getFormMibs.jsp":
		e.posts++
		if e.always || e.posts == 1 {
			w.Write([]byte(`<form name="submitForm">`))
			return
		}
		w.Write([]byte("sysName:x\r\n"))
	default:
		if r.Method == http.MethodPost {
			e.logins++
		}
		http.SetCookie(w, &http.Cookie{Name: "auth", Value: "tok"})
		w.Write([]byte(`<form name="submitForm"></form>`))
	}
}

func TestReloginWhenSessionExpired(t *testing.T) {
	e := &expiring{}
	srv := httptest.NewServer(e)
	defer srv.Close()
	s := edmsession.New(&http.Transport{}, srv.URL, "RW", "secure")
	text, err := s.Post(context.Background(), "/jsp/getFormMibs.jsp", readForm)
	if err != nil || !strings.Contains(text, "sysName:x") {
		t.Fatalf("got %q, %v", text, err)
	}
	if e.logins%2 != 0 || e.logins < 4 || e.posts != 2 { // two logins, each with two login requests
		t.Fatalf("logins %d posts %d, want two logins and 2 posts", e.logins, e.posts)
	}
}

func TestErrSessionWhenLoginPageKeepsComing(t *testing.T) {
	e := &expiring{always: true}
	srv := httptest.NewServer(e)
	defer srv.Close()
	s := edmsession.New(&http.Transport{}, srv.URL, "RW", "secure")
	if _, err := s.Post(context.Background(), "/jsp/getFormMibs.jsp", readForm); !errors.Is(err, edmsession.ErrSession) {
		t.Fatalf("err %v", err)
	}
}

func TestLogout(t *testing.T) {
	e := &expiring{}
	e.posts = 1 // skip the expiry
	srv := httptest.NewServer(e)
	defer srv.Close()
	ctx := context.Background()
	s := edmsession.New(&http.Transport{}, srv.URL, "RW", "secure")
	s.Logout(ctx) // not logged in: nothing is sent
	if e.logouts != 0 || e.logins != 0 {
		t.Fatalf("fresh session sent requests: %+v", e)
	}
	if _, err := s.Post(ctx, "/jsp/getFormMibs.jsp", readForm); err != nil {
		t.Fatal(err)
	}
	s.Logout(ctx)
	s.Logout(ctx)
	if e.logouts != 1 {
		t.Fatalf("logouts %d, want 1", e.logouts)
	}
}
