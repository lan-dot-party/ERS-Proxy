# Switch configuration (phase 8)

_Status: in progress on the branch `switch-config`. Steps 8.2–8.10 are implemented and run end
to end against the mock switch (preview, apply, confirm, revert, automatic revert after the
deadline). 8.1 (recording) is done on a VSP 8404 with VOSS 8.10.1.0 (2026-11-06), which also
verified 8.6 (EDM writer) on hardware. 8.11 (hardware test) has a first run over the EDM
channel on the same switch; open: the SNMP channel and VOSS 7.1 (checklist: section 9 of
[hardware-tests.md](hardware-tests.md)). 8.12 (documentation and rules) is done. Without
`configure.enabled: true` in `config.yaml` none of it is active._

Admins change switch settings from the port dashboard, as in UniFi or Omada: click a port, set
its VLANs, name and state; manage the switch's VLANs. The first version supports VSP/VOSS only.

Until now ERS-Proxy has only read from switches: monitoring sends no SNMP SET and no EDM write
(`TestReadOnly` in `internal/monitor`, `internal/snmp` and `internal/swos`;
[monitoring.md](monitoring.md), [security.md](security.md), [drivers.md](drivers.md)). This
feature changes that on purpose. The rules are:

- monitoring stays read-only; the existing `TestReadOnly` tests stay as they are;
- writes live only in the new package `internal/switchcfg`;
- the feature is off unless an operator turns it on, globally **in `config.yaml`** and per
  device.

## Scope of the first version

| Area | Settings |
|---|---|
| Port VLANs | PVID / untagged VLAN, tagged VLANs |
| Port | admin state (enable/disable), name (`ifAlias`) |
| VLANs | create, rename, delete |
| PoE | enable/disable per port (SNMP only on VOSS, see below) |

- **Port modes:** access, trunk and hybrid. A hybrid port tags its VLANs and also sends one
  VLAN untagged (the PVID). On VOSS this is tagging on plus "untag default VLAN"
  (`rcVlanPortUntagDefaultVlan`).
- **PoE on VOSS:** VOSS's EDM has no PoE objects to write (only a read of
  `pethPsePortDetectionStatus`). Over EDM a PoE change is refused (`no_poe`); it needs the
  SNMP channel (POWER-ETHERNET).
- **Switches:** VSP/VOSS (driver `voss`) only. ERS, TP-Link, SwOS, Cisco and generic Q-BRIDGE
  come later as separate steps.
- **Permission:** role `admin` only.
- **Persistence:** "commit confirmed". A change goes into the running configuration and must
  be confirmed within a time limit. Otherwise ERS-Proxy reverts it. The configuration is saved
  to NVRAM only on confirmation.
- **Write channels:** EDM (`setVariables` with the stored web login) and SNMP SET (with
  separate write credentials). Both sit behind one interface.

## Design

### Isolation and switches

- New package `internal/switchcfg` with sub-packages `rcplan` (planner for RAPID-CITY switches,
  VOSS now), `switchcfgtest` (in-memory switch for tests), and later `edmwrite` and
  `snmpwrite`. It lives outside `internal/monitor` and `internal/snmp`.
- Objects a planner reads are in `monitor.Objects` (the SNMP reader resolves names only
  there); objects that are only written (`rcVlanRowStatus`) are in `switchcfg.Objects`.
  - The gosnmp `Set` call appears only in `snmpwrite`. It uses the `*gosnmp.GoSNMP` that
    `snmp.Dial` returns, so `internal/snmp` still exports no `Set*` function.
  - A new test checks that only `internal/web` and `cmd/...` import `switchcfg`.
- `configure.enabled` in `config.yaml` (default `false`). **Only the configuration file can
  turn it on**: no `ERSPROXY_*` environment variable, command-line flag or UI setting enables
  it (a test checks that the environment cannot). Port monitoring alone, as a pure dashboard,
  is the default.
- When it is off, the server registers no configuration routes, shows no configuration
  controls or device setting, and does not start the manager; the manager also refuses every
  operation itself (`ErrDisabled`).
- Per device, `config_source`: `none` (default), `edm` or `snmp`.

### Model (`internal/switchcfg`)

**Changes and assignments**

