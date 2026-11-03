# Compatibility

## Switches (EDM through the proxy)

| Model | Firmware | Transport | Status | Verified how |
|---|---|---|---|---|
| ERS 5632FD (single unit) | 6.6.3.015 | HTTP | works | Hardware test 2026-11-03 (Chromium): login, discovery, Switch Summary, Chassis, Bridge, VLANs incl. Ports tab, MLT/LACP, Spanning Tree, Graph → Chassis, table export, open modes embedded, tab and window, logout; auto-login; EDM data source |
| ERS 5600 series (5698TFD(-PWR), 5650TD(-PWR)), stacks | 6.6.3.015 | HTTP | expected to work | Same firmware and EDM files as the verified 5632FD; stacks not yet tested |

Other ERS families with the same EDM (Ext JS 2.2) – for example ERS 3500, 4000/4800, 5500 and
5900 – are expected to need the same patches. Patches match file content, not paths or
versions, so they apply wherever the affected code appears. Please report results with the
"Device report" issue template.

## Port dashboard drivers

The port dashboard picks a driver from `sysObjectID` (highest match wins; `generic` is the
fallback). All drivers are read-only.

| Driver | Detection | Data used | Verification |
|---|---|---|---|
| `generic` | any device | IF-MIB, BRIDGE-MIB, Q-BRIDGE-MIB, LLDP, LAG, PoE (RFC 3621) | synthetic fixtures only |
| `ers` | `1.3.6.1.4.1.45.` | standard MIBs plus RAPID-CITY VLANs, MLT and S5-CHASSIS stack units | ERS 5632FD (one unit, 6.6.3.015): ports, VLAN bitmaps, tagging, LLDP, unit data; numbering of stack units 2–8 not yet verified |
| `cisco` | `1.3.6.1.4.1.9.` | standard MIBs plus CISCO-VTP-MIB (VLANs, trunks) and CISCO-VLAN-MEMBERSHIP-MIB (`vmVlan`); Q-BRIDGE fallback without VTP | synthetic fixtures only |
| `tplink` | `1.3.6.1.4.1.11863.` | standard MIBs plus TPLINK-DOT1Q-VLAN-MIB (port table: PVID, LAG; VLAN table: names, tagged/untagged port lists as text); front-panel ports from `ifName`; Q-BRIDGE fallback | T1500G-8T and Omada SG2210P (firmware 5.20): ports, PVID, tagging, LAG, LLDP; PoE (status, class, power per port, budget) from TPLINK-POWER-OVER-ETHERNET-MIB, POWER-ETHERNET-MIB as fallback. The SNMP agents are slow and drop requests: a poll takes 30–60 s |

Please attach `ers-proxy snmp-capture` output to a device report to get a driver verified.

## Why EDM breaks in modern browsers

EDM's `doAjaxRequest()` reads `response.getResponseHeader.Server`. Ext JS 2.2 builds that map
from `getAllResponseHeaders()`, and modern browsers return header names there in lower case
(`server:`). The lookup yields `undefined` and every AJAX callback throws a `TypeError`; EDM stays
empty after the login or hangs at "Discovering Device". The login and index pages also contain a
browser whitelist (`var supportedBrowser = false;`) that raises an alert in every current
browser.

ERS-Proxy patches the header map so that it also contains lower-case and title-case keys, and
turns the whitelist off. See `internal/edmproxy/patches/`.

## Behaviour of the ERS web server (seen on an ERS 5632FD, 6.6.3.015)

- **HTTPS:** not every installation has it; the tested switch offered HTTP only.
- **One connection per request.** The server closes the connection after every answer, and it
  refuses new connections (closed without answer) for a while after bursts – for example two
  EDM starts within a minute. ERS-Proxy repeats refused reads and keeps images in a per-device
  cache; if EDM still shows "failed in discover…", wait a minute and reopen the device.
- **`Cache-Control: max-age=0`** in a request yields an empty `304 Not Modified`, even for a
  POST; ERS-Proxy does not forward browser cache directives.
- **Login:** a wrong password is answered with `200` and `authError.html` ("Authentication
  Failed!"), not with an error status. After a few failed logins the account is locked for about
  1–2 minutes.
- **EDM bridge:** tables with not-accessible index objects (LLDP) return nothing when the index
  columns are requested; EDM asks for the pseudo column `indexNA` instead, which carries the
  instance ("timeMark.localPort.index").
- **Help:** the EDM help files are not on the switch; they have to be put on a TFTP server (see
  `helpSetup.html` on the switch). Without them, the help window shows an error after about 10 s.
- **SNMP:** `s5ChasComDescr` is a description ("24 SFP plus 8 10Gig XFP …"), `s5ChasComVer` the
  model with hardware revision ("5632FD HW:06"), `s5ChasComRunningSoftwareVer` the software.
  The out-of-band port has ifIndex 55001.

## TLS

The ERS 5600 firmware uses the Mocana TLS stack. Browsers from the time of the firmware
(Firefox 3.x) spoke SSL 3.0 and TLS 1.0, so the switch supports at least TLS 1.0. ERS-Proxy's
legacy profile covers TLS 1.0–1.2 with RSA key exchange and AES-CBC, 3DES or RC4-SHA. Not
supported: SSL 3.0, DHE-only cipher suites, RC4-MD5. `ers-proxy tls-probe https://<switch>`
shows what a switch accepts.

RSA keys below 1024 bits are rejected by Go. If a switch uses one, start ERS-Proxy with
`GODEBUG=rsa1024min=0` (for example in a systemd drop-in) or generate a new key on the switch.

## Browsers

The portal and the proxied EDM are tested with current Chromium. Firefox, Edge and Safari are
expected to work; please report problems.
