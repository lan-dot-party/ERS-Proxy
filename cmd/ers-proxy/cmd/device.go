package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/lan-dot-party/ers-proxy/internal/devices"
	"github.com/lan-dot-party/ers-proxy/internal/store"
	"github.com/lan-dot-party/ers-proxy/internal/upstream"
)

func init() {
	deviceAddCmd.Flags().String("name", "", "display name (required)")
	deviceAddCmd.Flags().String("url", "", "address, e.g. https://192.0.2.10 (required)")
	deviceAddCmd.Flags().String("kind", store.KindERS, "ers-edm (ERS, EDM through the proxy), vsp-edm (VSP/VOSS, EDM through the proxy), edm-direct (VSP/VOSS, EDM opened directly), swos (MikroTik SwOS through the proxy), gwn (Grandstream GWN, opened directly) or link (other device)")
	deviceAddCmd.Flags().String("group", "", "group name (created if missing)")
	deviceAddCmd.Flags().String("open-mode", store.OpenInherit, "inherit, embedded, tab or window")
	deviceAddCmd.Flags().String("notes", "", "optional notes")
	deviceExportCmd.Flags().String("format", "json", "json or csv")
	deviceCmd.AddCommand(deviceAddCmd, deviceListCmd, deviceImportCmd, deviceExportCmd, deviceDeleteCmd)
	rootCmd.AddCommand(deviceCmd, tlsProbeCmd)
}

var deviceCmd = &cobra.Command{Use: "device", Short: "Manage devices (the web UI can do the same)"}

var deviceAddCmd = &cobra.Command{
	Use:   "add",
	Short: "Add a device",
	RunE: func(cmd *cobra.Command, args []string) error {
		f := cmd.Flags()
		name, _ := f.GetString("name")
		rawURL, _ := f.GetString("url")
		kind, _ := f.GetString("kind")
		group, _ := f.GetString("group")
		mode, _ := f.GetString("open-mode")
		notes, _ := f.GetString("notes")
		if name == "" || rawURL == "" {
			return errors.New("--name and --url are required")
		}
		e, err := open(cmd.Context())
		if err != nil {
			return err
		}
		defer e.close()
		exp := &devices.Export{Version: devices.ExportVersion, Devices: []devices.ExportDevice{{
			Name: name, Group: group, Kind: kind, URL: rawURL, OpenMode: mode, Notes: notes}}}
		res, err := devices.Import(cmd.Context(), e.db, exp)
		if err != nil {
			return err
		}
		if len(res.Errors) > 0 {
			return errors.New(res.Errors[0])
		}
		if res.Skipped > 0 {
			return errors.New("a device with this name and address already exists")
		}
		e.audit(cmd.Context(), store.AuditEntry{Actor: "cli", Action: "device.create", Target: name, Details: kind + " " + rawURL})
		fmt.Println("device added; reload the web UI to see it")
		return nil
	},
}

var deviceListCmd = &cobra.Command{
	Use:   "list",
	Short: "List devices",
	RunE: func(cmd *cobra.Command, args []string) error {
		e, err := open(cmd.Context())
		if err != nil {
			return err
		}
		defer e.close()
		devs, err := e.db.ListDevices(cmd.Context())
		if err != nil {
			return err
		}
		tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "ID\tNAME\tGROUP\tKIND\tURL\tTLS")
		for _, d := range devs {
			tlsInfo := "-"
			if d.Upstream() && strings.HasPrefix(d.URL, "https://") {
				tlsInfo = d.TLSMode
				if d.TLSPin == "" {
					tlsInfo += ", not pinned"
				} else {
					tlsInfo += ", pinned"
				}
			}
			fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\t%s\n", d.ID, d.Name, d.GroupName, d.Kind, d.URL, tlsInfo)
		}
		return tw.Flush()
	},
}