- `Change` is made of:
  - `PortChange{IfIndex, Admin*, Alias*, VLANs*, PoE*}`; the VLAN part is
    `PortVLANs{Untagged, Tagged, Hybrid}` (the untagged VLAN is also the PVID). `Hybrid` means tagging on with an
    untagged VLAN, even without tagged VLANs. The web form's mode "hybrid" sets it and needs
    an untagged VLAN. The VOSS planner declares `Capabilities.Hybrid`; it needs
    `rcVlanPortUntagDefaultVlan` to be readable, otherwise a hybrid port is refused
    (`hybrid`);
  - `VLANChange{Op: create|rename|delete, ID, Name}`.
- `Assignment{Name, Index, Value monitor.Value}` addresses an object by MIB name, like
  `monitor.Reader`.
- `Writer{ Set(ctx, []Assignment) error }`. One logical step is one SNMP PDU or one
  `setVariables` request.

**Plans**

- A `Planner` per driver returns
  `Plan{Steps, Inverse, Expected, Save, Warnings, Hash}`.
- The inverse uses absolute values read just before the change, so applying it twice is
  harmless.
- Steps run in this order: create VLANs, then set membership, PVID and tagging, then delete.
  The inverse runs in reverse order.
- Deleting a VLAN captures its whole row first: name, type, STG, `rcVlanPortMembers`, and the
  PVID and tagging of affected ports. A delete that could not be restored this way is refused.

**Capabilities and port sets**

- Each driver declares `Capabilities`, which decide what the UI offers.
- Port sets are read with `monitor.BitsMSB`. A matching encoder is added. A membership change
  always reads the current bitmap fresh, flips the bit and writes the whole bitmap.

**Guards**

- **Warning, needs an explicit acknowledgement:**
  - the port has an LLDP neighbour (an uplink);
  - the port is a LAG/MLT member;
  - the VLAN has an IP interface (management) or an I-SID;
  - the VLAN being deleted still has members.
- **Management VLAN unknown** (`mgmt_unknown`): the planner finds the management VLAN through
  the IP address table (`ipAdEntIfIndex`), which EDM does not serve. When it cannot be read,
  every change that could cut the switch off (removing VLAN membership, disabling a port,
  deleting a VLAN) needs an acknowledgement, and the management VLAN cannot be refused by
  name.
- **Always refused:** deleting VLAN 1.

### Apply, confirm, revert (`switchcfg.Manager`)

1. **Lock.** The device is locked, and only one open change per device is allowed. A poller
   gate (`Hold`/`Release`) keeps half-applied states out of snapshots and history.
2. **Read and plan.** The affected objects are read fresh, through the same channel that will
   write: the shared EDM session, or SNMP with the write credentials. The plan is then
   computed. The preview shows the diff. Apply takes only the plan ID and its hash, never form
   fields, so what is written is exactly what was shown. Before writing, Apply reads the
   values of the plan again; if the switch changed since the preview, nothing is written
   (`ErrStale`).
3. **Persist.** The transaction is stored with the full plan before the first write (state
   `applying`).
4. **Write and check.**
   - A write error reverts what was already written (states `reverting`, then `failed`).
   - The values are then read back. A mismatch, or a switch that no longer answers, reverts
     at once (state `failed`).
5. **Wait for confirmation.** State `pending`. The deadline is stored as wall-clock time
   (`configure.confirm_timeout`, default 2 minutes). Then an immediate poll (`PollNow`).
6. **Confirm.** The state change is a compare-and-swap, so the timer and the click cannot
   both win.
   - Steps: read, save to NVRAM, read again (state `confirmed`).
   - If the save fails, the state is `confirmed_unsaved` and the UI offers "save again".
   - VOSS saves with `rcSysActionL1.0 = 7` (`saveRuntimeConfig`), recorded in 8.1. The VOSS
     planner has `SaveKnown = true`: a confirmed change ends as `confirmed`.
     `confirmed_unsaved` only happens when the save fails, or for a planner without a known
     save action.
