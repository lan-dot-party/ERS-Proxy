// Package patches fixes EDM (the web interface of Avaya/Extreme ERS switches)
// for modern browsers and for being served below /d/{token}/.
//
// Patches match file content, not paths or firmware versions, so they apply
// to every ERS family that ships the affected code. Each patch replaces an
// exact string; single patches only apply when the string occurs exactly once
// in a file, so an unexpected file version is left alone instead of broken.
package patches

import (
	"sort"
	"strings"
)

// Base is replaced by the proxy path of the device ("/d/{token}/").
const Base = "{{BASE}}"

// Patch is one textual fix.
type Patch struct {
	// Name identifies the patch in reports.
	Name string
	// Why explains the problem it fixes.
	Why string
	// Old is the exact text to find; New replaces it (may contain Base).
	Old, New string
	// All replaces every occurrence instead of requiring exactly one.
	All bool
}

// List is the set of patches, applied in order.
var List = []Patch{
	{
		Name: "ext-header-map",
		Why: "Ext JS 2.2 builds the response header map case-sensitively; modern browsers return lower-case " +
			"names, so EDM's response.getResponseHeader.Server is undefined and every AJAX callback throws.",
		Old: `p[l[k].substring(0,g)]=l[k].substring(g+2)`,
		New: `var hn=l[k].substring(0,g),hv=l[k].substring(g+2).replace(/\r$/,"");p[hn]=hv;` +
			`p[hn.toLowerCase()]=hv;` +
			`p[hn.toLowerCase().replace(/(^|-)([a-z])/g,function(a,b,c){return b+c.toUpperCase()})]=hv`,
	},
	{
		Name: "browser-whitelist",
		Why:  "EDM only accepts IE 7/8 and Firefox 3 and shows an alert in every current browser.",
		Old:  `var supportedBrowser = false;`,
		New:  `var supportedBrowser = true;`,
	},
	{
		Name: "login-xhr-root",
		Why:  "The login check posts to the absolute path /, which is outside the proxy path.",
		Old:  `xmlhttp.open("POST","/",false);`,
		New:  `xmlhttp.open("POST","` + Base + `",false);`,
	},
	{
		Name: "login-redirect-root",
		Why:  "After a password change the login page navigates to /, outside the proxy path.",
		Old:  `window.location.href="/";`,
		New:  `window.location.href="` + Base + `";`,
	},
	{
		Name: "login-status-text",
		Why: "EDM reads the reason phrase of a 205 answer (\"Change Password\"). HTTP/2 has no reason phrases, " +
			"so the proxy sends it in X-ERSProxy-Status-Text and the shim provides ersStatusText().",
		Old: `xmlhttp.statusText`,
		New: `ersStatusText(xmlhttp)`,
		All: true,
	},
	{
		Name: "root-links",
		Why:  "Links to / leave the proxy path.",
		Old:  `href="/"`,
		New:  `href="` + Base + `"`,
		All:  true,
	},
	{
		Name: "help-path",
		Why:  "The help window is opened at the absolute path /help/.",
		Old:  `self.location.host+'/help/'`,
		New:  `self.location.host+'` + Base + `help/'`,
	},
	{
		Name: "legacy-css-paths",
		Why:  "Browser-specific style sheets are loaded from the absolute path /css/.",
		Old:  `'/css/css_`,
		New:  `'` + Base + `css/css_`,
		All:  true,
	},
}

// Result reports what happened to one file.
type Result struct {
	// Applied maps patch names to the number of replacements.
	Applied map[string]int
	// Ambiguous lists single patches whose text occurred more than once.
	Ambiguous []string
}

// Names returns the applied patch names, sorted.
func (r Result) Names() []string {
	out := make([]string, 0, len(r.Applied))
	for k := range r.Applied {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Apply patches body. base is the proxy path ending in "/".
func Apply(body, base string) (string, Result) {
	res := Result{Applied: map[string]int{}}
	for _, p := range List {
		n := strings.Count(body, p.Old)
		if n == 0 {
			continue
		}
		repl := strings.ReplaceAll(p.New, Base, base)
		if p.All {
			body = strings.ReplaceAll(body, p.Old, repl)
			res.Applied[p.Name] += n
			continue
		}
		if n > 1 {
			res.Ambiguous = append(res.Ambiguous, p.Name)
			continue
		}
		body = strings.Replace(body, p.Old, repl, 1)
		res.Applied[p.Name]++
	}
	return body, res
}

// Patchable reports whether a response should go through Apply: HTML and
// JavaScript (by content type or file extension).
func Patchable(contentType, path string) bool {
	ct := strings.ToLower(contentType)
	if strings.Contains(ct, "html") || strings.Contains(ct, "javascript") || strings.Contains(ct, "ecmascript") {
		return true
	}
	p := strings.ToLower(path)
	return p == "/" || strings.HasSuffix(p, ".html") || strings.HasSuffix(p, ".htm") || strings.HasSuffix(p, ".js")
}

// IsHTML reports whether a response is an HTML document (gets the shim).
func IsHTML(contentType, path string) bool {
	ct := strings.ToLower(contentType)
	if strings.Contains(ct, "html") {
		return true
	}
	if ct != "" && !strings.HasPrefix(ct, "text/plain") {
		return false
	}
	p := strings.ToLower(path)
	return p == "/" || strings.HasSuffix(p, ".html") || strings.HasSuffix(p, ".htm")
}

// InjectScript inserts a <script src> tag as the first element of <head> (or
// at the start of the document), so it runs before the page's own scripts.
func InjectScript(html, src string) string {
	tag := `<script src="` + src + `"></script>`
	lower := strings.ToLower(html)
	if i := strings.Index(lower, "<head"); i >= 0 {
		if j := strings.IndexByte(lower[i:], '>'); j >= 0 {
			pos := i + j + 1
			return html[:pos] + tag + html[pos:]
		}
	}
	if i := strings.Index(lower, "<html"); i >= 0 {
		if j := strings.IndexByte(lower[i:], '>'); j >= 0 {
			pos := i + j + 1
			return html[:pos] + tag + html[pos:]
		}
	}
	return tag + html
}
