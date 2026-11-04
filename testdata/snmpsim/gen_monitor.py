#!/usr/bin/env python3
"""Generates the synthetic snmpsim agents for the port dashboard drivers.

    python3 -I testdata/snmpsim/gen_monitor.py

writes generic.snmprec (a Q-BRIDGE switch), ers5600.snmprec (an ERS 5600
two-unit stack with RAPID-CITY VLANs and no Q-BRIDGE), cisco.snmprec (VTP) and
tplink.snmprec (Q-BRIDGE) and voss.snmprec (VSP 4850GTS on VOSS 7.1). All values are made up;
only the OIDs are real. The community of each agent is its file name.
"""
import os

HERE = os.path.dirname(os.path.abspath(__file__))


def octets(data):
    return "4x|" + bytes(data).hex()


def bitmap(positions, size):
    """Bit at position i: octet i//8, most significant bit first."""
    b = bytearray(size)
    for i in positions:
        b[i // 8] |= 0x80 >> (i % 8)
    return bytes(b)


class Agent:
    def __init__(self):
        self.rows = []

    def add(self, oid, typ, value):
        self.rows.append((oid, f"{typ}|{value}" if not str(typ).startswith("4x") else typ))

    def s(self, oid, text):
        self.rows.append((oid, f"4|{text}"))

    def i(self, oid, n, typ=2):
        self.rows.append((oid, f"{typ}|{n}"))

    def x(self, oid, data):
        self.rows.append((oid, octets(data)))

    def o(self, oid, value):
        self.rows.append((oid, f"6|{value}"))

    def write(self, name):
        key = lambda r: [int(p) for p in r[0].split(".")]
        with open(os.path.join(HERE, name), "w") as f:
            for oid, v in sorted(self.rows, key=key):
                f.write(f"{oid}|{v}\n")


IF = "1.3.6.1.2.1.2.2.1"
IFX = "1.3.6.1.2.1.31.1.1.1"


def system(a, descr, oid, name):
    a.s("1.3.6.1.2.1.1.1.0", descr)
    a.o("1.3.6.1.2.1.1.2.0", oid)
    a.i("1.3.6.1.2.1.1.3.0", 8640000, 67)  # 1 day
    a.s("1.3.6.1.2.1.1.5.0", name)
    a.s("1.3.6.1.2.1.1.6.0", "lab rack 1")


def interface(a, idx, name, descr, typ, up, mbps, alias="", admin_up=True, in_octets=0, out_octets=0, errors=0):
    a.s(f"{IF}.2.{idx}", descr)
    a.i(f"{IF}.3.{idx}", typ)
    a.i(f"{IF}.5.{idx}", min(mbps * 1000000, 4294967295), 66)
    a.i(f"{IF}.7.{idx}", 1 if admin_up else 2)
    a.i(f"{IF}.8.{idx}", 1 if up else 2)
    a.i(f"{IF}.9.{idx}", 4200, 67)
    a.i(f"{IF}.14.{idx}", errors, 65)
    a.i(f"{IF}.20.{idx}", 0, 65)
    a.i(f"{IF}.13.{idx}", 0, 65)
    a.i(f"{IF}.19.{idx}", 0, 65)
    a.s(f"{IFX}.1.{idx}", name)
    a.i(f"{IFX}.6.{idx}", in_octets, 70)
    a.i(f"{IFX}.10.{idx}", out_octets, 70)
    a.i(f"{IFX}.15.{idx}", mbps if up else 0, 66)
    a.s(f"{IFX}.18.{idx}", alias)
    a.i(f"{IFX}.19.{idx}", 0, 67)


def generic():
    a = Agent()
    system(a, "Generic Q-BRIDGE test switch", "1.3.6.1.4.1.99999.1", "gen-sw1")
    for p in range(1, 9):
        up = p not in (5, 6)
        interface(a, p, f"gi1/0/{p}", f"GigabitEthernet1/0/{p}", 6, up, 1000 if p != 4 else 100,
                  alias={1: "printer", 8: "uplink core"}.get(p, ""), admin_up=p != 6,
                  in_octets=p * 1000000, out_octets=p * 2000000, errors=3 if p == 2 else 0)
        a.i(f"1.3.6.1.2.1.17.1.4.1.2.{p}", p)  # dot1dBasePortIfIndex
        a.i(f"1.3.6.1.2.1.10.7.2.1.19.{p}", 2 if p == 4 else 3)  # duplex
        a.i(f"1.3.6.1.2.1.17.7.1.4.5.1.1.{p}", 10 if p in (3, 4) else 1, 66)  # dot1qPvid
    interface(a, 100, "Po1", "Port-channel1", 161, True, 2000)
    interface(a, 200, "Vlan1", "Vlan1", 53, True, 1000)
    for p in (7, 8):  # LAG members
        a.i(f"1.2.840.10006.300.43.1.2.1.1.13.{p}", 100)
    # VLANs: 1 untagged on 1,2,5,6,7,8; 10 untagged on 3,4 and tagged on 8; 20 tagged on 8
    vlans = {1: ("default", [1, 2, 5, 6, 7, 8], [1, 2, 5, 6, 7, 8]),
             10: ("office", [3, 4, 8], [3, 4]),
             20: ("voice", [8], [])}
    for vid, (name, egress, untagged) in vlans.items():
        a.s(f"1.3.6.1.2.1.17.7.1.4.3.1.1.{vid}", name)
        a.x(f"1.3.6.1.2.1.17.7.1.4.3.1.2.{vid}", bitmap([p - 1 for p in egress], 2))
        a.x(f"1.3.6.1.2.1.17.7.1.4.3.1.4.{vid}", bitmap([p - 1 for p in untagged], 2))
        a.x(f"1.3.6.1.2.1.17.7.1.4.2.1.4.0.{vid}", bitmap([p - 1 for p in egress], 2))
        a.x(f"1.3.6.1.2.1.17.7.1.4.2.1.5.0.{vid}", bitmap([p - 1 for p in untagged], 2))
    # PoE on group 1, ports 1-2 (port 1 delivering class 2, port 2 searching)
    for p, st in ((1, 3), (2, 2)):
        a.i(f"1.3.6.1.2.1.105.1.1.1.3.1.{p}", 1)
        a.i(f"1.3.6.1.2.1.105.1.1.1.6.1.{p}", st)
        a.i(f"1.3.6.1.2.1.105.1.1.1.10.1.{p}", 3)
    a.i("1.3.6.1.2.1.105.1.3.1.1.2.1", 370, 66)
    a.i("1.3.6.1.2.1.105.1.3.1.1.4.1", 12, 66)
    # LLDP: local port 8 has a bridge neighbour
    for p in range(1, 9):
        a.i(f"1.0.8802.1.1.2.1.3.7.1.2.{p}", 5)
        a.s(f"1.0.8802.1.1.2.1.3.7.1.3.{p}", f"gi1/0/{p}")
    a.s("1.0.8802.1.1.2.1.4.1.1.7.0.8.1", "1/49")
    a.s("1.0.8802.1.1.2.1.4.1.1.8.0.8.1", "Uplink to access")
    a.s("1.0.8802.1.1.2.1.4.1.1.9.0.8.1", "core-sw")
    a.x("1.0.8802.1.1.2.1.4.1.1.12.0.8.1", [0x28, 0x00])  # bridge + router
    a.write("generic.snmprec")


def ers5600():
    a = Agent()
    system(a, "Ethernet Routing Switch 5698TFD-PWR   HW:07       FW:6.0.0.18   SW:v6.6.3.015 BN:15 (c) Avaya Networks",
           "1.3.6.1.4.1.45.3.80.1", "ers-stack")
    ppu = 128
    ports = [(1, n) for n in range(1, 5)] + [(2, n) for n in range(1, 3)]
    for unit, n in ports:
        idx = (unit - 1) * ppu + n
        up = not (unit == 2 and n == 2)
        interface(a, idx, f"{unit}/{n}", f"Ethernet Port, Unit {unit}, Port {n}", 6, up, 1000, alias="",
                  in_octets=idx * 500000, out_octets=idx * 700000)
        a.i(f"1.3.6.1.2.1.10.7.2.1.19.{idx}", 3)
        # rcVlanPortTable: PVID, tagging (ifIndex 1 = tagAll trunk, ifIndex 2 = untagPvidOnly)
        a.i(f"1.3.6.1.4.1.2272.1.3.3.1.7.{idx}", 1)
        a.i(f"1.3.6.1.4.1.2272.1.3.3.1.8.{idx}", {1: 1, 2: 4}.get(idx, 2))
        # rcPortTable fallback values
        a.i(f"1.3.6.1.4.1.2272.1.4.10.1.1.13.{idx}", 2)
        a.i(f"1.3.6.1.4.1.2272.1.4.10.1.1.15.{idx}", 1000)
        # LLDP local ports as on a real ERS 5632FD: number = ifIndex, id = MAC, descr "Port n"
        a.i(f"1.0.8802.1.1.2.1.3.7.1.2.{idx}", 3)
        a.x(f"1.0.8802.1.1.2.1.3.7.1.3.{idx}", [0x00, 0x1b, 0x4f, 0x00, unit, n])
        a.s(f"1.0.8802.1.1.2.1.3.7.1.4.{idx}", f"Port {n}")
    interface(a, 10001, "VLAN 1", "VLAN #1", 135, True, 1000)
    # Out-of-band management port: Ethernet, but no front-panel port (as on a real ERS 5632FD)
    interface(a, 55001, "OOB #0", "Out-of-band Port 1", 6, True, 100)
    vlans = {1: ("Default", [1, 2, 129, 130]), 20: ("voice", [1, 2, 130]), 30: ("cameras", [3, 4])}
    for vid, (name, members) in vlans.items():
        a.s(f"1.3.6.1.4.1.2272.1.3.2.1.2.{vid}", name)
        a.x(f"1.3.6.1.4.1.2272.1.3.2.1.11.{vid}", bitmap(members, 88))
    # LLDP neighbour on port 1/1 (the EDM bridge reads it only through "indexNA")
    a.s("1.0.8802.1.1.2.1.4.1.1.7.7638.1.2", "two-gigabitEthernet 1/0/2")
    a.s("1.0.8802.1.1.2.1.4.1.1.8.7638.1.2", "uplink")
    a.s("1.0.8802.1.1.2.1.4.1.1.9.7638.1.2", "core-sw")
    a.x("1.0.8802.1.1.2.1.4.1.1.12.7638.1.2", [0x28, 0x00])
    # MLT 1 = ports 1/3 and 1/4, MLT 2 unused
    a.s("1.3.6.1.4.1.2272.1.17.10.1.2.1", "to-core")
    a.x("1.3.6.1.4.1.2272.1.17.10.1.3.1", bitmap([3, 4], 88))
    a.s("1.3.6.1.4.1.2272.1.17.10.1.2.2", "")
    a.x("1.3.6.1.4.1.2272.1.17.10.1.3.2", bitmap([], 88))
    # Stack units: s5ChasComTable index 3.<unit*10>.0
    # Column layout as on a real ERS 5632FD: descr = description, ver = model + HW
    # revision, 19 = running software.
    for unit, model, descr in ((1, "5698TFD-PWR", "96 10/100/1000 plus 2 10Gig XFP, PoE"),
                               (2, "5650TD", "48 10/100/1000 plus 2 10Gig XFP")):
        a.o(f"1.3.6.1.4.1.45.1.6.3.3.1.1.4.3.{unit * 10}.0", "1.3.6.1.4.1.45.3.80.781")
        a.s(f"1.3.6.1.4.1.45.1.6.3.3.1.1.5.3.{unit * 10}.0", descr)
        a.s(f"1.3.6.1.4.1.45.1.6.3.3.1.1.6.3.{unit * 10}.0", f"{model} HW:07")
        a.s(f"1.3.6.1.4.1.45.1.6.3.3.1.1.7.3.{unit * 10}.0", f"SN000{unit}")
        a.s(f"1.3.6.1.4.1.45.1.6.3.3.1.1.19.3.{unit * 10}.0", "v6.6.3.015")
    # PoE: unit 1 port 2 delivering (group = unit)
    a.i("1.3.6.1.2.1.105.1.1.1.6.1.2", 3)
    a.i("1.3.6.1.2.1.105.1.1.1.6.1.1", 2)
    a.i("1.3.6.1.2.1.105.1.3.1.1.2.1", 320, 66)
    a.i("1.3.6.1.2.1.105.1.3.1.1.4.1", 7, 66)
    a.write("ers5600.snmprec")


def cisco():
    a = Agent()
    system(a, "Cisco IOS Software, synthetic test switch", "1.3.6.1.4.1.9.1.1208", "cisco-sw1")
    for p in range(1, 7):
        interface(a, p, f"Gi1/0/{p}", f"GigabitEthernet1/0/{p}", 6, p != 4, 1000,
                  in_octets=p * 1000000, out_octets=p * 1500000)
        a.i(f"1.3.6.1.2.1.17.1.4.1.2.{p}", p)
        a.i(f"1.3.6.1.2.1.10.7.2.1.19.{p}", 3)
    # CISCO-VTP-MIB vtpVlanTable, index managementDomainIndex.vlanId
    for vid, name in ((1, "default"), (10, "users"), (20, "voice"), (1002, "fddi-default"),
                      (1003, "token-ring-default"), (1004, "fddinet-default"), (1005, "trnet-default")):
        a.i(f"1.3.6.1.4.1.9.9.46.1.3.1.1.2.1.{vid}", 1)  # operational
        a.s(f"1.3.6.1.4.1.9.9.46.1.3.1.1.4.1.{vid}", name)
    # CISCO-VLAN-MEMBERSHIP-MIB vmVlan: ports 1-4 access, port 6 access (not trunking)
    for p, vlan in {1: 10, 2: 10, 3: 1, 4: 10, 6: 20}.items():
        a.i(f"1.3.6.1.4.1.9.9.68.1.2.2.1.2.{p}", vlan)
    # vlanTrunkPortTable: port 5 trunking, native 1, VLANs 1, 10, 20 (and 1002 + 1500, unknown)
    T = "1.3.6.1.4.1.9.9.46.1.6.1.1"
    a.i(f"{T}.14.5", 1)
    a.i(f"{T}.5.5", 1)
    a.x(f"{T}.4.5", bitmap([1, 10, 20, 1002], 128))
    a.x(f"{T}.17.5", bitmap([1500 - 1024], 128))
    a.x(f"{T}.18.5", bitmap([], 128))
    a.x(f"{T}.19.5", bitmap([], 128))
    a.i(f"{T}.14.6", 2)
    a.i(f"{T}.5.6", 1)
    a.write("cisco.snmprec")


def tplink():
    """Like a real Omada SG2210P (firmware 5.20): no Q-BRIDGE, every interface has
    ifType 6, ports from ifIndex 49153, VLANs in TPLINK-DOT1Q-VLAN-MIB as text lists."""
    a = Agent()
    system(a, "Omada 10-Port Gigabit Smart Switch with 8-Port PoE+", "1.3.6.1.4.1.11863.1.1.233", "tp-sw1")
    interface(a, 100, "Vlan-interface100", "Vlan-interface100", 6, True, 1000)
    interface(a, 32769, "port-channel 1", "port-channel 1", 6, True, 2000)
    TP = "1.3.6.1.4.1.11863.6.14.1"
    a.s(f"{TP}.1.1.1.1.32769", "LAG1")
    a.i(f"{TP}.1.1.1.2.32769", 111)
    a.s(f"{TP}.1.1.1.5.32769", "N/A")
    pvid = {4: 55, 5: 140, 7: 100, 8: 7}
    for n in range(1, 11):
        idx = 49152 + n
        interface(a, idx, f"gigabitEthernet 1/0/{n}", f"gigabitEthernet 1/0/{n}", 6, n in (5, 8, 9, 10), 1000,
                  in_octets=n * 1000000, out_octets=n * 2000000)
        a.i(f"1.3.6.1.2.1.17.1.4.1.2.{idx}", idx)
        a.s(f"{TP}.1.1.1.1.{idx}", f"1/0/{n}")
        a.i(f"{TP}.1.1.1.2.{idx}", pvid.get(n, 111))
        a.s(f"{TP}.1.1.1.5.{idx}", "LAG1" if n in (9, 10) else "N/A")
    # TPLINK-POWER-OVER-ETHERNET-MIB (0.1 W): budget 61.0 W, 1.9 W used; ports 1-8,
    # port 4 PoE off, port 5 delivers 1.5 W class 2 (values as on the real switch)
    POE = "1.3.6.1.4.1.11863.6.56.1.1"
    a.i(f"{POE}.1.1.0", 610)
    a.i(f"{POE}.1.3.0", 19)
    a.i(f"{POE}.1.4.0", 591)
    for n in range(1, 9):
        a.i(f"{POE}.2.1.1.1.{n}", n)
        a.i(f"{POE}.2.1.1.2.{n}", 0 if n == 4 else 1)
        a.i(f"{POE}.2.1.1.4.{n}", 300)
        a.i(f"{POE}.2.1.1.7.{n}", 15 if n == 5 else 0)
        a.i(f"{POE}.2.1.1.10.{n}", 2 if n == 5 else 9)
        a.i(f"{POE}.2.1.1.11.{n}", 2 if n == 5 else 0)
    trunk = "1/0/1-3,1/0/6,LAG1"
    vlans = {1: ("System-VLAN", "", ""), 7: ("N/A", trunk, "1/0/8"), 55: ("N/A", trunk, "1/0/4"),
             100: ("N/A", trunk, "1/0/7"), 111: ("users", "", trunk), 140: ("N/A", trunk, "1/0/5")}
    for vid, (name, tagged, untagged) in vlans.items():
        a.i(f"{TP}.2.1.1.1.{vid}", vid)
        a.s(f"{TP}.2.1.1.2.{vid}", name)
        a.s(f"{TP}.2.1.1.3.{vid}", tagged)
        a.s(f"{TP}.2.1.1.4.{vid}", untagged)
    a.write("tplink.snmprec")


def tplink_qbridge():
    """A TP-Link model with Q-BRIDGE (fallback path of the driver)."""
    a = Agent()
    system(a, "JetStream synthetic 8-port switch", "1.3.6.1.4.1.11863.1.1.7", "tp-sw1")
    for p in range(1, 9):
        interface(a, p, f"1/0/{p}", f"gigabitEthernet 1/0/{p}", 6, p != 5, 1000,
                  in_octets=p * 1000000, out_octets=p * 2000000)
        a.i(f"1.3.6.1.2.1.17.1.4.1.2.{p}", p)
        a.i(f"1.3.6.1.2.1.17.7.1.4.5.1.1.{p}", 10 if p in (3, 4) else 1, 66)
    vlans = {1: ("default", [1, 2, 5, 6, 7, 8], [1, 2, 5, 6, 7, 8]),
             10: ("office", [3, 4, 8], [3, 4]),
             20: ("voice", [8], [])}
    for vid, (name, egress, untagged) in vlans.items():
        a.s(f"1.3.6.1.2.1.17.7.1.4.3.1.1.{vid}", name)
        a.x(f"1.3.6.1.2.1.17.7.1.4.3.1.2.{vid}", bitmap([p - 1 for p in egress], 2))
        a.x(f"1.3.6.1.2.1.17.7.1.4.3.1.4.{vid}", bitmap([p - 1 for p in untagged], 2))
        a.x(f"1.3.6.1.2.1.17.7.1.4.2.1.4.0.{vid}", bitmap([p - 1 for p in egress], 2))
        a.x(f"1.3.6.1.2.1.17.7.1.4.2.1.5.0.{vid}", bitmap([p - 1 for p in untagged], 2))
    a.write("tplink-qbridge.snmprec")


def voss():
    """VSP 4850GTS on VOSS 7.1 as seen on real hardware: ifIndex 192 = 1/1, no
    EtherLike/LAG/PoE MIBs, duplex and speed from RAPID-CITY rcPortTable."""
    a = Agent()
    system(a, "VSP-4850GTS (7.1.10.0)  HW Base: VSP 4850", "1.3.6.1.4.1.2272.202", "vsp-lab")
    a.s("1.3.6.1.4.1.2272.1.1.7.0", "7.1.10.0 build on Mon Jan 01 00:00:00 UTC 2024")
    a.s("1.3.6.1.4.1.2272.1.4.2.0", "SYNTH0001")
    # (ifIndex, port name, link up, admin up, Mbit/s)
    ports = [(192, "1/1", True, True, 1000), (193, "1/2", True, True, 1000), (194, "1/3", True, True, 1000),
             (195, "1/4", False, True, 0), (196, "1/5", False, False, 0), (197, "1/6", True, True, 1000),
             (240, "1/49", True, True, 10000), (241, "1/50", True, True, 10000)]
    tagging = {192: 1, 193: 2, 194: 2, 195: 2, 196: 2, 197: 4, 240: 2, 241: 2}
    pvid = {192: 10, 193: 10, 194: 20, 195: 10, 196: 1, 197: 10, 240: 1, 241: 1}
    for idx, name, up, admin, mbps in ports:
        kind = "10GBaseSFP" if mbps == 10000 else "1000BaseTX"
        interface(a, idx, name, f"Extreme Networks Virtual Services Platform 4850GTS - {kind} Port {name}",
                  6, up, mbps, alias="uplink to core" if idx == 192 else "", admin_up=admin,
                  in_octets=idx * 100000, out_octets=idx * 200000)
        a.i(f"1.3.6.1.2.1.17.1.4.1.2.{idx}", idx)  # bridge port number = ifIndex
        a.i(f"1.3.6.1.4.1.2272.1.4.10.1.1.13.{idx}", 2 if up else 1)  # down ports report half
        a.i(f"1.3.6.1.4.1.2272.1.4.10.1.1.15.{idx}", mbps if up else 0)
        a.i(f"1.3.6.1.4.1.2272.1.3.3.1.7.{idx}", pvid[idx])
        a.i(f"1.3.6.1.4.1.2272.1.3.3.1.8.{idx}", tagging[idx])
        a.i(f"1.0.8802.1.1.2.1.3.7.1.2.{idx}", 5)
        a.s(f"1.0.8802.1.1.2.1.3.7.1.3.{idx}", name)
    vlans = {1: "Default", 10: "Office", 20: "Voice"}
    for vid, name in vlans.items():
        interface(a, 2048 + vid, name, f"VLAN - {name}", 135, True, 1000)
    members = {1: [192, 240, 241, 196], 10: [192, 193, 195, 197], 20: [192, 194, 197]}
    for vid, name in vlans.items():
        a.s(f"1.3.6.1.4.1.2272.1.3.2.1.2.{vid}", name)
        a.x(f"1.3.6.1.4.1.2272.1.3.2.1.11.{vid}", bitmap(members[vid], 31))
    a.s("1.3.6.1.4.1.2272.1.17.10.1.2.1", "MLT-Uplink")
    a.x("1.3.6.1.4.1.2272.1.17.10.1.3.1", bitmap([240, 241], 31))
    # LLDP neighbour on trunk port 1/1 (ifIndex 192)
    a.s("1.0.8802.1.1.2.1.4.1.1.7.0.192.1", "1/12")
    a.s("1.0.8802.1.1.2.1.4.1.1.8.0.192.1", "Downlink to lab")
    a.s("1.0.8802.1.1.2.1.4.1.1.9.0.192.1", "core-sw")
    a.x("1.0.8802.1.1.2.1.4.1.1.12.0.192.1", [0x28, 0x00])
    a.write("voss.snmprec")


def demo(source, target):
    """A copy of an agent with growing counters (snmpsim "numeric" variation),
    for demonstrations: traffic between about 1 and 80 Mbit/s per port."""
    out = []
    with open(os.path.join(HERE, source)) as f:
        for line in f:
            oid, tag, value = line.rstrip("\n").split("|", 2)
            parts = oid.split(".")
            if oid == "1.3.6.1.2.1.1.3.0":
                line = f"{oid}|67:numeric|rate=100,initial={value}\n"
            elif oid.startswith(IFX + ".6.") or oid.startswith(IFX + ".10."):
                idx = int(parts[-1])
                rate = (idx * 1237 % 79 + 1) * 125000 // (2 if oid.startswith(IFX + ".10.") else 1)
                line = f"{oid}|70:numeric|rate={rate},initial={value}\n"
            out.append(line)
    with open(os.path.join(HERE, target), "w") as f:
        f.writelines(out)


if __name__ == "__main__":
    generic()
    ers5600()
    cisco()
    tplink()
    tplink_qbridge()
    voss()
    demo("ers5600.snmprec", "demo-ers5600.snmprec")
    demo("generic.snmprec", "demo-generic.snmprec")