7. **Revert** on deadline or "Revert".
   - The inverse plan is written (every step, even after an error: the values are absolute,
     and a step may fail only because the change was applied in part), then read back
     (states `reverting`, then `reverted`).
   - If the switch cannot be reached, the state is `revert_failed`. The manager retries with
     backoff, and the UI shows a red warning: not saved, a reboot of the switch restores the
     last saved configuration.
8. **Recovery on start.**
   - `applying`: read the switch, compare with the before and expected values, and revert.
   - `pending`: wait for the rest of the deadline, or revert at once if it has passed.

Limits that the UI and the docs must state:

- If a change cuts ERS-Proxy off from the switch, ERS-Proxy cannot revert it. Only a reboot
  restores the switch. That is why nothing is saved before confirmation.
- Confirming also saves unsaved changes made by others, for example in EDM.

### EDM channel

**Shared session**

- Move session handling out of `edmsrc.Session` into `internal/edmsession`: login, cookie
  jar, `refused`, `Post` and `Logout`.
  - `edmsrc.Session` embeds it, so callers stay the same.
  - The poller and the writer share one session per device. VSP switches allow only a few web
    sessions.
- In `cmd/ers-proxy/cmd/proxy.go`, split a `Session(ctx, d)` accessor out of
  `edmSessions.open`.
  - The manager asks for the session on every call and never caches it. It is replaced when
    the stored login changes.

**Write requests**

- Request format, from EDM's own scripts: `POST jsp/setFormMibs.jsp` with
  `command=setVariables&varNames=…&varValues=…&varCount=n`.
  - Step 8.1 records the separator, the port-set encoding, the answers and the errors on a
    VSP.
- A write is replayed after a login page only if the login page is recognised for certain.
- `setVariables` is never marked with `upstream.WithRetry`. That stays reserved for read
  commands (`upstream.EDMReadCommand`).

### SNMP channel

- **Write credentials**
  - New field `snmp_write_credentials`, AES-GCM with AAD
    `ers-proxy snmp-write|<id>|<host>|<port>`.
  - Cleared when the host or SNMP port changes, like the read credentials (`devices.SNMPAAD`).
  - New card "SNMP write access": write-only, needs the password re-entered
    (`web.confirmPassword`).
- **Object names** come from `monitor.Objects`, plus a map of write-only objects in
  `switchcfg`: `rcVlanRowStatus`, `rcVlanType`, `rcVlanStgId` and VOSS's save action.
- **Shared poller code**
  - Export the SNMP target construction from `poller.open` as `poller.OpenSNMP`.
  - Export `pollOnce`, without the sink, as `poller.CollectNow`.
- _Done in 8.5_, together with `poller.Hold` (per-device gate) and `poller.PollNow`.

### Data (new migration `00004_configure.sql`)

- `devices.config_source TEXT NOT NULL DEFAULT 'none'`
- `devices.snmp_write_credentials BLOB`
- Table `config_changes`: one row per change, which also serves as the change history of a
  switch. It is deleted together with its device.

  | Column | Content |
  |---|---|
  | `device_id`, `user_id`, `actor` | which switch, who changed it |
  | `created_at`, `deadline`, `finished_at` | Unix seconds |
  | `state` | `applying`, `pending`, `confirmed`, `confirmed_unsaved`, `reverting`, `reverted`, `revert_failed`, `failed` |
  | `summary` | readable description of the change |
  | `plan` | JSON with forward, inverse and expected values |
  | `error` | why the change failed, if it did |

### UI

The UI is shown only to admins, and only when the feature is enabled globally and on the
device.

- **Port page:** a settings form with admin state, name, untagged VLAN, tagged VLANs and PoE.
- **Switch page:** create, rename and delete VLANs; the change history.
- **Preview page:** the diff and its warnings, then "Apply".
- **Banner:** a countdown with "Confirm" and "Revert".
  - "Confirm" and "Revert" are plain forms with the CSRF token, so they work without
    JavaScript; `static/switch.js` only runs the countdown (from `data-deadline`) and reloads
    the page at the deadline.
  - No inline scripts or styles. Texts are only keys in `internal/web/i18n.go`.
- **Preview:** port bitmaps are shown as the ports they contain (`rcVlanPortMembers.10 = 1/2,
  1/3`), not as hex octets.
- **Device form:** "Configuration through: off / EDM / SNMP". Turning it on needs the password
  re-entered.
