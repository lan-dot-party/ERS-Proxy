package monitor

// Objects maps the MIB object names that drivers use to their OIDs (without
// instance suffix). The SNMP reader needs it; the EDM reader passes the names
// to the switch as they are. OIDs are public facts from the standard and
// vendor MIBs; no MIB files are shipped.
//
// Scalars are read with ".0" appended.
var Objects = map[string]string{
	// SNMPv2-MIB system group (scalars)
	"sysDescr":    "1.3.6.1.2.1.1.1",
	"sysObjectID": "1.3.6.1.2.1.1.2",
	"sysUpTime":   "1.3.6.1.2.1.1.3",
	"sysName":     "1.3.6.1.2.1.1.5",
	"sysLocation": "1.3.6.1.2.1.1.6",

	// IF-MIB ifTable
	"ifDescr":       "1.3.6.1.2.1.2.2.1.2",
	"ifType":        "1.3.6.1.2.1.2.2.1.3",
	"ifSpeed":       "1.3.6.1.2.1.2.2.1.5",
	"ifAdminStatus": "1.3.6.1.2.1.2.2.1.7",
	"ifOperStatus":  "1.3.6.1.2.1.2.2.1.8",
	"ifLastChange":  "1.3.6.1.2.1.2.2.1.9",
	"ifInOctets":    "1.3.6.1.2.1.2.2.1.10",
	"ifInDiscards":  "1.3.6.1.2.1.2.2.1.13",
	"ifInErrors":    "1.3.6.1.2.1.2.2.1.14",
	"ifOutOctets":   "1.3.6.1.2.1.2.2.1.16",
	"ifOutDiscards": "1.3.6.1.2.1.2.2.1.19",
	"ifOutErrors":   "1.3.6.1.2.1.2.2.1.20",
	// IF-MIB ifXTable
	"ifName":                     "1.3.6.1.2.1.31.1.1.1.1",
	"ifHCInOctets":               "1.3.6.1.2.1.31.1.1.1.6",
	"ifHCOutOctets":              "1.3.6.1.2.1.31.1.1.1.10",
	"ifHighSpeed":                "1.3.6.1.2.1.31.1.1.1.15",
	"ifAlias":                    "1.3.6.1.2.1.31.1.1.1.18",
	"ifCounterDiscontinuityTime": "1.3.6.1.2.1.31.1.1.1.19",

	// EtherLike-MIB
	"dot3StatsDuplexStatus": "1.3.6.1.2.1.10.7.2.1.19",

	// POWER-ETHERNET-MIB (RFC 3621)
	"pethPsePortAdminEnable":          "1.3.6.1.2.1.105.1.1.1.3",
	"pethPsePortDetectionStatus":      "1.3.6.1.2.1.105.1.1.1.6",
	"pethPsePortPowerClassifications": "1.3.6.1.2.1.105.1.1.1.10",
	"pethMainPsePower":                "1.3.6.1.2.1.105.1.3.1.1.2",
	"pethMainPseConsumptionPower":     "1.3.6.1.2.1.105.1.3.1.1.4",

	// IEEE8023-LAG-MIB
	"dot3adAggPortAttachedAggID": "1.2.840.10006.300.43.1.2.1.1.13",

	// LLDP-MIB (IEEE 802.1AB)
	"lldpLocPortIdSubtype": "1.0.8802.1.1.2.1.3.7.1.2",
	"lldpLocPortId":        "1.0.8802.1.1.2.1.3.7.1.3",
	"lldpLocPortDesc":      "1.0.8802.1.1.2.1.3.7.1.4",
	"lldpRemPortId":        "1.0.8802.1.1.2.1.4.1.1.7",
	"lldpRemPortDesc":      "1.0.8802.1.1.2.1.4.1.1.8",
	"lldpRemSysName":       "1.0.8802.1.1.2.1.4.1.1.9",
	"lldpRemSysCapEnabled": "1.0.8802.1.1.2.1.4.1.1.12",

	// BRIDGE-MIB
	"dot1dBasePortIfIndex": "1.3.6.1.2.1.17.1.4.1.2",

	// TPLINK-DOT1Q-VLAN-MIB (JetStream/Omada switches without Q-BRIDGE); names
	// are ERS-Proxy's own. Port table index = ifIndex, VLAN table index = VLAN ID.
	"tpVlanPortName":   "1.3.6.1.4.1.11863.6.14.1.1.1.1.1",
	"tpVlanPortPvid":   "1.3.6.1.4.1.11863.6.14.1.1.1.1.2",
	"tpVlanPortLag":    "1.3.6.1.4.1.11863.6.14.1.1.1.1.5",
	"tpVlanName":       "1.3.6.1.4.1.11863.6.14.1.2.1.1.2",
	"tpVlanTagPorts":   "1.3.6.1.4.1.11863.6.14.1.2.1.1.3",
	"tpVlanUntagPorts": "1.3.6.1.4.1.11863.6.14.1.2.1.1.4",
	// TPLINK-POWER-OVER-ETHERNET-MIB: values in 0.1 W; port table index = port number.
	"tpPoeSystemPowerLimit":       "1.3.6.1.4.1.11863.6.56.1.1.1.1",
	"tpPoeSystemPowerConsumption": "1.3.6.1.4.1.11863.6.56.1.1.1.3",
	"tpPoePortStatus":             "1.3.6.1.4.1.11863.6.56.1.1.2.1.1.2",
	"tpPoePortPower":              "1.3.6.1.4.1.11863.6.56.1.1.2.1.1.7",
	"tpPoePortClass":              "1.3.6.1.4.1.11863.6.56.1.1.2.1.1.10",
	"tpPoePortPowerStatus":        "1.3.6.1.4.1.11863.6.56.1.1.2.1.1.11",

	// Q-BRIDGE-MIB
	"dot1qPvid":                     "1.3.6.1.2.1.17.7.1.4.5.1.1",
	"dot1qVlanStaticName":           "1.3.6.1.2.1.17.7.1.4.3.1.1",
	"dot1qVlanStaticEgressPorts":    "1.3.6.1.2.1.17.7.1.4.3.1.2",
	"dot1qVlanStaticUntaggedPorts":  "1.3.6.1.2.1.17.7.1.4.3.1.4",
	"dot1qVlanCurrentEgressPorts":   "1.3.6.1.2.1.17.7.1.4.2.1.4",
	"dot1qVlanCurrentUntaggedPorts": "1.3.6.1.2.1.17.7.1.4.2.1.5",

	// Cisco: CISCO-VTP-MIB, CISCO-VLAN-MEMBERSHIP-MIB
	"vtpVlanState":                "1.3.6.1.4.1.9.9.46.1.3.1.1.2",
	"vtpVlanName":                 "1.3.6.1.4.1.9.9.46.1.3.1.1.4",
	"vlanTrunkPortVlansEnabled":   "1.3.6.1.4.1.9.9.46.1.6.1.1.4",
	"vlanTrunkPortNativeVlan":     "1.3.6.1.4.1.9.9.46.1.6.1.1.5",
	"vlanTrunkPortDynamicStatus":  "1.3.6.1.4.1.9.9.46.1.6.1.1.14",
	"vlanTrunkPortVlansEnabled2k": "1.3.6.1.4.1.9.9.46.1.6.1.1.17",
	"vlanTrunkPortVlansEnabled3k": "1.3.6.1.4.1.9.9.46.1.6.1.1.18",
	"vlanTrunkPortVlansEnabled4k": "1.3.6.1.4.1.9.9.46.1.6.1.1.19",
	"vmVlan":                      "1.3.6.1.4.1.9.9.68.1.2.2.1.2",

	// Avaya/Nortel ERS (RAPID-CITY, S5-CHASSIS); from the firmware's agent table
	"rcVlanName":               "1.3.6.1.4.1.2272.1.3.2.1.2",
	"rcVlanPortMembers":        "1.3.6.1.4.1.2272.1.3.2.1.11",
	"rcVlanStgId":              "1.3.6.1.4.1.2272.1.3.2.1.9",
	"rcVlanType":               "1.3.6.1.4.1.2272.1.3.2.1.10",
	"rcVlanPortDefaultVlanId":  "1.3.6.1.4.1.2272.1.3.3.1.7",
	"rcVlanPortPerformTagging": "1.3.6.1.4.1.2272.1.3.3.1.8",
	// VOSS only: TruthValue, 1 = a tagging port sends its PVID untagged.
	"rcVlanPortUntagDefaultVlan":  "1.3.6.1.4.1.2272.1.3.3.1.13",
	"rcPortOperDuplex":            "1.3.6.1.4.1.2272.1.4.10.1.1.13",
	"rcPortOperSpeed":             "1.3.6.1.4.1.2272.1.4.10.1.1.15",
	"rcMltName":                   "1.3.6.1.4.1.2272.1.17.10.1.2",
	"rcMltPortMembers":            "1.3.6.1.4.1.2272.1.17.10.1.3",
	"s5ChasComType":               "1.3.6.1.4.1.45.1.6.3.3.1.1.4",
	"s5ChasComDescr":              "1.3.6.1.4.1.45.1.6.3.3.1.1.5",
	"s5ChasComVer":                "1.3.6.1.4.1.45.1.6.3.3.1.1.6",
	"s5ChasComSerNum":             "1.3.6.1.4.1.45.1.6.3.3.1.1.7",
	"s5ChasComRunningSoftwareVer": "1.3.6.1.4.1.45.1.6.3.3.1.1.19",
	// MIKROTIK-MIB scalars (RouterOS and SwOS)
	"mtxrSerialNumber":    "1.3.6.1.4.1.14988.1.1.7.3",
	"mtxrFirmwareVersion": "1.3.6.1.4.1.14988.1.1.7.4",
	// IP-MIB ipAddrTable: interfaces with an IP address (read by
	// internal/switchcfg to find the management VLAN)
	"ipAdEntIfIndex": "1.3.6.1.2.1.4.20.1.2",
	// RAPID-CITY scalars of VSP/VOSS switches
	"rcSysVersion":       "1.3.6.1.4.1.2272.1.1.7",
	"rcChasSerialNumber": "1.3.6.1.4.1.2272.1.4.2",
	// Grandstream GWN switch scalars below 1.3.6.1.4.1.42397.1.3.1 (no public
	// MIB; names chosen here, meaning seen on a GWN7721)
	"gwnProductModel":    "1.3.6.1.4.1.42397.1.3.1.1",
	"gwnFirmwareVersion": "1.3.6.1.4.1.42397.1.3.1.4",
}

// Scalar reports whether a name is a scalar object (read with ".0").
func Scalar(name string) bool {
	switch name {
	case "sysDescr", "sysObjectID", "sysUpTime", "sysName", "sysLocation", "rcSysVersion", "rcChasSerialNumber",
		"mtxrSerialNumber", "mtxrFirmwareVersion", "gwnProductModel", "gwnFirmwareVersion":
		return true
	}
	return false
}
