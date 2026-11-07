# Architecture

ERS-Proxy is a single Go binary (no CGO) with an embedded web UI and SQLite. It has two faces on
one listener:

- the **portal** (management UI): login, device sidebar, tabbed workspace, settings;
- the **EDM proxy** below `/d/{token}/`: switch content, isolated from the portal.

```
Browser ──HTTPS──> reverse proxy ──HTTP──> ers-proxy :9050
                                             │
              ┌──────────────────────────────┼─────────────────────────────┐
              │ /, /login, /admin/…, /open/{id}   (portal, SecurityHeaders) │
              │ /d/{token}/…                      (edmproxy, sandbox CSP)   │
              │ /health                                                     │
              └──────────────────────────────┬─────────────────────────────┘
                                             │ HTTP or HTTPS (TLS 1.0–1.3, pinned)
                                             ▼
                                      ERS switch web server ("Rapid Logic/1.1")
```

## Packages

| Package | Responsibility |
|---|---|
| `cmd/ers-proxy/cmd` | cobra CLI: `server`, `config init`, `migrate`, `backup`, `user …`, `device …`, `tls-probe`, `version`. `server.go` assembles the router; `proxy.go` connects the proxy to the UI. |
| `internal/config` | `config.yaml`, defaults, validation, `ERSPROXY_*` overrides. `Example` must equal `docs/config.example.yaml` (test). |
| `internal/store` | SQLite (modernc), goose migrations in `migrations/sqlite/`, users, sessions, groups, devices, audit log, `Backup` (`VACUUM INTO`). |
| `internal/secret` | AES-256-GCM with associated data; key file outside the database. |
| `internal/auth` | Argon2id, random tokens, login lockout, rate limiter (from LANLeecher). |
| `internal/app` | Shared services: session cookie and middleware, client IP behind trusted proxies, `SameOrigin`, audit helper, portal security headers (CSP `frame-src` includes link-device origins), request log with redacted tokens. |
| `internal/devices` | Device validation/normalisation (`Normalize`, including the monitoring fields), address helpers, credential encryption bound to device and address (`AAD`), SNMP credential encryption bound to device, host and SNMP port (`SNMPAAD`, `EncryptSNMP`), JSON/CSV import and export. |
| `internal/snmp` | gosnmp wrapper (from LANLeecher): `Credentials` v2c/v3, `Target`, `Dial`, `WalkColumn`, value helpers. Read-only: no SET API (a test parses the sources). |
| `internal/snmptest` | Test helper: starts snmpsim (`$ERSPROXY_SNMPSIM`, `.venv`, or PATH) on `testdata/snmpsim/*.snmprec`; tests skip when it is missing or with `-short`. CI installs it with pip. |
| `internal/monitor` | Port dashboard model: `Snapshot`, `Port`, `Reader` (objects by MIB name), `Driver`/`Registry`, `CollectBase` (IF-MIB, EtherLike, LAG, LLDP, PoE), `Objects` (name → OID), `Rates` (Counter64 deltas, wrap/reset/discontinuity) and `Diff` (port events). Read-only; `TestReadOnly` enforces it. See [drivers.md](drivers.md). |
| `internal/monitor/drivers` | `generic` (Q-BRIDGE), `ers` (`rcVlan*`, MLT, stack units, 128/64 ifIndexes per unit), `cisco` (VTP, `vmVlan`, trunks), `tplink` (TPLINK-DOT1Q-VLAN-MIB, ports by `ifName`, Q-BRIDGE fallback), `voss` (VSP/VOSS: slot/port from `ifName`, RAPID-CITY VLANs, port table and MLT shared with `ers`; tagging ports with `rcVlanPortUntagDefaultVlan` set are hybrid: PVID untagged, the rest tagged), `swos` (MikroTik SwOS: standard MIBs and MIKROTIK-MIB serial/firmware; VLANs and LAGs through `swossrc`), `grandstream` (Grandstream GWN: front-panel number from `ifName`, model/firmware from vendor scalars; VLANs, duplex, LLDP and LAGs through `gwnsrc` when the `gwn` source is used). |
| `internal/swos` | Client for MikroTik SwOS's web API: GET of `sys.b`, `link.b`, `fwd.b`, `vlan.b`, `lacp.b`, `stats.b` only (allow-list), a parser for its JavaScript object literals (hex numbers, hex-encoded strings, bitmaps), `State` with ports (SwOS's labels), VLAN settings and table, trunks and 64-bit counters. `swostest` is a fake SwOS web server with Digest login for tests. |
| `internal/monitor/swossrc` | `Reader` over `internal/swos`: loads the state once per poll and serves it under standard object names (system group, IF-MIB, EtherLike duplex, BRIDGE port map, IEEE8023-LAG with aggregate ID 128 + trunk, Q-BRIDGE with tagging derived from "VLAN Receive" and the default VLAN). |
| `internal/gwn` | Client for the web API of Grandstream GWN switches (the API of its web interface): `GET /get.cgi?cmd=…` and `POST /set.cgi?cmd=…`, JSON `{code,msg,data}`, code 401 = session ended, paged lists (`list`, `remain`). Login: `get_nonce`, then `login` with `hex(SHA-256("<user>:<nonce>:<password>"))`, token in the `Authorization` header. Sends only an allow-list of read commands and POSTs only `login` and `logout`, never a setting. One web session per account: logs in, reads and logs out per poll; `ErrBusy` (locked, in use) is temporary, `ErrLogin` (rejected) is not retried until the login or address changes. Always reads answers to the end, because a TCP reset on port 443 reboots the switch (firmware 1.0.7.128). `gwntest` is a fake GWN web server for tests (one session per account, paging, records write attempts). |
| `internal/monitor/gwnsrc` | `Reader` that serves the GWN web state under standard object names (system group, IF-MIB without counters, duplex, bridge port map, IEEE8023-LAG with aggregate ID 1000 + LAG, Q-BRIDGE PVID/names/port lists, LLDP) and, with SNMP credentials stored, reads counters and everything the web API lacks from SNMP; a failing SNMP read becomes a note (`SourceNotes`). Wired in `cmd/ers-proxy/cmd/gwnsource.go` (one `gwn.Client` per device, cached by address, TLS, pin and login hash). |
| `internal/monitor/snmpsrc` | `Reader` over SNMP (GET/GETBULK only, at most 4 objects per GET because GWN agents drop larger requests) and `Capture` (anonymised `.snmprec` for device reports, `ers-proxy snmp-capture`). |
| `internal/edmsession` | The logged-in EDM session of one switch (transport only): login on demand, re-login once when the session expired, no retry after a rejected login, `Logout`. There is exactly one per device; every user of the device's EDM shares it, because VSP switches allow only about five web sessions. |
| `internal/monitor/edmsrc` | `Reader` over EDM's `/jsp/getFormMibs.jsp` and `/jsp/getTableMibs.jsp` on the shared `edmsession.Session` of the switch (`Session.EDM()`) (experimental); `Tables` lists index objects per table. Names the switch answers with `Oid_Conversion_Fail` are dropped for the session and the table is asked again; `Session.Logout` ends a replaced session. |
| `internal/monitor/poller` | One loop per monitored device: open reader (SNMP or EDM), detect driver, collect, rates, events, hand to the `Sink`; restarts a loop when the device's settings change. For configuration changes: `Hold` (a per-device gate, so no poll runs while a change writes and reads back), `PollNow`, `CollectNow` (driver + collect through another reader) and `OpenSNMP` (SNMP reader with given credentials). `OpenGWN` opens the `gwn` source. |
| `internal/switchcfg` | Switch configuration (phase 8, see [switch-configuration.md](switch-configuration.md)); the only package that writes to switches, and only when `configure.enabled` is set in `config.yaml`. Model (`Change`, `Assignment`, `Step`, `Plan` with inverse and expected values, `Hash`), `Writer`, `Planner`/`Planners`, `Verify` (read-back), and the `Manager` ("commit confirmed": preview bound to a plan hash, per-device lock, transaction stored before the first write, read-back, pending with a stored deadline, confirm with compare-and-swap, revert, retries with backoff, `Recover` after a restart). Wired in `cmd/ers-proxy/cmd/configure.go` only when `configure.enabled` is set (`web.Server.Configure` stays nil otherwise, and no configuration route exists). A planner that cannot read the IP address table (EDM) warns `mgmt_unknown` instead of trusting that no management VLAN is affected. `switchcfg.Objects` holds the write-only objects, including `rcSysActionL1` (VOSS save). |
| `internal/switchcfg/rcplan` | Planner for RAPID-CITY switches (`VOSS()`; ERS later): `rcVlanTable` (RowStatus create/destroy, name, PortSet members), PVID and tagging per port, `ifAdminStatus`, `ifAlias`, `pethPsePortAdminEnable`; hybrid ports on VOSS (`PortVLANs.Hybrid`, `Capabilities.Hybrid`: tagging plus `rcVlanPortUntagDefaultVlan`; refused when that object cannot be read); the VOSS planner saves on confirm (`rcSysActionL1` = 7, `SaveKnown = true`); guards (uplink, LAG, management VLAN by `ipAdEntIfIndex`, VLAN with members); the inverse is computed as the transition from the new state back to the old one. |
| `internal/switchcfg/switchcfgtest` | In-memory switch for tests: `monitor.Reader` and `switchcfg.Writer` over the same objects, RowStatus handling, failure injection (rejected write, unreachable, ignored writes). |
| `internal/switchcfg/edmwrite` | EDM writer: one `POST /jsp/setFormMibs.jsp` (`command=setVariables`, `varNames`/`varValues` joined by TAB, `varCount`) per step on the device's shared `edmsession.Session`, never retried; PortSets as `0x..:..` hex (empty `0x00:00`), numbers as decimal, text as text (empty text as `0x00:`, because an empty value hangs VOSS's web server); `snmpErrorStatus:<n>` (n ≠ 0, with `snmpErrorIndex` and the standard status name) and `owdErrorStatus:` answers become `*SetError`. Verified on a VSP 8404 in step 8.1. |
| `internal/switchcfg/snmpwrite` | SNMP writer: one SET PDU per step over `snmp.Dial` with retries off (a lost answer must not send a second SET); OIDs from `switchcfg.OID`; error status/index become `*SetError`; the only place in the repository that calls gosnmp's `Set` (`TestSetOnlyHere`). |
| `internal/edmlogin` | Server-side EDM login shared by the proxy's auto-login and the EDM data source. `Style` tells ERS (`encode_ers2500`: XHR check, length-prefixed `encoded`) from VOSS (`encode_vrf`: one form POST with `<user>:<password>`) by the login form. A server error (HTTP 5xx, e.g. VOSS's `503 Server Too Busy`) is `ErrUnavailable`, not `ErrRejected`, so the session tries again later instead of stopping until new credentials are stored. |
| `internal/monitor/history` | `Recorder` (a `poller.Sink`): 1-minute samples per physical port/LAG with error deltas, port events, `Run` maintenance every 5 min (roll-up 60→900→3600 s, retention from `monitor.retention.*`), `Resolution`/`Series` for charts. |
| `internal/web` | Portal handlers and templates (`html/template`, `static/app.js`, no inline code), i18n (en/de). Interfaces to the proxy (`Opener`), health (`StatusSource`) and diagnostics. |
| `internal/web` (monitoring pages) | `/dashboard` (cards, `static/dashboard.js`, `/ui/monitor.json`), `/dashboard/{id}` (switch page: faceplate, sortable port and VLAN tables, events) and `/dashboard/{id}/port/{ifindex}` (port fields, events, SVG traffic charts rendered server-side in `chart.go`; hover and sorting in `static/switch.js`). Data comes from the `MonitorView` and `HistoryView` interfaces; 404 for unknown or unmonitored devices and ports. |
| `internal/web` (configuration) | `configure.go` (`Configurator`, the manager's interface) and `configure_ui.go`: routes below `/dashboard/{id}/configure/` (preview, preview page, apply, confirm, revert) and `/admin/devices/{id}/snmp-write`, registered only when `Server.Configure` is set, inside the admin group (role, CSRF, same origin). Port settings form on the port page, VLAN forms and change history on the switch page, banner (`config_banner` in `layout.html`) for pending, failed and unsaved changes; template `config_preview.html`. |
| `internal/upstream` | Connection to one device: `http.Transport` with TLS profiles (`modern`, `legacy`), automatic fallback, pinning (`VerifyConnection`), tolerant status line (`fixConn`), `Probe` for confirmation and `tls-probe`. `Client.Transport` repeats reads (GET/HEAD and requests marked with `WithRetry`) up to six times (about 16 s) when the switch drops the connection quickly without an answer – its web server has only a few connection slots for all clients together. `Options.KeepAlive` (set for VOSS devices: `vsp-edm`, `edm-direct`) sends `Connection: keep-alive` instead of `close`, because VOSS resets connections it is asked to close and loses the body; connections are still not reused. `Options.Login` (set for `swos` devices with a stored login) wraps the transport in `digestTransport` (`digest.go`): it answers HTTP Digest challenges (MD5/SHA-256, `qop=auth`) and signs later requests in advance with the last nonce; a request whose signature was refused with the same nonce is not repeated (wrong login). |
| `internal/edmproxy` | Proxy sessions (token registry, cookie jar), request/response pipeline, isolation headers, cookie shim, diagnostics per device. Serves EDM (ERS, VSP) and MikroTik SwOS (`swos`: the upstream client logs in with the stored web login; without one `/open/{id}` shows a message). |
| `internal/edmproxy/patches` | Content-based patches for EDM and HTML script injection. |
| `internal/health` | Periodic reachability checks; certificate change detection; framing check for link devices. |
| `internal/mockswitch`, `tools/mockswitch` | Fake ERS web server for tests (synthetic files) and local browser tests (real webroot from outside the repository). With `Options.Data` it applies `setVariables` to its data (RowStatus create/destroy for `rcVlanTable`, errors as `snmpErrorStatus`), `SetError` injects answers, and `Options.MaxSessions` answers further logins with `503 Server Too Busy`. |
| `tools/e2e` | Selenium test of EDM through the proxy (local only). |

## Opening a device

1. The sidebar link points to `/open/{id}` (ers-edm, vsp-edm, swos) or the device URL (link, edm-direct and gwn,
   `Device.Direct()`). `app.js` decides by the effective open mode (device → user →
   `ui.default_open_mode`): an iframe tab, a new browser tab, or a popup. `edm-direct` and `gwn` devices
   never get an iframe tab (`Device.AlwaysTab()`) (`EffectiveOpenMode` turns `embedded` into `tab`): in a frame the
   switch is a third party, so the browser drops its login cookie and shows no certificate
   warning. Links that cannot be framed (plain HTTP in an HTTPS portal, framing ban seen by the
   health check) fall back the same way.
2. `/open/{id}` requires a session and refuses `Sec-Fetch-Site: cross-site`. Direct devices are
   redirected to their URL. For proxied HTTPS devices without a pin it renders the certificate
   confirmation page (`trust.html`, frameable). Edm-direct devices are never opened through
   ERS-Proxy, so their certificate is confirmed from the edit page
   (`/admin/devices/{id}/trust`) before the EDM data source can connect.
3. `edmproxy.Open` creates a proxy session: a 128-bit token bound to the login session and the
   device, with its own cookie jar. Redirect to `/d/{token}/`. A session of the same login
   session for the same device that was used in the last 10 minutes and not logged out of EDM
   (`/logout.html` seen) is reused instead, so a workspace reload or "Open in EDM" does not
   log in at the switch again (VSP switches allow only a few EDM sessions). `?port=<ifIndex>`
   ("Open in EDM" for a port) is stored in the session's page storage (`x:port`); the shim
   then selects `Port_<ifIndex>` in EDM's device view and opens its dialog with a click and a
   double-click (on ERS it first brings the device view's tab to the front). It repeats the
   double-click every 2 s until a dialog tab appears, because EDM draws the ports before it
   has loaded what the dialog needs (VOSS 8 ignores an early double-click).

## Proxy pipeline (`edmproxy.Proxy.ServeHTTP`)

1. Resolve the token; end the session if idle (`proxy.idle_timeout`) or if the login session is
   gone (checked every 30 s).
2. Answer locally: CORS preflight (`OPTIONS`), `__ersproxy/shim.js`, `__ersproxy/cookie`
   (cookies written by page script), `__ersproxy/storage` (snapshots of the page's Web Storage,
   see below).
3. Otherwise forward with `httputil.ReverseProxy`:
   - **request:** strip the `/d/{token}` prefix, set `Host`, drop browser cookies, `Origin`,
     `Referer`, forwarding and fetch-metadata headers, `Accept-Encoding` and the browser's cache
     directives (`Cache-Control`, `Pragma` – the switch answers every request with
     `Cache-Control: max-age=0` with an empty 304, even a POST); send the jar's cookies. Bodies
     of `/jsp/` POSTs are buffered; read commands (`getVariables`, `getNextVariables`,
     `getTableVariables`) are marked with `upstream.WithRetry`. Images (GET without query) are answered
     from the per-device image cache (`imgcache.go`, filled from earlier responses, dropped by
     `ForgetDevice`);
   - **response:** move `Set-Cookie` into the jar, rewrite `Location` into the proxy path, put a
     non-standard reason phrase into `X-ERSProxy-Status-Text`, set the isolation headers; for
     documents and scripts: gunzip, apply patches, inject the shim into documents
     (`Sec-Fetch-Dest` decides, so `/jsp/` XHR answers stay untouched).
4. Before the reverse proxy writes, previously set headers are cleared – it adds upstream
   headers with `Add`, and a duplicated `Access-Control-Allow-Origin` breaks every XHR.

Isolation headers (sandbox mode): `Content-Security-Policy: sandbox allow-scripts allow-forms
allow-popups allow-modals allow-downloads; frame-ancestors 'self'`,
`Access-Control-Allow-Origin: null`, `Access-Control-Expose-Headers: *` (EDM reads `Server`),
`Referrer-Policy: no-referrer`, `X-Content-Type-Options: nosniff`.

Shim (`shim.go`, injected into every proxied document): `document.cookie` from the jar (writes
go to the jar), the reason phrase helper, the export/print window stand-in, and – only where
the sandbox blocks Web Storage – `localStorage`/`sessionStorage`. These keep their data in the
proxy session (`pageStorage`, at most 64 KiB, newest snapshot wins, never sent to the switch).
Without them VOSS's login page falls back to a `storeEDMUser` cookie, which the jar would send
to the switch, and the switch rejects the login. For `swos` devices the shim also maps
absolute `XMLHttpRequest` paths (`/link.b`) below `/d/{token}/`; SwOS's page loads all its data
that way.

Request and response headers of logins: the browser's `Authorization` is removed from every
proxied request, and `WWW-Authenticate` from every response, so the browser never shows a
login dialog for a switch; logins are the proxy's job (cookie jar, stored login).

## EDM specifics

- Login: `GET /` returns `login.html` and `Set-Cookie: auth=<nonce>`. The page reads the nonce
  via `document.cookie` (shim), checks the login with a synchronous XHR `POST /` (body
  `encoded=<len2><user>:<pass>&nonce=…`, sent as `text/plain`), then submits the form to `/`.
  A `205` answer with reason "Change Password" asks for a new password.
- Data: `POST jsp/getFormMibs.jsp`, `jsp/getTableMibs.jsp`, `jsp/setFormMibs.jsp` with
  `userId=<auth cookie>`; Ext JS adds `X-Requested-With` (hence the preflight).
- The browser fix: Ext JS 2.2 header map (`ext-header-map` patch). Further patches keep the
  page inside `/d/{token}/` (absolute `/` paths).
- Export and print of tables: `window.open('')`, then `document.write`. From the sandbox, under
  the portal's `Cross-Origin-Opener-Policy`, `window.open` returns `null`; the shim returns a
  stand-in that collects the HTML and then opens the window with it as a Blob URL (still
  sandboxed).
- Help: EDM opens `/help/<page>.html`. The switch does not contain the help files (they have to
  be put on a TFTP server, see `helpSetup.html` on the switch); it answers such requests with an
  EOF after about 10 s, which the proxy shows as a 502 page.
- Web server behaviour seen on an ERS 5632FD (6.6.3.015): one connection per request (it closes
  after every answer), `Cache-Control: max-age=0` in a request yields an empty 304 even for a
  POST, `If-Modified-Since` is ignored, and after bursts of connections it refuses new ones (EOF
  within milliseconds) for a while.

## Data model

Migration `00001_init.sql`: `users` (role `user`/`admin`, language, theme, default open mode),
`sessions` (SHA-256 of the cookie, CSRF token, sliding 12 h expiry), `device_groups`,
`devices` (kind `ers-edm`/`vsp-edm`/`edm-direct`/`swos`/`gwn`/`link` – a TEXT column, so new kinds need no migration; URL, open mode, isolation, TLS mode/pin/profile, encrypted credentials,
auto-login flag, health flag, notes, sort), `audit_log`. Proxy sessions, cookie jars, health
state and diagnostics live in memory only.

Migration `00002_monitoring.sql` adds to `devices`: `monitor_source` (`none`/`snmp`/`edm`; `swos` and `gwn`
need no migration, the column is TEXT),
`monitor_driver`, `monitor_interval` (seconds), `snmp_version`, `snmp_port` and
`snmp_credentials` (AES-GCM blob, AAD `ers-proxy snmp|<id>|<host>|<snmp_port>`).

Migration `00003_history.sql`: `port_samples` (`device_id`, `if_index`, `res` = 60/900/3600 s, `ts` = bucket
start; in/out average and maximum, `util_max`, error deltas, `up_frac`, `n` = raw samples; primary
key `(device_id, if_index, res, ts)`, `WITHOUT ROWID`, index `(res, ts)` for pruning) and `port_events`
(`device_id`, `if_index`, `port`, `at`, `kind`, `from_value`, `to_value`). Both cascade when a device is
deleted. Roll-up replaces whole buckets (averages weighted by `n`), so it is idempotent.

Migration `00004_configure.sql` adds to `devices`: `config_source` (`none`/`edm`/`snmp`, the
channel for configuration changes) and `snmp_write_credentials` (AES-GCM blob, separate from the
read-only SNMP credentials). Table `config_changes`: one row per configuration change (`device_id`,
`user_id`, `actor`, `created_at`, `deadline`, `finished_at`, `state`, `summary`, `plan` = JSON
of the `switchcfg.Plan`, `error`); it cascades with the device and is also the change history.
States: `applying` → `pending` → `confirmed`/`confirmed_unsaved`, or `reverting` →
`reverted`/`revert_failed`; `failed` when the apply itself was reverted. Transitions are
compare-and-swap updates (`TransitionConfigChange`).

## Security mechanisms

See [security.md](security.md). In code:

- portal: `app.SecurityHeaders`, `app.SameOrigin`, `web.csrf`, `web.requireRole`,
  `SameSite=Strict` session cookie (`app.SessionCookieAttrs`), re-authentication for sensitive
  actions (`web.confirmPassword`);
- proxy: `edmproxy.isolationHeaders`, token registry, server-side cookie jar;
- devices: `devices.AAD` (credentials bound to device and address), pin and credentials are
  cleared when the address changes (`web.deviceUpdate`); `devices.SNMPAAD` (SNMP credentials
  bound to device, host and SNMP port; cleared when either changes);
- upstream: `upstream.TLSConfig` (pinning instead of chain validation).

## Tests

- Unit and integration tests per package (`go test ./...`), including the proxy against
  `internal/mockswitch` with synthetic EDM files, legacy TLS 1.0 servers, malformed status lines,
  CSRF/Origin rejection and sandbox headers.
- Browser test against a real webroot: `tools/e2e/edm_e2e.py` (see its header).
