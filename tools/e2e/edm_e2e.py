#!/usr/bin/env python3
"""Browser test: EDM through ERS-Proxy in headless Chromium.

Runs only locally, against a mock switch that serves a real EDM webroot (which
is not part of this repository):

    go run ./tools/mockswitch -root /path/to/edm/webroot -listen 127.0.0.1:18765 &
    ./bin/ers-proxy -c test/config.yaml server &          # device 1 -> http://127.0.0.1:18765
    python3 -I tools/e2e/edm_e2e.py http://127.0.0.1:9050 admin 'password' 1 /tmp/shots

Checks:
  * the EDM login and the device view load inside the workspace tab,
  * no JavaScript errors in the console,
  * the EDM document runs in the sandbox: it cannot read the portal, cannot
    reach the parent frame, and its document.cookie comes from the shim,
  * every request the EDM makes stays below /d/{token}/,
  * an export window (window.open("") + document.write) gets its content.

Needs: python3-selenium, chromium, chromium-driver (Debian packages).
Exit code 0 when all checks pass.
"""
import json
import sys
import time

from selenium import webdriver
from selenium.common.exceptions import NoAlertPresentException
from selenium.webdriver.chrome.options import Options
from selenium.webdriver.chrome.service import Service
from selenium.webdriver.common.by import By

base, user, password, device_id, shots = sys.argv[1:6]
switch_user = sys.argv[6] if len(sys.argv) > 6 else "RW"
switch_password = sys.argv[7] if len(sys.argv) > 7 else "secure"

opts = Options()
for a in ["--headless=new", "--no-sandbox", "--disable-gpu", "--window-size=1600,1000"]:
    opts.add_argument(a)
opts.set_capability("goog:loggingPrefs", {"browser": "ALL", "performance": "ALL"})
opts.set_capability("unhandledPromptBehavior", "accept")
drv = webdriver.Chrome(service=Service("/usr/bin/chromedriver"), options=opts)
failures = []


def check(ok, msg):
    print(("PASS " if ok else "FAIL ") + msg)
    if not ok:
        failures.append(msg)


def accept_alerts(seconds):
    for _ in range(seconds):
        time.sleep(1)
        try:
            alert = drv.switch_to.alert
            print("ALERT:", alert.text)
            failures.append("unexpected alert: " + alert.text)
            alert.accept()
        except NoAlertPresentException:
            pass


VERBOSE = bool(__import__("os").environ.get("E2E_VERBOSE"))


def console_errors():
    errs = []
    for e in drv.get_log("browser"):
        if VERBOSE:
            print("LOG", e["level"], e["message"][:400])
        if e["level"] == "SEVERE":
            errs.append(e["message"])
    return errs