- **Audit log:**
  - Actions: `config.apply`, `config.confirm`, `config.revert`, `config.revert_failed`,
    `config.save`.
  - Each entry carries a readable diff and never contains credentials.

## Steps

1. **8.1 Recording on a VSP.** _Done 2026-11-06 on a VSP 8404 with VOSS 8.10.1.0._ Run each
   v1 action once by hand in EDM through the proxy.
   - Record the network traffic, and read the same objects over SNMP.
   - The result is the "Write recipes" section below: objects, index, encoding, order,
     answers, errors, the save action, and VOSS's tagging and STG quirks.
   - Add synthetic fixtures.
   - Afterwards, reset the switch and log out.
2. **8.2 `internal/edmsession`.** Move session handling out of `edmsrc` (a pure refactor; all
   tests stay green). Add `edmSessions.Session`.
3. **8.3 Model and VOSS planner.** In `internal/switchcfg` and `internal/switchcfg/rcplan`:
   table tests that also cover the inverse plan, the guards and the port-set encoder.
4. **8.4 Migration, store and configuration.**
   - Migration `00004`, store functions and `configure.*`.
   - `docs/config.example.yaml` must still equal `config.Example`.
   - Update [configuration.md](configuration.md).
5. **8.5 Manager and poller hooks.**
   - Add `Hold`/`Release`, `PollNow`, `CollectNow` and `OpenSNMP`.
   - Build the state machine and the recovery on start.
   - Test with a fake writer that injects failures: an abort mid-write, a read-back mismatch,
     an unreachable switch on confirm and on revert, a restart while a change is `pending`, and
     the race between confirm and the timer.
6. **8.6 `edmwrite`.** `internal/mockswitch` applies `setVariables` to its data, answers with
   errors when asked to, and returns 503 after N sessions.
7. **8.7 `snmpwrite`.**
   - Add the write credentials: encryption, AAD, and clearing when the address changes.
   - Test modifications with snmpsim's `writecache` variation, using a fresh `.snmprec` copy
     per test.
   - Test row creation and deletion through the PDU contents.
8. **8.8 Wiring in `cmd/ers-proxy/cmd`.**
   - A reader and a writer per channel, and the manager start.
   - Log out on shutdown.
   - The device form and the audit entries.
9. **8.9 Web, read part.** Capabilities, the preview with its hash, the change history.
10. **8.10 Web, write part.** The port and VLAN forms, the banner, the JavaScript POST, i18n,
    and screenshots against the mock.
11. **8.11 Hardware test on a VSP.**
    - Use both channels, and only unused ports and test VLANs.
    - For each action, test four paths: apply then confirm, apply then revert, apply then let
      the deadline pass, and a restart of ERS-Proxy while the change is `pending`.
    - Also try an invalid value.
    - Record the results in [hardware-tests.md](hardware-tests.md) and
      [compatibility.md](compatibility.md).
12. **8.12 Documentation and rules.**
    - [security.md](security.md): new risk. An admin account can now change the network, and
      write credentials are stored.
    - [monitoring.md](monitoring.md) and [drivers.md](drivers.md): monitoring is read-only;
      configuration is separate and opt-in.
    - [architecture.md](architecture.md).
    - CLAUDE.md: writes only to test switches the maintainer approved, never in automated
      tests.
    - README, CHANGELOG and ROADMAP.

Steps 8.2 to 8.5 need no hardware and can start before 8.1. Step 8.1 must be done before
`edmwrite` (8.6) is finished.

Later steps:

- ERS: watch out for autosave, which writes the running configuration to NVRAM without being
  asked.
- TP-Link: SNMP, port lists as text, saving.
- SwOS: POST requests, which need an encoder.
- Cisco and generic Q-BRIDGE.
- Selecting several ports at once; LAG and STP.

## Verification

- After every step, run `go vet ./...` and `go test ./...`.
- End to end against the mock in an isolated test instance:
  - change a port's VLAN, check the preview, apply, see the banner, confirm;
  - in a second run, do not confirm and check that the change is reverted automatically;
  - take screenshots of the port page, the preview and the banner.
- Real hardware only on a VSP approved for write tests. Never on ERS, TP-Link or SwOS
  switches in production.

