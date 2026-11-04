# Writing a port dashboard driver

A driver tells the port dashboard where one vendor keeps its data. This page is for
contributors; administrators want [monitoring.md](monitoring.md). Everything here is
**read-only**: a driver never writes to a device.

## Architecture

```
poller ──> Driver.Collect(ctx, Reader) ──> *monitor.Snapshot ──> Rates / Diff ──> history
                          │
              Reader (by MIB object name)
              ├── snmpsrc  (SNMP GET/walk; names -> OIDs via monitor.Objects)
              └── edmsrc   (EDM getFormMibs/getTableMibs; ERS only, experimental)
```

- **Reader** (`internal/monitor/monitor.go`) reads objects by MIB name:
  `Scalars(ctx, names...)` and `Columns(ctx, names...)`, which walks table columns and merges
  them by row index (`"12"`, `"1.5"`; split with `monitor.Ints`). Objects the agent does not
  implement are simply absent. Drivers must not depend on the transport.
- **`monitor.Objects`** (`internal/monitor/objects.go`) maps every object name to its OID without
  instance suffix. Scalars are read with `.0` appended. `snmp-capture` walks exactly these
  objects. The EDM reader can only serve the tables listed in `edmsrc.Tables`; everything else
  reads as empty there.
- **`CollectBase`** (`internal/monitor/base.go`) reads what the standard MIBs provide: system
  group, IF-MIB interfaces and counters (64-bit preferred), EtherLike duplex, 802.3ad
  aggregation, LLDP neighbours and PoE. It returns a `Base` with the `Snapshot` and the
  bridge-port map (`dot1dBasePort` to ifIndex). `BaseOptions` adapts it to a vendor:
  - `Locate(p) (unit, number)` places a physical port on the front panel. Default: unit 1,
    number = bridge port number (or position among the physical ports).
  - `PoEIfIndex(group, port) int` maps a `pethPsePortTable` index to an ifIndex. Default: the
    port with `Unit == group` and `Number == port`.
  - `SkipPoE` leaves POWER-ETHERNET-MIB out; a driver with a vendor PoE table calls
    `Base.ReadPoE` only when that table is missing (`tplink` does this).
- **`Driver`** interface:
  - `Name()`: the identifier stored with a device (`generic`, `ers`, `cisco`, `tplink`, `voss`).
  - `Match(System) int`: how well the driver fits. `0` not at all, `10` generic fallback, `90`
    probably (for example by `sysDescr`), `100` made for this vendor (by `sysObjectID` prefix).
  - `Collect(ctx, Reader) (*Snapshot, error)`.
- **Registry**: `monitor.NewRegistry(drivers.All()...)`. `Detect(System)` returns the driver
  with the highest `Match` score, `Get(name)` a fixed one (device setting `monitor_driver`),
  `ErrNoDriver` if all score 0.
- **Snapshot** (`Time`, `Driver`, `System`, `Units`, `Ports`, `VLANs`, `PoE`, `Notes`). A `Port`
  carries `IfIndex`, `Unit`/`Number` (0 for non-front-panel interfaces), names, `Physical`,
  `IsLAG`, `Admin`/`Oper`, `Speed` (bit/s), `Duplex`, `PVID`, `Untagged`, `Tagged`, `Mode`
  (`access`, `trunk`, `hybrid`), `PoE`, `LAG`/`LAGName`, `Neighbor`, and the raw counters.
  `InBps`, `OutBps` and `Utilization` are filled later by `monitor.Rates`; drivers leave them
  alone. Call `SetMode(p)` after filling the VLAN lists when the vendor has no explicit mode.
  Sort the VLAN lists (`sort.Ints`).

### Bitmap conventions

`monitor.BitsMSB(b)` returns the positions of all set bits, counting from the most significant
bit of the first octet as 0.

| Bitmap | Position means |
|---|---|
| Q-BRIDGE `PortList` (`dot1qVlanCurrentEgressPorts`, `...Untagged...`) | bit position + 1 = **bridge port** number; map it with `Base.BridgePorts` (the shared `QBridge` helper does this) |
| ERS `PortSet` (`rcVlanPortMembers`, `rcMltPortMembers`) | bit position = **ifIndex** |
| Cisco `vlanTrunkPortVlansEnabled*` | four bitmaps of 1024 VLANs each; VLAN ID = bit position + 1024 × bitmap number |

Check the convention of a new MIB against a real capture; off-by-one errors here silently put
VLANs on the wrong ports.

## Step by step

1. **Capture a device.** From a host that may query the switch (read-only, GET/GETBULK only):
   ```bash
   ERSPROXY_SNMP_COMMUNITY='<ro-community>' ers-proxy snmp-capture --host 192.0.2.20 --out switch.snmprec
   # or with the credentials stored for a device: ers-proxy snmp-capture <id> --out switch.snmprec
   ```
   Names, aliases, serial numbers, VLAN names and LLDP neighbours are anonymised unless you pass
   `--raw`. **Read the file before you share it** (model and firmware strings stay; check
   `sysLocation`, descriptions and MAC addresses). Attach it to a "Device report" issue. Never
   commit a real capture unless it is anonymised and the maintainers approved it.
