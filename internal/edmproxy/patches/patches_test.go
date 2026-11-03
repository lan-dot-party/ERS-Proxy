package patches

import (
	"strings"
	"testing"
)

// The fixtures are synthetic: they contain only the strings the patches look
// for, never vendor files (see CONTRIBUTING.md).

func TestExtHeaderMap(t *testing.T) {
	in := `for(k=0;k<l.length;k++){g=l[k].indexOf(":");if(g>=0){p[l[k].substring(0,g)]=l[k].substring(g+2)}}`
	out, res := Apply(in, "/d/tok/")
	if res.Applied["ext-header-map"] != 1 || !strings.Contains(out, "p[hn.toLowerCase()]=hv") {
		t.Fatalf("not applied: %s %+v", out, res)
	}
}

func TestAmbiguousSinglePatchIsSkipped(t *testing.T) {
	in := "var supportedBrowser = false;\nvar supportedBrowser = false;"
	out, res := Apply(in, "/d/tok/")
	if out != in || len(res.Ambiguous) != 1 || res.Ambiguous[0] != "browser-whitelist" {
		t.Fatalf("ambiguous patch applied: %+v", res)
	}
}

func TestLoginPage(t *testing.T) {
	in := `<html><head><title>x</title></head><body>
var supportedBrowser = false;
xmlhttp.open("POST","/",false);
if (xmlhttp.status==205){ if (xmlhttp.statusText.indexOf("Change Password Retry")>=0){} else if (xmlhttp.statusText.indexOf("Change Password")>=0){} }
window.location.href="/";
<a href="/">home</a></body></html>`
	out, res := Apply(in, "/d/tok/")
	for _, name := range []string{"browser-whitelist", "login-xhr-root", "login-redirect-root", "root-links"} {
		if res.Applied[name] != 1 {
			t.Errorf("%s not applied once: %+v", name, res.Applied)
		}
	}
	if res.Applied["login-status-text"] != 2 {
		t.Errorf("status text replaced %d times", res.Applied["login-status-text"])
	}
	for _, want := range []string{`xmlhttp.open("POST","/d/tok/",false)`, `window.location.href="/d/tok/"`, `href="/d/tok/"`,
		`ersStatusText(xmlhttp).indexOf("Change Password Retry")`} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q", want)
		}
	}
	if strings.Contains(out, `"/"`) {
		t.Error("absolute root path left in the page")
	}
}

func TestHelpAndCSSPaths(t *testing.T) {
	in := `var helpUrl=self.location.protocol+'//'+self.location.host+'/help/'+helpPath; a('/css/css_ie5win.css');b('/css/css_ns6up.css')`
	out, _ := Apply(in, "/d/tok/")
	if !strings.Contains(out, `self.location.host+'/d/tok/help/'`) || strings.Count(out, `'/d/tok/css/css_`) != 2 {
		t.Fatalf("paths not rewritten: %s", out)
	}
}

func TestInjectScript(t *testing.T) {
	cases := map[string]string{
		`<HTML><HEAD><title>a</title>`: `<HTML><HEAD><script src="/s.js"></script><title>a</title>`,
		`<html lang="en"><body>`:       `<html lang="en"><script src="/s.js"></script><body>`,
		`hello`:                        `<script src="/s.js"></script>hello`,
	}
	for in, want := range cases {
		if got := InjectScript(in, "/s.js"); got != want {
			t.Errorf("InjectScript(%q) = %q", in, got)
		}
	}
}

func TestPatchable(t *testing.T) {
	if !Patchable("application/x-javascript", "/x") || !Patchable("", "/ext/ext-all.js") || Patchable("image/gif", "/a.gif") {
		t.Fatal("Patchable")
	}
	if !IsHTML("", "/") || IsHTML("application/javascript", "/a.js") || !IsHTML("text/html; charset=utf-8", "/x.jsp") {
		t.Fatal("IsHTML")
	}
}