## Write recipes

Recorded 2026-11-06 on a VSP 8404 with VOSS 8.10.1.0, EDM over HTTPS. Sources: EDM's own
JavaScript of VOSS 8.10, and `setVariables` requests sent to the switch by hand and read back
over SNMP. The ERS 5600 webroot uses the same request format.

| Item | Format |
|---|---|
| Request | `POST /jsp/setFormMibs.jsp`, form fields `command=setVariables`, `varNames`, `varValues`, `varCount`, `userId` |
| Several variables | names and values joined by a TAB; `varCount` = their number; EDM sends at most 60 per request |
| Names | `object.index` (`rcVlanPortMembers.10`, `ifAlias.192`; scalars `name.0`) |
| Numbers and enums | decimal text |
| PortSet | `0x` + lowercase hex octets joined by `:`; bit i = ifIndex i, most significant bit first; EDM cuts the trailing zero octets, an empty set is `0x00:00`; the switch answers with the full length |
| Text | as is; **empty text is sent as `0x00:`**. A really empty value makes the VSP's web server hang (no answer). |
| Success answer | `name:value` plus CR LF per variable, name without index (`ifAlias:uplink`) |
| Error answer | `snmpErrorStatus:<n>`, CR LF, `snmpErrorIndex:<i>`, CR LF; no message. `<i>` is the position of the failed variable. |
| Error codes | standard SNMP statuses, plus vendor codes on VOSS (seen: 12801 when creating VLAN 4090, a reserved ID). `owdErrorStatus:` is an error too. |
| Logout | EDM's own logout also goes to `setFormMibs.jsp` (`command=logout`); ERS-Proxy uses `GET /logout.html` |

- The agent decodes text that starts with `0x` as hex. The planner therefore refuses port
  names and VLAN names that start with `0x`.
- ERS-Proxy encodes empty text as `0x00:`, parses `snmpErrorIndex` and names the standard SNMP
  statuses in the error.

VOSS objects:

| Action | Objects |
|---|---|
| Port name | `ifAlias` |
| Port admin state | `ifAdminStatus` (1 up, 2 down) |
| Create VLAN | one request with `rcVlanRowStatus = 4` (createAndGo), `rcVlanType = 1` (byPort), `rcVlanName`, `rcVlanStgId = 1` |
| Rename VLAN | `rcVlanName` |
| Delete VLAN | `rcVlanRowStatus = 6` (destroy) |
| Membership | `rcVlanPortMembers`; always the whole PortSet of the VLAN |
| PVID | `rcVlanPortDefaultVlanId`; a port that leaves all its VLANs gets PVID 0, and 0 can be written |
| Tagging | `rcVlanPortPerformTagging` is a TruthValue: 1 tag, 2 untag. There is no "untag PVID only" value. |
| Untag default VLAN | its own object `rcVlanPortUntagDefaultVlan` (`1.3.6.1.4.1.2272.1.3.3.1.13`, TruthValue); CLI `untag-port-default-vlan` |
| Save | `rcSysActionL1.0 = 7` (`saveRuntimeConfig`, `1.3.6.1.4.1.2272.1.1.86`); `rcSysLastRunTimeConfigSave` changes after it |

- **Port modes:** access = tagging 2 and untag default 2. Trunk = tagging 1 and untag default 2. Hybrid = tagging
  1, untag default 1 and the PVID.
- **Order VOSS accepts (the planner's order):** create the VLAN (one request, see above);
  rename; enable tagging; add members; set the PVID; set untag default VLAN. Back: tag the
  default VLAN, remove members, PVID, disable tagging, delete the VLAN.
- EDM also sends `rcVlanColor` and `rcVlanPortMembers` when it creates a VLAN. Neither is
  needed.
- **Management VLAN:** EDM does not serve the IP address table, so over EDM every port change
  shows the `mgmt_unknown` warning (as designed).
- **PoE:** VOSS's EDM has no PoE objects to write, so PoE over EDM stays refused (`no_poe`).
  The VSP 8404 has no PoE anyway.

Still open: the same recipes on VOSS 7.1 (VSP 4850GTS), and the SNMP channel (needs write
credentials on the switch).