try:
    drv.get(base + "/login")
    drv.find_element(By.NAME, "username").send_keys(user)
    drv.find_element(By.NAME, "password").send_keys(password)
    drv.find_element(By.CSS_SELECTOR, "form button[type=submit]").click()
    time.sleep(1)
    drv.execute_script("try{localStorage.removeItem('ersp.tabs')}catch(e){}")
    drv.get(base + "/")
    time.sleep(1)
    drv.get_log("performance")  # discard the portal's own requests
    link = drv.find_element(By.CSS_SELECTOR, '.device-open[data-id="%s"]' % device_id)
    drv.execute_script("arguments[0].dataset.mode='embedded'", link)
    link.click()
    accept_alerts(3)
    frame = drv.find_element(By.CSS_SELECTOR, "iframe.pane")
    drv.switch_to.frame(frame)
    frame_url = drv.execute_script("return document.URL")
    check(frame_url.startswith(base + "/d/"), "tab shows the proxy path")
    time.sleep(1)
    if drv.find_elements(By.NAME, "username"):
        drv.find_element(By.NAME, "username").send_keys(switch_user)
        drv.find_element(By.NAME, "password").send_keys(switch_password)
        drv.save_screenshot(shots + "/e2e-1-login.png")
        drv.find_element(By.NAME, "goto").click()
    else:
        print("INFO no EDM login form (automatic login)")
    accept_alerts(10)
    drv.switch_to.default_content()
    drv.save_screenshot(shots + "/e2e-2-edm.png")
    drv.switch_to.frame(drv.find_element(By.CSS_SELECTOR, "iframe.pane"))

    text = drv.execute_script("return document.body ? document.body.innerText : ''")
    check("Discovering" not in text or "Summary" in text, "EDM got past device discovery")
    check(drv.execute_script("return typeof Ext !== 'undefined' && !!document.querySelector('.x-panel')"),
          "Ext JS panels rendered")

    probe = drv.execute_async_script("""
        var done = arguments[arguments.length - 1];
        var out = {};
        try { out.origin = String(self.origin); } catch (e) { out.origin = 'error ' + e; }
        try { out.cookie = document.cookie; } catch (e) { out.cookie = 'error ' + e; }
        try { out.parent = parent.document.title; } catch (e) { out.parent = 'blocked'; }
        try { localStorage.getItem('x'); out.storage = 'available'; } catch (e) { out.storage = 'blocked'; }
        fetch('/admin/devices', {credentials: 'include'}).then(function (r) {
            return r.text().then(function (t) { out.portal = r.status + ' ' + (t.indexOf('ers-proxy-devices') >= 0 || t.indexOf('/admin/devices/new') >= 0 ? 'ADMIN PAGE' : 'no data'); });
        }).catch(function (e) { out.portal = 'blocked: ' + e; }).then(function () { done(out); });
    """)
    print("SANDBOX", json.dumps(probe))
    check(probe["origin"] == "null", "EDM document has an opaque origin")
    check(probe["parent"] == "blocked", "EDM cannot reach the portal frame")
    check("ADMIN PAGE" not in probe["portal"], "EDM cannot read portal pages (%s)" % probe["portal"])
    check(probe["cookie"].startswith("auth="), "document.cookie comes from the shim")

    # Table export/print: EDM writes into an empty popup (shim loads it as a Blob URL).
    before = set(drv.window_handles)
    drv.execute_script("""
        var b = document.createElement('button');
        b.id = 'ersp-e2e-export'; b.textContent = 'export';
        b.style.cssText = 'position:fixed;top:0;left:0;z-index:99999';
        b.onclick = function () {   // a click, so that the popup blocker allows it
            try {
                var w = window.open('', 'ersp_e2e_export');
                w.document.open(); w.document.writeln('<p>e2e-export</p>'); w.document.close();
                b.dataset.r = 'opened';
            } catch (e) { b.dataset.r = 'error: ' + e; }
        };
        document.body.appendChild(b);""")
    drv.find_element(By.ID, "ersp-e2e-export").click()
    time.sleep(0.5)
    print("EXPORT click:", drv.find_element(By.ID, "ersp-e2e-export").get_attribute("data-r"))
    time.sleep(2)
    main = drv.current_window_handle
    popup_text = ""
    for h in set(drv.window_handles) - before:
        drv.switch_to.window(h)
        popup_text = drv.execute_script("return document.body ? document.body.innerText : ''")
        drv.close()
    drv.switch_to.window(main)
    print("EXPORT windows=%d text=%r" % (len(set(drv.window_handles) - before) + (1 if popup_text else 0), popup_text[:80]))
    check("e2e-export" in popup_text, "export window receives the written content")
    drv.switch_to.default_content()

    outside = []
    for entry in drv.get_log("performance"):
        msg = json.loads(entry["message"])["message"]
        if msg.get("method") != "Network.requestWillBeSent":
            continue
        url = msg["params"]["request"]["url"]
        if url.startswith(base) and not url.startswith(base + "/d/") and not url.startswith(base + "/open/") \
                and not url.startswith(base + "/admin/devices"):
            outside.append(url)
    check(not outside, "all EDM requests stay below /d/ %s" % (outside[:5] if outside else ""))
    errs = console_errors()
    errs = [e for e in errs if "/admin/devices" not in e]  # the deliberate probe above
    for e in errs:
        print("CONSOLE", e[:300])
    check(not errs, "no console errors")
finally:
    drv.quit()

print("RESULT", "OK" if not failures else "FAILED: %d" % len(failures))
sys.exit(1 if failures else 0)