var deviceImportCmd = &cobra.Command{
	Use:   "import <file.json|file.csv|->",
	Short: "Import devices from a JSON export or a CSV file (name,group,kind,url)",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		var r io.Reader = os.Stdin
		if args[0] != "-" {
			f, err := os.Open(args[0])
			if err != nil {
				return err
			}
			defer f.Close()
			r = f
		}
		var exp *devices.Export
		var err error
		if strings.HasSuffix(strings.ToLower(args[0]), ".csv") {
			exp, err = devices.ReadCSV(r)
		} else {
			exp, err = devices.ReadJSON(r)
		}
		if err != nil {
			return err
		}
		e, err := open(cmd.Context())
		if err != nil {
			return err
		}
		defer e.close()
		res, err := devices.Import(cmd.Context(), e.db, exp)
		if err != nil {
			return err
		}
		for _, msg := range res.Errors {
			fmt.Fprintln(os.Stderr, "skipped:", msg)
		}
		e.audit(cmd.Context(), store.AuditEntry{Actor: "cli", Action: "device.import", Target: args[0],
			Details: fmt.Sprintf("created=%d skipped=%d errors=%d", res.Created, res.Skipped, len(res.Errors))})
		fmt.Printf("%d created, %d already present, %d invalid, %d new groups\n", res.Created, res.Skipped, len(res.Errors), res.GroupsCreated)
		return nil
	},
}

var deviceExportCmd = &cobra.Command{
	Use:   "export",
	Short: "Write all groups and devices to stdout (without stored passwords)",
	RunE: func(cmd *cobra.Command, args []string) error {
		format, _ := cmd.Flags().GetString("format")
		e, err := open(cmd.Context())
		if err != nil {
			return err
		}
		defer e.close()
		exp, err := devices.Build(cmd.Context(), e.db)
		if err != nil {
			return err
		}
		switch format {
		case "json":
			return devices.WriteJSON(os.Stdout, exp)
		case "csv":
			return devices.WriteCSV(os.Stdout, exp)
		}
		return errors.New("--format must be json or csv")
	},
}

var deviceDeleteCmd = &cobra.Command{
	Use:   "delete <id>",
	Short: "Delete a device",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		var id int64
		if _, err := fmt.Sscan(args[0], &id); err != nil {
			return errors.New("id must be a number (see device list)")
		}
		e, err := open(cmd.Context())
		if err != nil {
			return err
		}
		defer e.close()
		d, err := e.db.DeviceByID(cmd.Context(), id)
		if err != nil {
			return fmt.Errorf("device %d: %w", id, err)
		}
		if err := e.db.DeleteDevice(cmd.Context(), id); err != nil {
			return err
		}
		e.audit(cmd.Context(), store.AuditEntry{Actor: "cli", Action: "device.delete", Target: d.Name, Details: d.URL})
		fmt.Println("device deleted")
		return nil
	},
}

var tlsProbeCmd = &cobra.Command{
	Use:   "tls-probe <https://switch[:port]>",
	Short: "Show which TLS versions and ciphers a device accepts, and its certificate fingerprint",
	Long: `Connects to the device with every TLS version that ERS-Proxy supports
(TLS 1.3 down to TLS 1.0, including the legacy cipher suites of old switches)
and prints what works. The certificate is not validated; compare the
fingerprint with the one shown on the switch before confirming it in the UI.
Does not need a configuration file.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		raw := args[0]
		if !strings.Contains(raw, "://") {
			raw = "https://" + raw
		}
		host, port := devices.HostPort(raw)
		if host == "" {
			return fmt.Errorf("invalid address %q", args[0])
		}
		ctx, cancel := context.WithTimeout(cmd.Context(), time.Minute)
		defer cancel()
		res := upstream.Probe(ctx, net.JoinHostPort(host, port), host, 8*time.Second)
		tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "PROFILE\tVERSION\tRESULT")
		for _, a := range res.Attempts {
			result := a.Cipher
			if a.Err != nil {
				result = "failed: " + a.Err.Error()
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\n", a.Profile, a.Version, result)
		}
		_ = tw.Flush()
		if res.Cert != nil {
			c := res.Cert
			fmt.Printf("\nCertificate\n  SHA-256:   %s\n  Subject:   %s\n  Issuer:    %s\n  Valid to:  %s\n  Key:       %d bit\n",
				c.Fingerprint, c.Subject, c.Issuer, c.NotAfter.Format("2006-01-02"), c.KeyBits)
		}
		if res.Best == nil {
			return errors.New("no TLS connection possible")
		}
		fmt.Printf("\nBest: %s with %s (profile %s)\n", res.Best.Version, res.Best.Cipher, res.Best.Profile)
		if res.Best.Profile == upstream.ProfileLegacy {
			fmt.Println("The device needs the legacy profile; tls_mode \"auto\" selects it automatically.")
		}
		return nil
	},
}
