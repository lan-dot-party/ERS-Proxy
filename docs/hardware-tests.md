# Hardware test plan

Roadmap step 6.1 (and the open part of 7.10). Everything in ERS-Proxy up to now was verified
against mocks and simulators only. This plan lists what has to be checked on real devices, how,
what result is expected, and what to record. Work through it top to bottom; each section can be
done on its own day.

## Safety rules (read first)

- **Read-only.** Never click *Apply*, *Save*, *Reset* or *Delete* in EDM, never run
  configuration commands on the switch, never send SNMP SETs (ERS-Proxy cannot). Navigate and
  look only.
- Use a **read-only account** for EDM (e.g. the `RO` user) and a **read-only SNMP community or
  SNMPv3 read-only user**. Only where a step needs more, it says so.
- Production switches: agree on a time window with the maintainer before starting.
- Never commit captures, screenshots, HAR files or logs that contain IP addresses, names,
  passwords or communities. `snmp-capture` anonymises by default; still check the file.
- Record results in the table at the end and in [compatibility.md](compatibility.md).

## 0. Collect before starting

| Item | Value (fill in locally, not in the repository) |
|---|---|
| ERS model(s), stack size, units | |
| ERS firmware version (`show sys-info`, or EDM Summary) | |
| ERS management address, HTTP and/or HTTPS enabled | |
| EDM read-only account | |
| SNMP on the ERS: version, read-only community or v3 user | |
| TP-Link T1500G address, SNMP read-only community | |
| Browser versions used (Firefox, Chromium) | Chromium 156, Firefox ESR 153.6, WebKitGTK 2.54 (Safari's engine) – all through Selenium |
| ERS-Proxy version (`ers-proxy version`) | |

The switch must be reachable from the ERS-Proxy host (TCP 80/443 for EDM, UDP 161 for SNMP).

## 1. Basics at the ERS

1. Reachability and web server:
   ```bash
   curl -sI http://<ers>/ | head
   ```
   *Expect:* `Server: Rapid Logic/1.1` (or similar) and `Set-Cookie: auth=…`.
   *Record:* exact `Server` header, whether `Content-Encoding: gzip` appears.
2. TLS (only if HTTPS is enabled):
   ```bash
   ers-proxy tls-probe https://<ers>
   ```
   *Expect:* at least one successful line; most likely `legacy  TLS 1.0  TLS_RSA_WITH_…`.
   *Record:* versions and ciphers that work, key size, fingerprint. If nothing works: does the
   switch only offer SSLv3 or DHE suites? (Not supported by Go – then HTTPS is not possible.)
   Compare the fingerprint with the certificate shown on the switch (ACLI, command depends on
   the firmware, e.g. `show ssl certificate`).

## 2. EDM through the proxy over HTTP

Add the switch as kind *ERS switch* with `http://<ers>`.

1. Open it in a **workspace tab**. The EDM login page appears without a "browser not supported"
   alert.
2. Log in with the read-only account.
   *Expect:* the device view loads, "Discovering Device" disappears, the *Switch Summary* or
   *Device Physical View* shows real data.
3. Open read-only dialogs and check that data appears (do not apply anything):
   - Edit → Chassis / Unit information
   - Edit → Ports (a single port, then the port table)
   - VLAN → VLANs (table, then *Ports* tab)
   - VLAN / MLT dialog
   - Graph → Port statistics of an active port (charts)
   - PoE dialogs (PWR models)
   - Help button (opens a help window) and an *Export* button of a table (opens a window)
4. Browser developer tools (F12) → Console: note every error; Network: no request may leave
   `/d/<token>/` (all requests go to the proxy path).
5. ERS-Proxy → Settings → Diagnostics: note the applied patches (expected: `ext-header-map`,
   `browser-whitelist`, `login-xhr-root`, `login-status-text`, …) and any "ambiguous" entry.
6. Repeat 1–2 with open mode **new browser tab** and **window**.
7. Repeat 1–3 in the second browser (Firefox and Chromium).
8. Log out with EDM's own *Log out* link; then log out of ERS-Proxy and check that the EDM tab
   shows "logged out of ERS-Proxy" after at most 30 s.

*If a dialog fails:* note the dialog, the first console error, and try once with the device's
isolation set to *off* (device settings) to see whether the sandbox is the cause. Set it back to
*sandbox* afterwards.

## 3. EDM through the proxy over HTTPS

Only if HTTPS is enabled. Add a second device (or change the address) to `https://<ers>`.

1. Opening the device shows the certificate confirmation page. Compare the fingerprint with
   section 1.2, confirm.
2. *Expect:* EDM loads as in section 2; Diagnostics shows TLS version, cipher and profile
   (`legacy` expected).
3. Repeat section 2 steps 2–3 briefly.

## 4. Auto-login (experimental)

1. In the device settings, store the read-only EDM login (card "Automatic login").
2. Open the device. *Expect:* the device view appears without the EDM login page; Diagnostics
   shows "Last automatic login".
3. Store a wrong password, open again. *Expect:* the EDM login page; Diagnostics "Last error:
   auto-login: …". Then store the right one again or delete the login.
4. *Record:* whether the switch answers the login check with 200, and what happens with an
   account that must change its password (only if such an account exists – do not create one).

## 5. Port dashboard over SNMP (ERS)

Set the device's monitoring source to *SNMP*, SNMP version and port, and the read-only
credentials (card "SNMP access").

1. ```bash
   ers-proxy -c <config> snmp-test <device-id>
   ers-proxy -c <config> snmp-test <device-id> --collect
   ```
   *Expect:* driver `ers`, the number of units, one line per front-panel port.
2. Compare with the switch (EDM or ACLI, read-only; ACLI commands vary by firmware):

   | Check | Compare with | Why it matters |
   |---|---|---|
   | Port numbering: unit 2 port 1 must be `2/1` (ifIndex 129 on ERS 5500/5600) | port names in EDM, `show interfaces` | the 128-per-unit rule comes from the EDM code only |
   | Link up/down and speed/duplex of a few ports | EDM port table | |
   | VLAN list and names | `show vlan`, EDM VLAN table | `rcVlanName`/`rcVlanPortMembers` |
   | PVID and untagged/tagged per port, including a trunk and an access port | EDM VLAN → Ports (Tagging column) | PortSet bitmap and tagging modes |
   | MLT members | `show mlt`, EDM MLT | `rcMltPortMembers` |
   | PoE status per port and budget | `show poe-port-status` / EDM PoE | group = unit mapping |
   | Stack units, models, firmware, serials | `show stack-info` / EDM Chassis | `s5ChasComTable` index `3.<unit*10>.0` |
   | LLDP neighbours on uplinks | `show lldp neighbor` | local port mapping |
3. In the web UI: Dashboard shows the switch; after two polls (2 minutes) traffic rates appear.
   Compare the rate of a busy port with EDM's port graph (order of magnitude is enough).
4. Capture for the test suite:
   ```bash
   ers-proxy -c <config> snmp-capture <device-id> --out ers-real.snmprec
   ```
   Check the file for leftover names/addresses. Keep it locally; it is used to turn the
   synthetic `ers5600` fixture into one that matches the real switch.
5. *If something is wrong:* note which column/port, and the matching lines of the capture (OID
   and value). Most likely candidates: ports per unit (128 vs 64), PortSet bit order, tagging
   mode values.

## 6. Port dashboard over EDM (ERS, experimental)

Needs the stored EDM login of section 4 (read-only account).

1. Change the monitoring source of the device to *EDM*, interval ≥ 60 s.
2. *Expect:* after a poll, the dashboard shows the same ports, VLANs, MLTs and units as in
   section 5 (no PoE and no EtherLike duplex via EDM; duplex comes from `rcPortOperDuplex`).
3. While the EDM data source is active, use EDM in the browser at the same time (section 2).
   *Record:* does the switch limit concurrent web sessions? Does the poller log the browser out
   or the other way round? (`bsEdmInactivityTimeout`, session table size.)
4. Logs: `journalctl -u ers-proxy` (or the user unit) – "poll failed" lines and their errors.
5. Switch the source back to *SNMP* if SNMP works.

## 7. TP-Link T1500G (step 7.10)

1. Add it as kind *Link*, monitoring source *SNMP*, driver *auto*, read-only community.
2. ```bash
   ers-proxy -c <config> snmp-test <device-id> --collect
   ers-proxy -c <config> snmp-capture <device-id> --out tplink-real.snmprec
   ```
3. *Expect:* driver `tplink`. Check whether VLANs, PVID and untagged/tagged ports appear and
   match the switch's web UI.
4. *Decide:* if the VLANs are right, the TP-Link driver stays "generic + model" and the note can
   go; if they are empty or wrong, look in the capture for `1.3.6.1.4.1.11863.` objects with VLAN
   data and extend `internal/monitor/drivers/tplink.go`.

## 8. Health checks

1. The status dot of every device is green; for a device with a wrong address it turns red within
   about a minute.
2. HTTPS device: if the switch certificate is regenerated (only if planned anyway), the dot turns
   orange ("certificate changed") and opening the device is blocked until an admin confirms.

## After the tests

- Update [compatibility.md](compatibility.md): model, firmware, transport, result, how verified.
- Update [ROADMAP.md](ROADMAP.md): mark 6.1 done (and 7.10 final), describe what was verified.
- Remove the "not yet verified on hardware" notes that the tests confirmed (ERS driver note,
  TP-Link note) and adapt the synthetic fixtures in `testdata/snmpsim/gen_monitor.py` to what the
  real captures showed.
- Fix every problem found, each with a test (mock or fixture) that reproduces it.
- Remove the demo devices and the simulator from test installations once real switches are
  monitored.

## Result table

| # | Check | Result (ok / problem) | Notes |
|---|---|---|---|
| 1.1 | Web server headers | ok | 2026-11-03, ERS 5632FD 6.6.3.015: `Server: Rapid Logic/1.1`, `Set-Cookie: auth=…`, gzip; `HEAD /` answers 404, `GET /` 401 with the login page |
| 1.2 | TLS probe | n/a | the tested switch offers HTTP only |
| 2.1–2.2 | EDM login and device view (HTTP) | ok after fix | the *Log On* click did nothing: the switch answers `Cache-Control: max-age=0` with an empty 304 (fixed) |
| 2.3 | Read-only dialogs | ok | Chassis, Bridge, VLANs (+ Ports), MLT/LACP, STP, Graph → Chassis; port dialogs need a port selected (not tested), no PoE on this model; help files not on the switch (TFTP); export window was empty (fixed) |
| 2.4 | Console errors / requests outside `/d/` | ok | no JS errors; nothing outside `/d/`; 502s from refused connections during bursts (fixed: retry, image cache) |
| 2.5 | Patch report | ok | `browser-whitelist`, `ext-header-map`, `help-path`, `legacy-css-paths`, `login-redirect-root`, `login-status-text`, `login-xhr-root`; nothing ambiguous |
| 2.6–2.7 | Open modes, second browser | ok (mock and VSP) | embedded, tab and window ok in Chromium; Firefox ESR 153.6 (2026-11-04): mock ERS through `tools/e2e/edm_e2e.py` (login, discovery, sandbox checks, export window, no requests outside `/d/`, no JS errors), on the VSPs embedded, tab and window, auto-login and manual login, dialogs, write test, EDM logout, "Open in EDM" for a port from the dashboard panel. The same in WebKitGTK 2.54 (Safari's engine, MiniBrowser under Xvfb) on 2026-11-04, including the manual login. The real ERS was offline, so ERS in Firefox and WebKit is verified against the mock only; Safari itself (macOS) only by hand |
| 2.8 | Logout behaviour | ok | EDM's *Log out* shows "EDM Logout!"; after the portal logout the EDM window shows "You are logged out of ERS-Proxy" |
| 3 | EDM over HTTPS, pinning, legacy TLS | n/a | no HTTPS on the switch |
| 4 | Auto-login | ok after fix | login check answers 200; a wrong password also gets 200 with `authError.html` and was taken as success (fixed); after a few failed logins the switch locks the account for about 1–2 minutes |
| 5.1–5.2 | SNMP dashboard data vs. switch | ok after fix | driver `ers`, 32 ports, VLANs, PVID, tagging, LLDP match EDM; OOB port showed as "430/89" and unit model/firmware came from the wrong columns (fixed) |
| 5.3 | Traffic rates | ok | rates of the uplink plausible (kbit/s range) |
| 6 | EDM data source, concurrent sessions | ok after fixes | same ports, VLANs, tagging and unit as SNMP; LLDP needed `indexNA` (fixed); poller and browser sessions do not log each other out; during EDM starts the switch sometimes answers polls with 503 or refuses connections for several seconds |
| 7 | TP-Link driver decision | done | T1500G-8T and Omada SG2210P have no Q-BRIDGE; all interfaces report ifType 6. Driver now reads TPLINK-DOT1Q-VLAN-MIB and numbers ports by `ifName`; `snmp-capture` stopped at an undecodable LLDP value and a PoE timeout (now skipped) |
| 8 | Health checks | ok | 8.1: switch green, unreachable address red within a minute; 8.2 n/a (no HTTPS) |
| V.1 | VSP 4850GTS (VOSS 7.1.10.0): web server, TLS | ok | 2026-11-04: HTTPS only, TLS 1.2 with RSA key exchange and without secure renegotiation (OpenSSL 3 clients refuse it; Go and Chromium connect). `Connection: close` makes the web server reset the connection after the response headers (fixed: `KeepAlive` for `edm-direct`); few web sessions, `503 Server Too Busy` beyond that |
| V.2 | VSP: device kind `edm-direct` | ok after fix | opens directly in the browser; certificate confirmed from the edit page; EDM login stored. Embedded in the workspace the frame stayed empty until the certificate was accepted elsewhere (no warning in frames), and the login then failed with "Authentication Failed" (the `auth` nonce cookie is a third-party cookie in the frame); `edm-direct` now always opens in a tab or window |
| V.3 | VSP: SNMP dashboard data | ok | driver `voss`: 50 front ports 1/1–1/50, VLAN interfaces not on the faceplate, PVID, trunk tagging, speed/duplex, LLDP neighbour, chassis serial and release; down ports showed "half" duplex (fixed) |
| V.4 | VSP: EDM data source | ok after fixes | VOSS login variant (`encode_vrf`, fixed); `ifName`, `ifType`, `ifHighSpeed` unknown to VOSS's EDM (`Oid_Conversion_Fail` truncated the table, fixed); same ports, VLANs, tagging, LLDP and unit as SNMP. MLTs and channelized ports not configured on the test switch |
| V.5 | VSP: device kind `vsp-edm` (EDM through the proxy, embedded) | ok after fixes | 2026-11-04, Chromium 156: auto-login and manual login, navigation tree, Edit › Chassis and VLAN › VLANs, write test (`sysLocation` set, read back, reverted), EDM's *Log out* (stays below `/d/{token}/`), no console errors. Fixed on the way: keep-alive also for proxied VOSS devices; the login failed with "Authentication Failed" because the sandbox has no `localStorage` and the page's fallback cookie went to the switch (shim now provides Web Storage) |
| V.6 | VSP: "Open in EDM" for a port | ok | 2026-11-04: dashboard link `/?open=<id>&port=<ifIndex>` opens "Port 1/6 General", a second link "Port 1/48 General" in the same (reused) proxy session |
| V.7 | VSP 8404 (VOSS 8.10.1.0): SNMP, TLS, EDM login | partly | 2026-11-04: TLS 1.2 ECDHE/AES-GCM (modern profile); SNMP v2c dashboard data ok; EDM login page sets no `auth` cookie before the login (auto-login stopped with "did not set the auth cookie", fixed); the factory default login was then rejected ("Authentication Failed"); with another stored login: auto-login, device view, "Open in EDM" for ports 1/15 and 1/1 (the port dialog needed retries: VOSS 8 ignores a double-click until it has loaded the port data – fixed) |
