package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"text/tabwriter"
	"time"

	"github.com/gosnmp/gosnmp"
	"github.com/spf13/cobra"

	"github.com/lan-dot-party/ers-proxy/internal/devices"
	"github.com/lan-dot-party/ers-proxy/internal/monitor"
	"github.com/lan-dot-party/ers-proxy/internal/monitor/drivers"
	"github.com/lan-dot-party/ers-proxy/internal/monitor/snmpsrc"
	"github.com/lan-dot-party/ers-proxy/internal/snmp"
)

const snmpCommunityEnv = "ERSPROXY_SNMP_COMMUNITY"

func init() {
	addTargetFlags(snmpTestCmd)
	snmpTestCmd.Flags().Bool("collect", false, "also run the dashboard driver and print the port table")
	addTargetFlags(snmpCaptureCmd)
	snmpCaptureCmd.Flags().String("out", "", "write to this file (default stdout; never overwrites)")
	snmpCaptureCmd.Flags().Bool("raw", false, "do not anonymise names, aliases, serial numbers and neighbours")
	rootCmd.AddCommand(snmpTestCmd, snmpCaptureCmd)
}

var snmpTestCmd = &cobra.Command{
	Use:   "snmp-test [device-id]",
	Short: "Read the system group and count the interfaces of a device via SNMP (read-only)",
	Long: `Uses the SNMP credentials stored for the device (device ID), or, with --host,
an ad-hoc target whose v2c community is read from the environment variable
` + snmpCommunityEnv + ` (never from an argument). Only reads; timeout 5 s, 1 retry.`,
	Args: cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		target, err := resolveSNMPTarget(cmd, args)
		if err != nil {
			return err
		}
		if collect, _ := cmd.Flags().GetBool("collect"); collect {
			return runCollect(cmd.Context(), target)
		}
		return runSNMPTest(cmd.Context(), target)
	},
}

// addTargetFlags adds the ad-hoc target flags shared by the SNMP commands.
func addTargetFlags(c *cobra.Command) {
	c.Flags().String("host", "", "ad-hoc target: host to query (community from $"+snmpCommunityEnv+")")
	c.Flags().Int("port", 161, "ad-hoc target: SNMP port")
	c.Flags().String("version", "v2c", "ad-hoc target: SNMP version (v2c)")
}

// resolveSNMPTarget returns the target of a device ID or of --host.
func resolveSNMPTarget(cmd *cobra.Command, args []string) (snmp.Target, error) {
	f := cmd.Flags()
	host, _ := f.GetString("host")
	var target snmp.Target
	switch {
	case len(args) == 1 && host != "":
		return target, errors.New("give either a device ID or --host, not both")
	case len(args) == 1:
		id, err := strconv.ParseInt(args[0], 10, 64)
		if err != nil || id <= 0 {
			return target, fmt.Errorf("invalid device ID %q", args[0])
		}
		if target, err = deviceTarget(cmd.Context(), id); err != nil {
			return target, err
		}
	case host != "":
		port, _ := f.GetInt("port")
		version, _ := f.GetString("version")
		community := os.Getenv(snmpCommunityEnv)
		if community == "" {
			return target, errors.New("set the community in the environment variable " + snmpCommunityEnv)
		}
		target = snmp.Target{Host: host, Port: port, Version: version, Creds: snmp.Credentials{Community: community}}
	default:
		return target, errors.New("give a device ID or --host")
	}
	target.Timeout, target.Retries = 5*time.Second, 1
	return target, nil
}

var snmpCaptureCmd = &cobra.Command{
	Use:   "snmp-capture [device-id]",
	Short: "Record the SNMP data the dashboard drivers use as an anonymised .snmprec file (read-only)",
	Long: `Walks every object the port dashboard drivers read and writes the answers in
snmpsim's .snmprec format. Attach the file to a "Device report" issue so that the
device can be reproduced in tests. Names, aliases, serial numbers, VLAN names and
LLDP neighbours are replaced unless --raw is given; check the file before sharing.
Sends only GET/GETBULK requests.`,
	Args: cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		target, err := resolveSNMPTarget(cmd, args)
		if err != nil {
			return err
		}
		raw, _ := cmd.Flags().GetBool("raw")
		out, _ := cmd.Flags().GetString("out")
		w := os.Stdout
		if out != "" && out != "-" {
			f, err := os.OpenFile(out, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o600)
			if err != nil {
				return err
			}
			defer f.Close()
			w = f
		}
		ctx, cancel := context.WithTimeout(cmd.Context(), 5*time.Minute)
		defer cancel()
		n, skipped, err := snmpsrc.Capture(ctx, target, w, !raw)
		if err != nil {
			return err
		}
		for _, s := range skipped {
			fmt.Fprintln(os.Stderr, "skipped", s)
		}
		fmt.Fprintf(os.Stderr, "%d values captured\n", n)
		return nil
	},
}

