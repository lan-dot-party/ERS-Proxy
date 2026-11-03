package edmproxy

import (
	"encoding/json"
	"strings"
)

// shimPath is served by the proxy itself below /d/{token}/.
const shimPath = "/__ersproxy/shim.js"

// cookiePath receives cookies that page script writes (shim → proxy).
const cookiePath = "/__ersproxy/cookie"

// shimJS returns the script injected at the top of every proxied HTML page.
//
//   - document.cookie: sandboxed documents have an opaque origin and may not
//     use cookies. EDM reads its session ID from the "auth" cookie, so the shim
//     provides the values of the proxy's cookie jar, and forwards writes to it.
//   - ersStatusText(xhr): the reason phrase of a response (see patches
//     "login-status-text"), which HTTP/2 and Go's server do not carry.
//   - window.open("") (sandbox only): EDM's table export and print open an
//     empty window and write into it. A window opened from an opaque origin
//     inside a page with Cross-Origin-Opener-Policy cannot be scripted
//     (window.open returns null), so the shim returns a stand-in, collects
//     what is written and opens the window with it as a Blob URL. The window
//     stays sandboxed.
func shimJS(cookies, base string) string {
	c, _ := json.Marshal(cookies)
	b, _ := json.Marshal(base + strings.TrimPrefix(cookiePath, "/"))
	return `(function () {
  "use strict";
  var jar = ` + string(c) + `;
  var post = ` + string(b) + `;
  function parse(s) {
    var out = {};
    if (!s) return out;
    s.split("; ").forEach(function (p) {
      var i = p.indexOf("=");
      if (i > 0) out[p.slice(0, i)] = p.slice(i + 1);
    });
    return out;
  }
  function serialize(m) {
    return Object.keys(m).map(function (k) { return k + "=" + m[k]; }).join("; ");
  }
  try {
    Object.defineProperty(document, "cookie", {
      configurable: true,
      get: function () { return jar; },
      set: function (v) {
        v = String(v);
        var first = v.split(";")[0];
        var i = first.indexOf("=");
        if (i <= 0) return;
        var m = parse(jar);
        var name = first.slice(0, i).trim();
        if (/max-age=0|max-age=-|expires=thu, 01 jan 1970/i.test(v)) {
          delete m[name];
        } else {
          m[name] = first.slice(i + 1);
        }
        jar = serialize(m);
        try {
          var x = new XMLHttpRequest();
          x.open("POST", post, true);
          x.setRequestHeader("Content-Type", "text/plain");
          x.send(v);
        } catch (e) { /* best effort */ }
      }
    });
  } catch (e) { /* very old engines: leave document.cookie alone */ }
  window.ersStatusText = function (xhr) {
    var t = null;
    try { t = xhr.getResponseHeader("X-ERSProxy-Status-Text"); } catch (e) { /* not available */ }
    return t || xhr.statusText || "";
  };
  // Windows with generated content: EDM's table export and print open an
  // empty window and write into it. From the sandbox (and with the portal's
  // Cross-Origin-Opener-Policy) such a window cannot be scripted, so the shim
  // hands out a stand-in, collects what is written and then opens the window
  // with the content as a Blob URL. The window stays sandboxed.
  if (String(self.origin) === "null" && typeof window.open === "function") {
    var realOpen = window.open;
    window.open = function (url, name, features) {
      if (url && String(url) !== "about:blank") return realOpen.apply(window, arguments);
      var buf = [], print = false, timer = null, real = null;
      function show() {
        timer = null;
        var html = buf.join("");
        if (print) html += "<script>window.print();<\/script>";
        real = realOpen.call(window, URL.createObjectURL(new Blob([html], { type: "text/html" })),
          name || "_blank", features || "");
      }
      function later() { if (!timer) timer = setTimeout(show, 0); }
      function text(args) { return Array.prototype.join.call(args, ""); }
      var doc = {
        open: function () { buf = []; return doc; },
        write: function () { buf.push(text(arguments)); },
        writeln: function () { buf.push(text(arguments) + "\n"); },
        close: later
      };
      return {
        document: doc,
        print: function () { print = true; later(); },
        focus: function () { try { if (real) real.focus(); } catch (e) { /* ignore */ } },
        close: function () { try { if (real) real.close(); } catch (e) { /* ignore */ } }
      };
    };
  }
})();
`
}
