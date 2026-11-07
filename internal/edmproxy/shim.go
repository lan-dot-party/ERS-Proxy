package edmproxy

import (
	"encoding/json"
	"strings"
)

// shimPath is served by the proxy itself below /d/{token}/.
const shimPath = "/__ersproxy/shim.js"

// cookiePath receives cookies that page script writes (shim → proxy).
const cookiePath = "/__ersproxy/cookie"

// storagePath receives snapshots of the page's Web Storage (shim → proxy).
const storagePath = "/__ersproxy/storage"

// shimJS returns the script injected at the top of every proxied HTML page.
//
//   - document.cookie: sandboxed documents have an opaque origin and may not
//     use cookies. EDM reads its session ID from the "auth" cookie, so the shim
//     provides the values of the proxy's cookie jar, and forwards writes to it.
//   - localStorage, sessionStorage (sandbox only): a sandboxed document has no
//     Web Storage, access throws. EDM of VOSS then keeps its data in cookies,
//     which the proxy would send to the switch – and the switch rejects the
//     login. The shim provides both storages; it sends a snapshot after every
//     change (sendBeacon, so it survives the login form's navigation) and the
//     proxy keeps it with the proxy session, never sending it to the switch.
//   - Port selection: "Open in EDM" for a port of the dashboard passes the
//     ifIndex with the proxy session ("x:port" in the page storage); the shim
//     selects that port in EDM's device view and opens its edit dialog.
//   - ersStatusText(xhr): the reason phrase of a response (see patches
//     "login-status-text"), which HTTP/2 and Go's server do not carry.
//   - window.open("") (sandbox only): EDM's table export and print open an
//     empty window and write into it. A window opened from an opaque origin
//     inside a page with Cross-Origin-Opener-Policy cannot be scripted
//     (window.open returns null), so the shim returns a stand-in, collects
//     what is written and opens the window with it as a Blob URL. The window
//     stays sandboxed.
//   - Absolute request paths (rebase, SwOS only): SwOS's page loads its data
//     with XMLHttpRequest from absolute paths ("/link.b"), which would leave
//     the proxy path. The shim maps such paths below /d/{token}/.
func shimJS(cookies, storage, base string, rebase bool) string {
	c, _ := json.Marshal(cookies)
	bp, _ := json.Marshal(base)
	rb := "false"
	if rebase {
		rb = "true"
	}
	b, _ := json.Marshal(base + strings.TrimPrefix(cookiePath, "/"))
	sp, _ := json.Marshal(base + strings.TrimPrefix(storagePath, "/"))
	if !json.Valid([]byte(storage)) {
		storage = "{}"
	}
	return `(function () {
  "use strict";
  var jar = ` + string(c) + `;
  var post = ` + string(b) + `;
  var base = ` + string(bp) + `;
  if (` + rb + ` && window.XMLHttpRequest) {
    // "/x" -> base + "x"; protocol-relative ("//host") and absolute URLs and
    // paths already below base stay as they are.
    var realXHROpen = XMLHttpRequest.prototype.open;
    XMLHttpRequest.prototype.open = function (method, url) {
      var args = Array.prototype.slice.call(arguments);
      if (typeof url === "string" && url.charAt(0) === "/" && url.charAt(1) !== "/" && url.indexOf(base) !== 0) {
        args[1] = base + url.slice(1);
      }
      return realXHROpen.apply(this, args);
    };
  }
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
  // Page storage kept by the proxy: "l:<key>" for localStorage, "s:<key>"
  // for sessionStorage (both only in the sandbox), "x:port" for the port to
  // select (see below).
  var data = ` + storage + `;
  var storagePost = ` + string(sp) + `;
  var seq = 0;
  var save = function () {
    seq = Math.max(seq + 1, Date.now() * 1000);
    var body = seq + "\n" + JSON.stringify(data);
    try {
      if (navigator.sendBeacon && navigator.sendBeacon(storagePost, new Blob([body], { type: "text/plain" }))) return;
      var x = new XMLHttpRequest();
      x.open("POST", storagePost, true);
      x.setRequestHeader("Content-Type", "text/plain");
      x.send(body);
    } catch (e) { /* best effort: the values stay in this document */ }
  };
  var noStorage = false;
  try { noStorage = !window.localStorage; } catch (e) { noStorage = true; }
  if (noStorage) {
    var makeStorage = function (prefix) {
      var keys = function () {
        return Object.keys(data).filter(function (k) { return k.indexOf(prefix) === 0; });
      };
      var st = {
        getItem: function (k) {
          k = prefix + k;
          return Object.prototype.hasOwnProperty.call(data, k) ? data[k] : null;
        },
        setItem: function (k, v) { data[prefix + k] = String(v); save(); },
        removeItem: function (k) {
          if (Object.prototype.hasOwnProperty.call(data, prefix + k)) { delete data[prefix + k]; save(); }
        },
        clear: function () { keys().forEach(function (k) { delete data[k]; }); save(); },
        key: function (i) { var ks = keys(); return i >= 0 && i < ks.length ? ks[i].slice(prefix.length) : null; }
      };
      Object.defineProperty(st, "length", { get: function () { return keys().length; } });
      return st;
    };
    var local = makeStorage("l:"), session = makeStorage("s:");
    try {
      Object.defineProperty(window, "localStorage", { configurable: true, get: function () { return local; } });
      Object.defineProperty(window, "sessionStorage", { configurable: true, get: function () { return session; } });
    } catch (e) { /* leave Web Storage alone */ }
  }
  // "Open in EDM" for a port: once EDM's device view shows the port
  // (element Port_<ifIndex> on ERS and VSP), select it and open its edit
  // dialog, as a click and a double-click would. Pages without the device
  // view (the login page) leave the request for the next page.
  var tries = 0;
  var selectPort = function () {
    var n = data["x:port"];
    if (!n || !/^[0-9]+$/.test(n)) return;
    var el = document.getElementById("Port_" + n);
    if (!el || !window.theView) {
      // ERS (Ext JS 2) draws the device view only once its tab is shown;
      // "Switch Summary" is in front after the login.
      if (tries === 6 && window.theView) {
        try {
          var center = window.Ext && Ext.getCmp && Ext.getCmp("center");
          if (center && center.setActiveTab) center.setActiveTab("deviceImage");
        } catch (e) { /* no such tab */ }
      }
      if (++tries < 120) setTimeout(selectPort, 500);
      return;
    }
    delete data["x:port"];
    save();
    // EDM draws the ports before it has loaded what the dialog needs (VOSS 8
    // ignores an early double-click), so repeat until a dialog tab appears.
    var tabCount = function () {
      return document.querySelectorAll(".x-tab-inner, .x-tab-strip-text").length;
    };
    // EDM's handlers are called directly (not through dispatchEvent), so
    // that an exception of a too early attempt ends up here instead of in the
    // console.
    var mouse = function (type) {
      var ev = new MouseEvent(type, { bubbles: true, cancelable: true, view: window });
      var handler = el["on" + type];
      if (typeof handler === "function") handler.call(el, ev);
      else el.dispatchEvent(ev);
    };
    var fire = function (attempt) {
      var before = tabCount();
      try {
        if (attempt === 0 && el.scrollIntoView) el.scrollIntoView({ block: "nearest" });
        mouse("click");
        mouse("dblclick");
      } catch (e) { /* EDM not ready yet: try again */ }
      setTimeout(function () {
        if (tabCount() <= before && attempt < 20) fire(attempt + 1);
      }, 2000);
    };
    setTimeout(function () { fire(0); }, 500);
  };
  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", selectPort);
  } else {
    selectPort();
  }
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
