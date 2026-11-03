// ERS-Proxy port dashboard. Renders /ui/monitor.json into switch cards with a
// faceplate per stack unit. Everything a device reports (names, aliases, LLDP)
// is untrusted: it is only ever set with textContent, never as HTML.
(function () {
  "use strict";
  var root = document.getElementById("dash");
  if (!root) return;
  var T = {};
  try { T = JSON.parse(root.dataset.i18n || "{}"); } catch (e) { /* keys shown as is */ }
  function t(k) { return T[k] || k; }
  var vlanSel = document.getElementById("dash-vlan");
  var search = document.getElementById("dash-search");
  var panel = document.getElementById("port-panel");
  var data = null;
  var selected = null; // {dev, ifindex}

  function el(tag, cls, text) {
    var e = document.createElement(tag);
    if (cls) e.className = cls;
    if (text !== undefined && text !== null) e.textContent = String(text);
    return e;
  }

  function speedText(bps) {
    if (!bps) return "–";
    if (bps >= 1e9) return (bps / 1e9) + "G";
    if (bps >= 1e6) return (bps / 1e6) + "M";
    return bps + "";
  }
  function rateText(bps) {
    if (bps >= 1e9) return (bps / 1e9).toFixed(2) + " Gbit/s";
    if (bps >= 1e6) return (bps / 1e6).toFixed(1) + " Mbit/s";
    if (bps >= 1e3) return (bps / 1e3).toFixed(0) + " kbit/s";
    return Math.round(bps) + " bit/s";
  }
  function durText(sec) {
    var d = Math.floor(sec / 86400), h = Math.floor(sec % 86400 / 3600), m = Math.floor(sec % 3600 / 60);
    return (d ? d + " " + t("dash.days") + " " : "") + h + "h " + m + "m";
  }

  function speedClass(p) {
    if (p.admin === "down") return "sadmin";
    if (p.oper !== "up") return "sdown";
    if (p.speed >= 1e10) return "s10g";
    if (p.speed >= 1e9) return "s1g";
    if (p.speed >= 1e8) return "s100m";
    return "s10m";
  }

  function matches(p, q) {
    if (!q) return true;
    var hay = [p.label, p.name, p.alias, p.lag_name, p.neighbor && p.neighbor.sys_name].join(" ").toLowerCase();
    return hay.indexOf(q) >= 0;
  }

  function tile(dev, p, vlan, q) {
    var b = el("button", "tile " + speedClass(p), p.number || "");
    b.type = "button";
    if (p.poe && p.poe.Status === "delivering") b.classList.add("poe");
    if (p.mode === "trunk" || p.mode === "hybrid") b.classList.add("trunk");
    if (p.neighbor && p.neighbor.bridge) b.classList.add("uplink");
    if (p.in_errors + p.out_errors > 0) b.classList.add("err");
    if (p.lag) b.classList.add("lagm");
    if (vlan) {
      if ((p.untagged || []).indexOf(vlan) >= 0) b.classList.add("vuntag");
      else if ((p.tagged || []).indexOf(vlan) >= 0) b.classList.add("vtag");
      else b.classList.add("dim");
    }
    if (!matches(p, q)) b.classList.add("dim");
    if (selected && selected.dev === dev.id && selected.ifindex === p.ifindex) b.classList.add("selected");
    var tip = p.label + (p.alias ? " – " + p.alias : "") + "\n" + p.oper + (p.oper === "up" ? " " + speedText(p.speed) : "");
    if (p.neighbor) tip += "\n→ " + p.neighbor.sys_name;
    b.title = tip;
    b.setAttribute("aria-label", tip.replace(/\n/g, ", "));
    b.addEventListener("click", function () { selected = { dev: dev.id, ifindex: p.ifindex }; render(); showPanel(dev, p); });
    return b;
  }

  function card(dev, vlan, q) {
    var c = el("section", "card switch-card");
    var head = el("div", "card-head");
    var h = el("h3");
    var hl = el("a", null, dev.name);
    hl.href = "/dashboard/" + encodeURIComponent(dev.id);
    h.appendChild(hl);
    head.appendChild(h);
    var drv = el("span", "badge", dev.driver || dev.source);
    head.appendChild(drv);
    c.appendChild(head);
    var meta = el("div", "switch-meta");
    if (dev.system) {
      meta.appendChild(el("span", null, dev.system.descr.split("  ")[0]));
      meta.appendChild(el("span", null, t("dash.uptime") + ": " + durText(dev.system.uptime)));
    }
    var phys = (dev.ports || []).filter(function (p) { return p.physical; });
    var up = phys.filter(function (p) { return p.oper === "up"; }).length;
    if (phys.length) meta.appendChild(el("span", null, t("dash.ports_up") + ": " + up + "/" + phys.length));
    if (dev.poe) meta.appendChild(el("span", null, t("dash.poe") + ": " + dev.poe.used + "/" + dev.poe.total + " W"));
    if (dev.last_poll) meta.appendChild(el("span", null, t("dash.last_poll") + ": " + new Date(dev.last_poll * 1000).toLocaleTimeString()));
    if (dev.error) meta.appendChild(el("span", "err", t("dash.error") + ": " + dev.error));
    c.appendChild(meta);
    if (!dev.ports) {
      c.appendChild(el("p", "muted small", t("dash.no_data")));
      return c;
    }
    var units = {};
    phys.forEach(function (p) { (units[p.unit] = units[p.unit] || []).push(p); });
    Object.keys(units).sort(function (a, b) { return a - b; }).forEach(function (u) {
      var ports = units[u].sort(function (a, b) { return a.number - b.number; });
      var wrap = el("div", "unit");
      if (Object.keys(units).length > 1) wrap.appendChild(el("div", "unit-label", t("dash.unit") + " " + u));
      var fp = el("div", "faceplate" + (ports.length <= 12 ? " single" : ""));
      // Odd ports on top, even ports below, like on the front panel.
      ports.forEach(function (p) { fp.appendChild(tile(dev, p, vlan, q)); });
      wrap.appendChild(fp);
      c.appendChild(wrap);
    });
    var others = (dev.ports || []).filter(function (p) { return !p.physical; });
    if (others.length) {
      var o = el("div", "other-ifs", t("dash.other_ifs") + ": ");
      others.forEach(function (p) {
        var b = el("button", "mini", p.name + " " + (p.oper === "up" ? speedText(p.speed) : p.oper));
        b.type = "button";
        b.addEventListener("click", function () { selected = { dev: dev.id, ifindex: p.ifindex }; render(); showPanel(dev, p); });
        o.appendChild(b);
      });
      c.appendChild(o);
    }
    if (dev.notes && dev.notes.length) {
      var n = el("details", "small muted");
      n.appendChild(el("summary", null, t("dash.notes")));
      dev.notes.forEach(function (x) { n.appendChild(el("p", null, x)); });
      c.appendChild(n);
    }
    return c;
  }

  function row(tbody, label, value) {
    if (value === undefined || value === null || value === "") return;
    var tr = el("tr");
    tr.appendChild(el("th", null, label));
    tr.appendChild(el("td", null, value));
    tbody.appendChild(tr);
  }

  function showPanel(dev, p) {
    document.getElementById("port-panel-title").textContent = dev.name + " · " + p.label + (p.name && p.name !== p.label ? " (" + p.name + ")" : "");
    var body = document.getElementById("port-panel-body");
    body.textContent = "";
    row(body, t("dash.alias"), p.alias);
    row(body, t("dash.state"), (p.admin === "down" ? t("dash.admin_down") : p.oper));
    if (p.oper === "up") row(body, t("dash.speed"), speedText(p.speed) + (p.duplex ? ", " + p.duplex : ""));
    row(body, t("dash.mode"), p.mode);
    row(body, t("dash.pvid"), p.pvid || "");
    row(body, t("dash.untagged"), (p.untagged || []).join(", "));
    row(body, t("dash.tagged"), (p.tagged || []).join(", "));
    if (p.poe) row(body, "PoE", p.poe.Status + (p.poe.Class ? ", class " + p.poe.Class : "") +
      (p.poe.PowerMW ? ", " + (p.poe.PowerMW / 1000).toFixed(1) + " W" : ""));
    row(body, t("dash.lag"), p.lag_name);
    if (p.neighbor) row(body, t("dash.neighbor"), p.neighbor.sys_name + (p.neighbor.port_id ? " (" + p.neighbor.port_id + ")" : ""));
    row(body, t("dash.traffic"), t("dash.in") + " " + rateText(p.in_bps) + " · " + t("dash.out") + " " + rateText(p.out_bps));
    if (p.speed) row(body, t("dash.utilization"), p.util.toFixed(1) + " %");
    row(body, t("dash.errors"), t("dash.in") + " " + p.in_errors + " · " + t("dash.out") + " " + p.out_errors);
    if (dev.system && p.last_change) row(body, t("dash.last_change"), durText(Math.max(0, dev.system.uptime - p.last_change)));
    var actions = document.getElementById("port-panel-actions");
    actions.textContent = "";
    var det = el("a", "button", t("dash.details"));
    det.href = "/dashboard/" + encodeURIComponent(dev.id) + "/port/" + encodeURIComponent(p.ifindex);
    actions.appendChild(det);
    if (dev.proxied) {
      var a = el("a", "button", t("dash.open_edm"));
      a.href = "/?open=" + encodeURIComponent(dev.id);
      actions.appendChild(a);
    }
    panel.hidden = false;
  }

  document.getElementById("port-panel-close").textContent = t("dash.close");
  document.getElementById("port-panel-close").addEventListener("click", function () {
    panel.hidden = true; selected = null; render();
  });

  function fillVLANs() {
    var seen = {};
    (data.devices || []).forEach(function (d) { (d.vlans || []).forEach(function (v) { if (!seen[v.id]) seen[v.id] = v.name; }); });
    var cur = vlanSel.value;
    while (vlanSel.options.length > 1) vlanSel.remove(1);
    Object.keys(seen).sort(function (a, b) { return a - b; }).forEach(function (id) {
      var o = el("option", null, id + (seen[id] ? " – " + seen[id] : ""));
      o.value = id;
      vlanSel.appendChild(o);
    });
    vlanSel.value = cur;
  }

  function render() {
    if (!data || !data.devices.length) return;
    var vlan = parseInt(vlanSel.value, 10) || 0;
    var q = search.value.trim().toLowerCase();
    root.textContent = "";
    data.devices.forEach(function (d) { root.appendChild(card(d, vlan, q)); });
  }

  function load() {
    fetch("/ui/monitor.json", { headers: { Accept: "application/json" }, credentials: "same-origin" })
      .then(function (r) { return r.ok ? r.json() : null; })
      .then(function (j) {
        if (!j) return;
        data = j;
        fillVLANs();
        render();
        if (selected && !panel.hidden) {
          data.devices.forEach(function (d) {
            if (d.id !== selected.dev) return;
            (d.ports || []).forEach(function (p) { if (p.ifindex === selected.ifindex) showPanel(d, p); });
          });
        }
      })
      .catch(function () { /* keep the last state */ });
  }
  vlanSel.addEventListener("change", render);
  search.addEventListener("input", render);
  load();
  window.setInterval(load, 15000);
})();
