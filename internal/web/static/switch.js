// ERS-Proxy switch and port pages: sortable tables, local time stamps and the
// chart hover. The markup is rendered (and escaped) on the server; this file
// only reads data attributes and writes with textContent.
(function () {
  "use strict";

  // Time stamps in the browser's time zone.
  var lang = document.documentElement.lang || undefined;
  var fmtFull = new Intl.DateTimeFormat(lang, { dateStyle: "short", timeStyle: "medium" });
  var fmtTime = new Intl.DateTimeFormat(lang, { hour: "2-digit", minute: "2-digit" });
  var fmtDate = new Intl.DateTimeFormat(lang, { day: "2-digit", month: "2-digit", hour: "2-digit", minute: "2-digit" });
  Array.prototype.forEach.call(document.querySelectorAll("time.ts[data-ts]"), function (e) {
    var ts = Number(e.dataset.ts);
    if (ts) e.textContent = fmtFull.format(new Date(ts * 1000));
  });
  Array.prototype.forEach.call(document.querySelectorAll("text.xl[data-ts]"), function (e) {
    var d = new Date(Number(e.dataset.ts) * 1000);
    e.textContent = (e.dataset.fmt === "date" ? fmtDate : fmtTime).format(d);
  });

  // Sortable tables: click a header with data-sort ("num" or "str").
  Array.prototype.forEach.call(document.querySelectorAll("table.sortable"), function (table) {
    var heads = table.tHead ? table.tHead.rows[0].cells : [];
    Array.prototype.forEach.call(heads, function (th, col) {
      var kind = th.dataset.sort;
      if (!kind) return;
      th.tabIndex = 0;
      function sort() {
        var asc = th.getAttribute("aria-sort") !== "ascending";
        Array.prototype.forEach.call(heads, function (h) { h.removeAttribute("aria-sort"); });
        th.setAttribute("aria-sort", asc ? "ascending" : "descending");
        var body = table.tBodies[0];
        var rows = Array.prototype.slice.call(body.rows);
        function key(r) {
          var c = r.cells[col];
          var v = c.dataset.v !== undefined ? c.dataset.v : c.textContent;
          return kind === "num" ? (v === "" || isNaN(Number(v)) ? -1 : Number(v)) : v.toLowerCase();
        }
        var keyed = rows.map(function (r, i) { return { r: r, k: key(r), i: i }; });
        keyed.sort(function (a, b) {
          var c = kind === "num" ? a.k - b.k : (a.k < b.k ? -1 : a.k > b.k ? 1 : 0);
          return (asc ? c : -c) || a.i - b.i;
        });
        keyed.forEach(function (x) { body.appendChild(x.r); });
      }
      th.addEventListener("click", sort);
      th.addEventListener("keydown", function (e) { if (e.key === "Enter" || e.key === " ") { e.preventDefault(); sort(); } });
    });
  });

  // Chart hover.
  var NS = "http://www.w3.org/2000/svg";
  function nums(s) { return s ? s.split(",").map(Number) : []; }
  function bps(v) {
    if (v >= 1e9) return (v / 1e9).toFixed(2) + " Gbit/s";
    if (v >= 1e6) return (v / 1e6).toFixed(1) + " Mbit/s";
    if (v >= 1e3) return (v / 1e3).toFixed(0) + " kbit/s";
    return Math.round(v) + " bit/s";
  }
  function svgEl(name, cls) {
    var e = document.createElementNS(NS, name);
    if (cls) e.setAttribute("class", cls);
    return e;
  }
  function div(cls) { var e = document.createElement("div"); e.className = cls; return e; }

  Array.prototype.forEach.call(document.querySelectorAll(".chart-wrap[data-l-in]"), function (wrap) {
    var svg = wrap.querySelector("svg.chart");
    if (!svg) return;
    var d = svg.dataset;
    var ts = nums(d.ts), inn = nums(d.in), out = nums(d.out), inMax = nums(d.inmax), outMax = nums(d.outmax), err = nums(d.err);
    var t0 = Number(d.t0), t1 = Number(d.t1), res = Number(d.res), top = Number(d.top);
    var showMax = res > 60;
    var X0 = 70, X1 = 886, YB = 262, YT = 14;
    var vb = svg.viewBox.baseVal;
    function px(t) { return X0 + (X1 - X0) * (t - t0) / (t1 - t0); }
    function py(v) { return YB - (YB - YT) * v / top; }

    var marker = svgEl("g", "hover");
    var rule = svgEl("line", "hover-rule");
    rule.setAttribute("y1", YB); rule.setAttribute("y2", YT);
    var dIn = svgEl("circle", "hover-dot-in"), dOut = svgEl("circle", "hover-dot-out");
    dIn.setAttribute("r", 4); dOut.setAttribute("r", 4);
    marker.appendChild(rule); marker.appendChild(dIn); marker.appendChild(dOut);
    svg.appendChild(marker);

    var tip = div("chart-tip");
    var rows = [div("chart-tip-time"), div("chart-tip-row"), div("chart-tip-row"), div("chart-tip-row")];
    rows.forEach(function (r) { tip.appendChild(r); });
    wrap.appendChild(tip);

    function line(el, label, value, extra) {
      el.textContent = label + " ";
      var b = document.createElement("b");
      b.textContent = value;
      el.appendChild(b);
      if (extra) el.appendChild(document.createTextNode(extra));
    }

    function show(e) {
      var r = svg.getBoundingClientRect();
      if (!r.width || !ts.length) return;
      var vx = vb.x + (e.clientX - r.left) * vb.width / r.width;
      var best = 0, bd = Infinity;
      for (var i = 0; i < ts.length; i++) {
        var dd = Math.abs(px(ts[i] + res / 2) - vx);
        if (dd < bd) { bd = dd; best = i; }
      }
      var i2 = best, x = px(ts[i2]);
      rule.setAttribute("x1", x); rule.setAttribute("x2", x);
      dIn.setAttribute("cx", x); dIn.setAttribute("cy", py(inn[i2]));
      dOut.setAttribute("cx", x); dOut.setAttribute("cy", py(out[i2]));
      marker.classList.add("on");
      var from = new Date(ts[i2] * 1000), to = new Date((ts[i2] + res) * 1000);
      rows[0].textContent = fmtDate.format(from) + "–" + fmtTime.format(to);
      line(rows[1], wrap.dataset.lIn, bps(inn[i2]), showMax ? " (" + wrap.dataset.lMax + " " + bps(inMax[i2]) + ")" : "");
      line(rows[2], wrap.dataset.lOut, bps(out[i2]), showMax ? " (" + wrap.dataset.lMax + " " + bps(outMax[i2]) + ")" : "");
      line(rows[3], wrap.dataset.lErr, String(err[i2]));
      rows[3].hidden = !err[i2];
      tip.classList.add("on");
      var left = (x - vb.x) * r.width / vb.width;
      var topPx = (Math.min(py(inn[i2]), py(out[i2])) - vb.y) * r.height / vb.height;
      var tw = tip.offsetWidth, th = tip.offsetHeight;
      var tl = left + 12;
      if (tl + tw > r.width) tl = left - 12 - tw;
      tip.style.left = Math.max(0, tl) + "px";
      tip.style.top = Math.max(0, Math.min(r.height - th, topPx - th / 2)) + "px";
    }
    function hide() { marker.classList.remove("on"); tip.classList.remove("on"); }
    svg.addEventListener("pointermove", show);
    svg.addEventListener("pointerdown", show);
    svg.addEventListener("pointerleave", hide);
  });

  // Pending configuration change: count down to the deadline, then reload so
  // that the page shows what happened. Works without JS too (the buttons are
  // plain forms; the server reverts at the deadline).
  Array.prototype.forEach.call(document.querySelectorAll(".config-countdown[data-deadline]"), function (e) {
    var deadline = Number(e.dataset.deadline) * 1000;
    function tick() {
      var left = Math.max(0, Math.round((deadline - Date.now()) / 1000));
      e.textContent = Math.floor(left / 60) + ":" + ("0" + (left % 60)).slice(-2);
      if (left > 0) {
        setTimeout(tick, 1000);
      } else {
        setTimeout(function () { window.location.reload(); }, 4000);
      }
    }
    tick();
  });

  // Port settings form: the mode decides which VLAN fields apply. Without JS
  // all fields stay visible and the mode select decides on the server.
  Array.prototype.forEach.call(document.querySelectorAll("form.config-form"), function (form) {
    var mode = form.querySelector("select.config-mode");
    if (!mode) return;
    function sync() { form.dataset.mode = mode.value; }
    mode.addEventListener("change", sync);
    sync();
  });
})();
