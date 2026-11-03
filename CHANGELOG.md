# Changelog

All notable changes to this project are documented in this file.
The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and the project
uses [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Fixed

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
