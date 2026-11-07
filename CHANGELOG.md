# Changelog

All notable changes to this project are documented in this file.
The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and the project
uses [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- Grandstream GWN switches (verified read-only on a GWN7721, firmware 1.0.7.128): device kind
  `gwn` (the browser opens the switch's web interface directly in a tab; the stored login is
  the web login), driver `grandstream` and data source `gwn`, which reads VLANs, duplex, LLDP
  and LAGs from the switch's web API (read commands only, one login per poll because the switch
  allows one web session per account) and counters from SNMP. Use an `http://` address: on the
  tested firmware a TCP reset on the HTTPS port reboots the switch, so never port-scan GWN
  switches.
- Changing switch settings from the dashboard (phase 8, experimental, VSP/VOSS only, verified
  on one VSP over EDM only; see `docs/switch-configuration.md`). Off unless `configure.enabled:
  true` is set in `config.yaml` and the device has a configuration channel (EDM or SNMP);
  admins only. Port VLANs, port state, port name, VLANs and PoE; every change is previewed,
  applied "commit confirmed" and reverted automatically unless it is confirmed in time.
  Change history per switch, audit log entries.
- `configure.enabled` and `configure.confirm_timeout` in `config.yaml`. Writing to switches is
  off by default and can only be turned on in the configuration file; no environment variable
  enables it.
- Migration `00004_configure.sql`: `devices.config_source`, `devices.snmp_write_credentials` and
  the table `config_changes`.
- Writers for phase 8: over EDM (`setVariables`, formats from EDM's own scripts,
  tested against the mock switch) and over SNMP SET (tested against snmpsim), plus encrypted
  SNMP write credentials that are cleared when the switch's address or SNMP port changes.
- Phase 8 verified on a VSP 8404 with VOSS 8.10.1.0 over the EDM channel (2026-11-06): port
  VLANs, port name and state, VLAN create/rename/delete, confirm, revert, automatic revert
  after the deadline and after a restart. SNMP channel and VOSS 7.1 still to be tested.
- Hybrid ports for VOSS in the configuration planner (`PortVLANs.Hybrid`; the port form's mode
  "hybrid" needs an untagged VLAN).
- A confirmed change on VOSS is now saved to NVRAM (`rcSysActionL1` = 7) and ends as
  `confirmed`, not `confirmed_unsaved`.

### Changed

- EDM session handling moved from `internal/monitor/edmsrc` to `internal/edmsession`, so the
  dashboard's EDM data source and future configuration changes share one session per switch.
- The device list has a single *Add switch* button instead of *Add ERS switch* and *Add link*;
  the kind is chosen in the form.
- The new-device form takes the switch login (auto-login, SwOS web login, EDM/SwOS data source)
  together with SNMP access, confirmed with one password entry.

### Fixed

- SNMP GET requests now carry at most 4 objects (was 10), for agents that silently drop larger
  requests (Grandstream GWN).
