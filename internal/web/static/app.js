// ERS-Proxy UI. No inline scripts: the CSP only allows files from /static.
(function () {
  "use strict";

  // Confirm dialogs: <form data-confirm="..."> or <button data-confirm="...">.
  document.addEventListener("submit", function (e) {
    var form = e.target;
    var submitter = e.submitter;
    var msg = (submitter && submitter.dataset.confirm) || form.dataset.confirm;
    if (msg && !window.confirm(msg)) {
      e.preventDefault();
    }
  });

  // Device form: proxy settings only for ERS switches.
  var kind = document.getElementById("device-kind");
  if (kind) {
    var form = document.getElementById("device-form");
    var sync = function () {
      form.classList.toggle("device-kind-link", kind.value === "link");
    };
    kind.addEventListener("change", sync);
    sync();
  }

  var workspace = document.getElementById("workspace");
  if (!workspace) {
    return;
  }

  var store = {
    get: function (k) {
      try { return JSON.parse(window.localStorage.getItem(k) || "null"); } catch (err) { return null; }
    },
    set: function (k, v) {
      try { window.localStorage.setItem(k, JSON.stringify(v)); } catch (err) { /* private mode */ }
    }
  };

  // --- Sidebar filter ---
  var filter = document.getElementById("device-filter");
  if (filter) {
    filter.addEventListener("input", function () {
      var q = filter.value.trim().toLowerCase();
      var items = workspace.querySelectorAll(".device");
      for (var i = 0; i < items.length; i++) {
        var hay = (items[i].dataset.name || "").toLowerCase();
        items[i].hidden = q !== "" && hay.indexOf(q) < 0;
      }
    });
  }

  // --- Tabs ---
  var tabbar = document.getElementById("tabbar");
  var panes = document.getElementById("panes");
  var empty = document.getElementById("pane-empty");
  var tpl = document.getElementById("tab-template");
  var tabs = [];      // {id, kind, name, href, tab, frame}
  var active = null;  // id

  function deviceLink(id) {
    return workspace.querySelector('.device-open[data-id="' + id + '"]');
  }

  function save() {
    store.set("ersp.tabs", {
      open: tabs.map(function (t) { return t.id; }),
      active: active
    });
  }

  function find(id) {
    for (var i = 0; i < tabs.length; i++) {
      if (tabs[i].id === id) return tabs[i];
    }
    return null;
  }

  function activate(id) {
    active = id;
    tabs.forEach(function (t) {
      var on = t.id === id;
      t.tab.classList.toggle("active", on);
      t.tab.setAttribute("aria-selected", on ? "true" : "false");
      t.frame.hidden = !on;
    });
    var items = workspace.querySelectorAll(".device");
    for (var i = 0; i < items.length; i++) {
      var a = items[i].querySelector(".device-open");
      items[i].classList.toggle("active", a && a.dataset.id === id);
    }
    empty.hidden = tabs.length > 0;
    var t = find(id);
    document.title = (t ? t.name + " · " : "") + "ERS-Proxy";
    save();
  }

  function openPopup(href, id) {
    window.open(href, "ersp-" + id, "popup,width=1280,height=860");
  }

  function openTab(link) {
    var id = link.dataset.id;
    var existing = find(id);
    if (existing) {
      activate(id);
      return;
    }
    var node = tpl.content.firstElementChild.cloneNode(true);
    node.querySelector(".tab-title").textContent = link.dataset.name;
    node.title = link.dataset.name;
    var frame = document.createElement("iframe");
    frame.className = "pane";
    frame.title = link.dataset.name;
    if (link.dataset.kind === "link") {
      frame.referrerPolicy = "no-referrer";
    }
    frame.src = link.getAttribute("href");
    var t = { id: id, kind: link.dataset.kind, name: link.dataset.name, href: link.getAttribute("href"), tab: node, frame: frame };
    node.addEventListener("click", function (e) {
      var btn = e.target.closest("button");
      var act = btn && btn.dataset.act;
      if (act === "close") {
        close(id);
      } else if (act === "reload") {
        frame.src = t.href;
        activate(id);
      } else if (act === "popout") {
        openPopup(t.href, id);
        close(id);
      } else {
        activate(id);
      }
    });
    node.addEventListener("auxclick", function (e) {
      if (e.button === 1) close(id);
    });
    tabs.push(t);
    tabbar.appendChild(node);
    panes.appendChild(frame);
    activate(id);
  }

  function close(id) {
    var t = find(id);
    if (!t) return;
    var idx = tabs.indexOf(t);
    tabs.splice(idx, 1);
    t.tab.remove();
    t.frame.remove(); // ends the frame; the proxy session expires on the server
    if (active === id) {
      var next = tabs[idx] || tabs[idx - 1];
      active = next ? next.id : null;
    }
    activate(active);
  }

  workspace.addEventListener("click", function (e) {
    var a = e.target.closest("a");
    if (!a || e.defaultPrevented || e.button !== 0 || e.ctrlKey || e.metaKey || e.shiftKey || e.altKey) {
      return;
    }
    if (a.dataset.openAs === "window") {
      e.preventDefault();
      openPopup(a.getAttribute("href"), a.dataset.id);
      return;
    }
    if (!a.classList.contains("device-open")) {
      return;
    }
    var mode = a.dataset.mode;
    if (mode === "embedded") {
      e.preventDefault();
      openTab(a);
    } else if (mode === "tab") {
      e.preventDefault();
      window.open(a.getAttribute("href"), "_blank", "noopener,noreferrer");
    } else if (mode === "window") {
      e.preventDefault();
      openPopup(a.getAttribute("href"), a.dataset.id);
    }
  });

  // Restore the tabs of the last visit (only devices that still exist and
  // still open embedded).
  var saved = store.get("ersp.tabs");
  if (saved && saved.open) {
    saved.open.forEach(function (id) {
      var link = deviceLink(id);
      if (link && link.dataset.mode === "embedded") {
        openTab(link);
      }
    });
    if (saved.active && find(saved.active)) {
      activate(saved.active);
    }
  }
  activate(active);

  // /?open=<id> (from the dashboard): open that device like a click would.
  try {
    var openID = new URLSearchParams(window.location.search).get("open");
    if (openID && /^[0-9]+$/.test(openID)) {
      var toOpen = deviceLink(openID);
      if (toOpen) toOpen.click();
      window.history.replaceState(null, "", "/");
    }
  } catch (err) { /* old browser */ }

  // --- Status dots ---
  var sidebar = document.getElementById("sidebar");
  function refreshStatus() {
    fetch("/ui/status", { headers: { Accept: "application/json" }, credentials: "same-origin" })
      .then(function (r) { return r.ok ? r.json() : null; })
      .then(function (data) {
        if (!data) return;
        var dots = workspace.querySelectorAll("[data-status]");
        for (var i = 0; i < dots.length; i++) {
          var state = data[dots[i].dataset.status] || "unknown";
          dots[i].className = "dot dot-" + state;
          dots[i].title = sidebar.dataset["label" + state.charAt(0).toUpperCase() + state.slice(1)] || state;
        }
      })
      .catch(function () { /* offline: keep the old state */ });
  }
  refreshStatus();
  window.setInterval(refreshStatus, 30000);
})();