// runCollect shows what the detected driver reads from a device.
func runCollect(ctx context.Context, t snmp.Target) error {
	r, err := snmpsrc.Open(ctx, t)
	if err != nil {
		return err
	}
	defer r.Close()
	sys, err := monitor.ReadSystem(ctx, r)
	if err != nil {
		return err
	}
	d, err := monitor.NewRegistry(drivers.All()...).Detect(sys)
	if err != nil {
		return err
	}
	snap, err := d.Collect(ctx, r)
	if err != nil {
		return err
	}
	fmt.Printf("driver: %s, %d interfaces, %d VLANs, %d units\n", d.Name(), len(snap.Ports), len(snap.VLANs), len(snap.Units))
	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "PORT\tNAME\tSTATE\tSPEED\tDUPLEX\tPVID\tUNTAGGED\tTAGGED\tPOE\tLAG\tNEIGHBOR")
	for _, p := range snap.Ports {
		if !p.Physical {
			continue
		}
		poe, nb := "-", "-"
		if p.PoE != nil {
			poe = p.PoE.Status
		}
		if p.Neighbor != nil {
			nb = p.Neighbor.SysName
		}
		fmt.Fprintf(tw, "%d/%d\t%s\t%s/%s\t%d\t%s\t%d\t%v\t%v\t%s\t%s\t%s\n", p.Unit, p.Number, p.Name, p.Admin, p.Oper,
			p.Speed/1_000_000, p.Duplex, p.PVID, p.Untagged, p.Tagged, poe, p.LAGName, nb)
	}
	for _, n := range snap.Notes {
		fmt.Println("note:", n)
	}
	return tw.Flush()
}

func deviceTarget(ctx context.Context, id int64) (snmp.Target, error) {
	e, err := open(ctx)
	if err != nil {
		return snmp.Target{}, err
	}
	defer e.close()
	d, err := e.db.DeviceByID(ctx, id)
	if err != nil {
		return snmp.Target{}, fmt.Errorf("device %d: %w", id, err)
	}
	if !d.HasSNMPCredentials() {
		return snmp.Target{}, errors.New("no SNMP credentials stored for this device (set them in the web UI)")
	}
	creds, err := devices.DecryptSNMP(e.key, d)
	if err != nil {
		return snmp.Target{}, fmt.Errorf("decrypt SNMP credentials: %w", err)
	}
	host, _ := devices.HostPort(d.URL)
	return snmp.Target{Host: host, Port: d.SNMPPort, Version: d.SNMPVersion, Creds: creds}, nil
}

func runSNMPTest(ctx context.Context, t snmp.Target) error {
	g, err := snmp.Dial(ctx, t, 0)
	if err != nil {
		return err
	}
	defer snmp.Close(g)
	res, err := g.Get([]string{
		"1.3.6.1.2.1.1.5.0", "1.3.6.1.2.1.1.1.0", "1.3.6.1.2.1.1.2.0", "1.3.6.1.2.1.1.3.0",
	})
	if err != nil {
		return fmt.Errorf("query %s:%d: %w", t.Host, t.Port, err)
	}
	vals := map[string]gosnmp.SnmpPDU{}
	for _, v := range res.Variables {
		vals[v.Name] = v
	}
	fmt.Println("sysName:     ", snmp.String(vals[".1.3.6.1.2.1.1.5.0"]))
	fmt.Println("sysDescr:    ", snmp.String(vals[".1.3.6.1.2.1.1.1.0"]))
	if oid, ok := vals[".1.3.6.1.2.1.1.2.0"].Value.(string); ok {
		fmt.Println("sysObjectID: ", oid)
	} else {
		fmt.Println("sysObjectID:  (not available)")
	}
	if ticks, ok := snmp.Uint64(vals[".1.3.6.1.2.1.1.3.0"]); ok {
		fmt.Println("sysUpTime:   ", (time.Duration(ticks) * 10 * time.Millisecond).Round(time.Second))
	} else {
		fmt.Println("sysUpTime:    (not available)")
	}
	rows, err := snmp.WalkColumn(g, "1.3.6.1.2.1.2.2.1.2") // ifDescr (some agents omit ifIndex)
	if err != nil {
		return fmt.Errorf("walk ifTable: %w", err)
	}
	fmt.Println("ifTable rows:", len(rows))
	return nil
}