- A busy switch during the server-side EDM login (HTTP 5xx, such as VOSS's `503 Server Too
  Busy` when all web sessions are taken) counted as a rejected password: the dashboard's EDM
  data source then stopped logging in until the login was stored again. It is now a temporary
  error and the next poll tries again.
- Setting an empty port name over EDM hung the VSP's web server: empty text is now sent as
  `0x00:`, as EDM does. Port and VLAN names starting with `0x` are refused.
- Hybrid ports on VOSS (tagging with "untag default VLAN") showed as trunks on the dashboard;
  the VOSS driver now reads `rcVlanPortUntagDefaultVlan` and shows them as hybrid.
- EDM write errors now include the failed variable (`snmpErrorIndex`) and the name of the
  standard SNMP error status.

- "Open in EDM" for a port left `TypeError` messages in the console on VOSS 8 while EDM was
  still loading; the shim now calls EDM's click handlers directly and keeps such early failures
  to itself while it retries.
- Auto-login and the EDM data source stopped at VOSS 8 ("the switch did not set the auth
  cookie"): its login page sets no nonce cookie. For VOSS the login now goes ahead with an
  empty nonce, as the switch's own login form does; ERS still requires the cookie.
- Opening a proxied device again (workspace reload, "Open in EDM" from the dashboard) logged in
  at the switch every time and left the old EDM session behind; VSP switches answer `503 Server
  Too Busy` after a few. A proxy session of the same portal login used in the last 10 minutes
  is now reused, unless EDM was logged out in it.
- `edm-direct` devices (VSP/VOSS) no longer open embedded in the workspace, but in a new tab
  (or the window mode). In a frame the switch is a third party: the browser showed no
  certificate warning (the frame stayed empty) and dropped the switch's `auth` login cookie, so
  the login failed with "Authentication Failed". Storing an EDM login for such a device no
  longer turns on auto-login, which only proxied devices have; the device form says so.
- Ports without link showed "half" duplex on ERS and VSP switches (`rcPortOperDuplex` reports
  half duplex for them); duplex from the RAPID-CITY port table is now only shown for ports that
  are up.
- EDM login through the proxy did nothing after clicking *Log On* on a real ERS 5600: browsers
  send `Cache-Control: max-age=0` with the form submit, and the switch answers every such
  request with an empty `304 Not Modified`, even a POST. The proxy no longer forwards the
  browser's `Cache-Control` and `Pragma` headers (found in the hardware test, step 6.1).
- Missing images, empty EDM dialogs and failed automatic logins while two EDM sessions (or a
  session and the EDM data source) load at the same time: the switch closes surplus connections
  without an answer. The upstream client now repeats reads (GET/HEAD and EDM read commands, never
  `setVariables`) up to six times within about 16 s, but only when the switch refused the
  connection quickly (a missing file such as `/help/…` ends in a slow EOF and is not repeated).
- Table *Export* and *Print* in EDM opened an empty window: a window opened from the sandbox
  (under the portal's Cross-Origin-Opener-Policy) cannot be scripted. The shim now collects what
  EDM writes into such a window and opens it with that content as a Blob URL; the window stays
  sandboxed.
- Auto-login and the EDM data source took a wrong stored password for a successful login: the
  switch answers it with `200` and `authError.html` ("Authentication Failed!"). The login now
  recognises that page; the mock switch can emulate it (`Options.Password`).
- The EDM data source found no LLDP neighbours on a real switch: the LLDP index objects are
  not-accessible, so the reader now asks for EDM's pseudo column `indexNA` like EDM itself; the
  mock switch answers like the switch (empty table for explicit LLDP index columns).
- The EDM data source no longer logs in again after the switch rejected the stored login (until
  the login is changed): ERS switches lock the account after a few failed logins, and a poll every
  minute would have kept it locked.
- TP-Link driver (step 7.10, from real T1500G-8T and Omada SG2210P): these switches have no
  Q-BRIDGE-MIB and report ifType 6 for VLAN interfaces and port channels. The driver now reads
  TPLINK-DOT1Q-VLAN-MIB (PVID, tagged/untagged port lists, LAG membership) and numbers ports by
  `ifName` ("gigabitEthernet 1/0/5" = 1/5) instead of showing "1/49153" and a VLAN interface as
  port. The preliminary note is gone; Q-BRIDGE stays as fallback (`tplink-qbridge.snmprec`).
- TP-Link PoE: the SG2210P does not answer POWER-ETHERNET-MIB; the driver reads
  TPLINK-POWER-OVER-ETHERNET-MIB (port status, class, power, budget) and uses POWER-ETHERNET-MIB
  only as fallback (`BaseOptions.SkipPoE`, `Base.ReadPoE`), which also shortens polls of the slow
  agents (105 s → 39 s on an SG2210P).
- The dashboard's port panel shows the PoE power of a port when the switch reports it.
- Polls longer than the interval ran back to back; the poller now pauses at least a quarter
  interval.
- `snmp-capture` stopped at the first object a device could not answer (a timeout, an LLDP value
  gosnmp cannot decode); such objects are now skipped and listed.
- The dashboard kept the old name of a renamed device until its monitoring settings changed.
- ERS driver: unit model and firmware came from the wrong columns on real hardware
  (`s5ChasComDescr` is a description, `s5ChasComVer` the model with hardware revision). Model now
  comes from `s5ChasComVer`, firmware from `s5ChasComRunningSoftwareVer`.
- ERS driver: the out-of-band management port (ifIndex 55001) appeared as front-panel port
  "430/89". Interfaces beyond the largest stack (8 units) are no longer counted as front-panel
  ports; the synthetic `ers5600` fixture now contains such a port.

### Added

- MikroTik SwOS switches (step 7.14), verified read-only on a CRS305-1G-4S+ with SwOS 2.18:
  - Device kind `swos`: SwOS's web interface embedded through the proxy. ERS-Proxy logs in with
    the stored web login (HTTP Digest, `upstream.Options.Login`); the browser never sees the
    password or a login dialog. The shim maps SwOS's absolute XHR paths below the proxy path.
  - Port dashboard driver `swos` (detected by `sysObjectID` 1.3.6.1.4.1.14988.2): ports, speed and
    traffic over SNMP, model, serial number and firmware from MIKROTIK-MIB.
  - Data source `swos` (`internal/swos`, `internal/monitor/swossrc`): reads SwOS's web API (GET
    of six read-only files) for VLANs, PVID, tagging, duplex and LAGs, which SwOS's SNMP agent
    does not provide. Port names follow SwOS (`ETH/BOOT`, `SFP1`, …).
- The proxy no longer forwards the browser's `Authorization` header to any switch and drops
  `WWW-Authenticate` from switch answers.

- Browser test `tools/e2e/edm_e2e.py` also runs in Firefox (`E2E_BROWSER=firefox`, geckodriver;
  JavaScript errors through WebDriver BiDi) and WebKitGTK, Safari's engine (`E2E_BROWSER=webkit`,
  webkit2gtk-driver, under `xvfb-run`). Verified with Firefox ESR 153 and WebKitGTK 2.54 against
  the mock switch and on the VSPs.
- SNMPv3 integration tests against snmpsim (`snmptest.SimulatorV3`): noAuthNoPriv and
  authNoPriv with MD5, SHA and SHA-256, and a wrong password. authPriv cases are skipped because
  snmpsim cannot decrypt them (`ERSPROXY_SNMPSIM_PRIV=1` runs them).
- SNMP access can be entered when a device is added. The SNMP version (v2c/v3) is now chosen in
  the SNMP access card together with the credentials instead of being saved first in the main
  form; the card shows only the fields of the chosen version and v3 security level.
- "Open in EDM" for a port (dashboard panel and port page) now opens that port: EDM selects it
  in the device view and opens its edit dialog (verified on a VSP 4850GTS; ERS through the same
  `Port_<ifIndex>` elements, not yet verified on hardware). Verified on VOSS 7.1 and 8.10; the
  double-click is repeated until the dialog appears, since VOSS 8 ignores it while still
  loading the port data.
- Device kind `vsp-edm`: a VSP switch with VOSS whose EDM runs through the proxy like an ERS –
  embedded in the workspace, with auto-login and the certificate pinned on the server (verified
  on a VSP 4850GTS with VOSS 7.1.10.0). `edm-direct` remains for opening a VSP directly.
- The shim provides `localStorage` and `sessionStorage` in the sandbox, kept per proxy session
  and never sent to the switch. VOSS's login page needs them: without, it stored the user name
  in a cookie that reached the switch, and the login failed.
- Extreme VSP switches with VOSS (verified on a VSP 4850GTS, VOSS 7.1.10.0):
  - New device kind `edm-direct` ("EDM switch, opened directly"): the browser opens the switch
    by its URL like a link (VOSS's EDM works in current browsers), while TLS mode, certificate
    pin, stored login and the EDM data source work as for `ers-edm`. The certificate is confirmed
    from the device's edit page (*Check and confirm certificate*). No migration needed.
  - Port dashboard driver `voss` (`sysObjectID` 1.3.6.1.4.1.2272.): front-panel slot/port from
    `ifName`, RAPID-CITY VLANs, PVID and tagging, speed/duplex from `rcPortTable`, MLT, chassis
    serial and release. Synthetic `voss.snmprec`.
  - EDM login in the VOSS variant (`encode_vrf`: one form POST, no length prefix, no
    15-character limit), detected from the login page.
  - `upstream.Options.KeepAlive`: requests to VOSS devices (`edm-direct`, `vsp-edm`) say
    `Connection: keep-alive`; VOSS's web server resets connections it is asked to close and the
    answer is lost.
  - EDM data source: object names the switch cannot resolve (`Oid_Conversion_Fail`; VOSS lacks
    `ifName`, `ifType`, `ifHighSpeed`) are left out for the session instead of truncating the
    table to one row; a replaced session is logged out (`/logout.html`), since VOSS allows only a
    few web sessions.

- Image cache in the EDM proxy: images of a device are kept in memory (up to 8 MiB per device)
  and served from there on later EDM starts. Each start used to fetch a few hundred images again
  because the proxy path changes with every token.

- Port dashboard (phase 7): common snapshot model, driver registry with auto-detection and the
  drivers generic (Q-BRIDGE) and ERS (RAPID-CITY VLANs, MLT, stack units) (steps 7.2, 7.3);
  poller with traffic rates, counter-wrap/reset handling and port events (step 7.4); dashboard
  overview with faceplates, VLAN highlight, search and port panel (step 7.6); experimental EDM
  data source for ERS switches without SNMP (step 7.8); `ers-proxy snmp-capture` and
  `snmp-test --collect`; documentation `docs/monitoring.md` and `docs/drivers.md` (step 7.11).
- Switch and port pages (step 7.7): `/dashboard/{id}` with header, faceplate, sortable port table,
  VLAN table and recent events; `/dashboard/{id}/port/{ifindex}` with all port fields, port events
  and server-rendered SVG traffic charts (1h, 24h, 7d, 30d, 1y; in/out, peaks for coarse
  resolutions, errors per interval) with hover tooltips. Dashboard card titles link to the switch
  page, the port panel has a "Details" link.
- Cisco and TP-Link port dashboard drivers (steps 7.9, 7.10): Cisco reads VLANs, access VLAN
  and trunks from CISCO-VTP-MIB and CISCO-VLAN-MEMBERSHIP-MIB (Q-BRIDGE fallback); the TP-Link
  driver is preliminary and uses Q-BRIDGE. Synthetic `cisco.snmprec` and `tplink.snmprec`.
- Port history (step 7.5): migration `00003_history.sql` (`port_samples`, `port_events`),
  `internal/monitor/history` recorder with roll-up (1 min, 15 min, 1 h) and retention job, config
  section `monitor.retention` (`minute`, `quarter`, `hour`, `events`).
- SNMP foundation (step 7.1): read-only `internal/snmp` (v2c/v3), encrypted SNMP credentials
  bound to device, host and SNMP port, migration `00002_monitoring.sql` and "Monitoring"
  fields in the device form and in import/export, "SNMP access" card, `snmp-test` command,
  snmpsim test harness (`internal/snmptest`; CI installs snmpsim).
- Repository documents, roadmap and documentation skeleton (step 0.1).
- Go module, command line (`version`, `config init`) and YAML configuration (step 0.2).
- CI, security scans (CodeQL, dependency review, govulncheck, gitleaks), dependabot and
  issue templates (step 0.3).
- SQLite store with migrations, `migrate` and `backup` commands (step 1.1).
- AES-256-GCM encryption of credentials bound to device and host (step 1.2).
- Users with roles `admin` and `user`, Argon2id passwords, sessions, CSRF protection and
  login lockout; `user` commands (step 1.3).
- Web UI foundation: login, account page, security headers, English and German (step 1.4).
- Packages (deb, rpm, Arch, apk), Docker image and release workflow with install tests
  (step 1.5).
- Groups and devices (ERS switches through the proxy, other devices as links), sidebar with
  status dots and filter, tabbed workspace, open modes per user and device, JSON/CSV import and
  export, `device` commands (phase 2).
- EDM proxy: TLS 1.0–1.3 with automatic legacy fallback and certificate pinning, proxy sessions
  with server-side cookie jar, browser sandbox for switch content, content-based patches for
  Ext JS 2.2 and absolute paths, diagnostics page, `tls-probe` command, `tools/mockswitch`
  and a browser test (phase 3).
- Health checks: reachability, certificate changes, framing detection for links (phase 4).
- Experimental automatic EDM login with stored, encrypted credentials (phase 5).