2. **Add OIDs** of the vendor tables to `monitor.Objects` in `internal/monitor/objects.go`
   (names from the MIB, numeric OIDs as plain strings, a comment naming the MIB). Only OIDs are
   added, never MIB files. If the EDM source should serve them too, add the table to
   `edmsrc.Tables`.
3. **Write the driver** in `internal/monitor/drivers/<vendor>.go`. Start from `CollectBase`
   (with `BaseOptions` if the numbering is vendor-specific), set `Snap.Driver`, then add the
   vendor parts:
   ```go
   func (MyVendor) Collect(ctx context.Context, r monitor.Reader) (*monitor.Snapshot, error) {
       b, err := monitor.CollectBase(ctx, r, monitor.BaseOptions{})
       if err != nil {
           return nil, err
       }
       b.Snap.Driver = "myvendor"
       if err := QBridge(ctx, r, b); err != nil { // or vendor VLAN tables
           b.Snap.Notes = append(b.Snap.Notes, "VLANs: "+err.Error())
       }
       return b.Snap, nil
   }
   ```
   Look at `generic.go` (Q-BRIDGE), `tplink.go` (vendor table with text port lists), `cisco.go` (vendor VLAN tables
   with Q-BRIDGE fallback) `ers.go` (own numbering, PortSet bitmaps, stack units) and `voss.go` (slot/port from `ifName`, reuses the ERS RAPID-CITY helpers). A failing
   optional read should add a note and keep going; only a missing system group or interface
   table is an error.
4. **Register** it in `drivers.All()` (`generic.go`). Give `Match` a score of 100 for the
   vendor's `sysObjectID` prefix; use 90 only for a weaker signal such as `sysDescr`.
5. **Add a synthetic fixture.** In `testdata/snmpsim/gen_monitor.py` add a function that builds
   an `Agent` with the helpers (`system`, `interface`, `a.s`, `a.i`, `a.x`, `a.o`, `bitmap`)
   and ends with `a.write("<name>.snmprec")`; call it from the `__main__` block and run
   `python3 -I testdata/snmpsim/gen_monitor.py`. The **community of an agent is its file name**
   (snmpsim convention). Only the OIDs are real; all values are made up. Keep the device small
   but include each feature the driver reads: up, down and admin-down ports, a trunk, a LAG, PoE
   and a neighbour where relevant.
6. **Write a simulator test** in `internal/monitor/drivers/drivers_test.go`, modelled on
   `TestGenericAgainstSimulator`:
   ```go
   func TestMyVendorAgainstSimulator(t *testing.T) {
       s, d := collect(t, "myvendor") // community = file name; also checks Detect
       if d.Name() != "myvendor" { t.Fatalf("detected %s", d.Name()) }
       p := port(t, s, 1)
       // assert state, speed, VLANs, mode, PoE, LAG, neighbour ...
   }
   ```
   Add small pure tests for decoding helpers (see `TestERSTagging`, `TestCiscoTrunkBitmaps`).
7. **Run the tests.** The simulator tests are skipped when `snmpsim` is missing or with
   `-short`:
   ```bash
   python3 -m venv .venv && .venv/bin/pip install snmpsim pysmi   # once; .venv is found automatically
   # or: export ERSPROXY_SNMPSIM=/path/to/snmpsim-command-responder
   go vet ./... && go test ./...
   ```
8. **Stay read-only.** `TestReadOnly` (`internal/monitor/monitor_test.go`) fails if code in
   `internal/monitor` or `internal/snmp` calls `Set`/`SetRequest` or mentions the EDM write
   commands. Do not work around it.
9. **Document** the driver in the "Port dashboard drivers" table of
   [compatibility.md](compatibility.md): detection, data used, and how it was verified
   (synthetic fixtures only, or a named model and firmware on real hardware).

## Rules

- **No vendor files in the repository.** No MIB files and no real captures; OIDs only, with the
  MIB named in a comment. Fixtures contain synthetic values.
- **Treat everything a device reports as untrusted.** Names, aliases and neighbour data are
  displayed after escaping; do not build paths, commands or queries from them, and do not
  assume sizes (bitmaps may be shorter or longer than expected, indexes may be missing).
- **No SNMP SET, no EDM writes**, not even "harmless" ones.
- **Record assumptions.** If a driver relies on facts that no real device confirmed (port
  numbering, bitmap order, tagging semantics), add a line to `Snapshot.Notes`. Notes appear on
  the switch page and in `snmp-test --collect`; the `ers` driver does this for stacks
  (numbering of units 2–8).
- A device that lacks an optional table must still produce a snapshot.
