package cmd

import (
	"context"
	"errors"
	"time"

	"github.com/lan-dot-party/ers-proxy/internal/devices"
	"github.com/lan-dot-party/ers-proxy/internal/monitor"
	"github.com/lan-dot-party/ers-proxy/internal/monitor/edmsrc"
	"github.com/lan-dot-party/ers-proxy/internal/monitor/poller"
	"github.com/lan-dot-party/ers-proxy/internal/snmp"
	"github.com/lan-dot-party/ers-proxy/internal/store"
	"github.com/lan-dot-party/ers-proxy/internal/switchcfg"
	"github.com/lan-dot-party/ers-proxy/internal/switchcfg/edmwrite"
	"github.com/lan-dot-party/ers-proxy/internal/switchcfg/rcplan"
	"github.com/lan-dot-party/ers-proxy/internal/switchcfg/snmpwrite"
)

// newConfigure builds the configuration manager. It is only called when
// configure.enabled is set in config.yaml; otherwise nothing of
// internal/switchcfg runs and the UI has no configuration routes.
func newConfigure(e *env, es *edmSessions, mon *poller.Poller) *switchcfg.Manager {
	return &switchcfg.Manager{
		Enabled:  e.cfg.Configure.Enabled,
		Timeout:  e.cfg.Configure.ConfirmTimeout,
		DB:       e.db,
		Monitor:  mon,
		Open:     configOpener(e, es),
		Planners: switchcfg.NewPlanners(rcplan.VOSS()),
		Log:      e.log.With("component", "configure"),
		// Automatic actions (revert on timeout, recovery after a restart);
		// actions of users are audited by the web layer with their IP.
		Audit: func(ctx context.Context, action, target, details string) {
			if err := e.db.Audit(context.WithoutCancel(ctx), store.AuditEntry{Actor: "system", Action: action,
				Target: target, Details: details}); err != nil {
				e.log.Error("audit", "action", action, "err", err)
			}
		},
	}
}

// configOpener opens the configuration channel of a device: the shared EDM
// session (reads and writes through EDM, like the dashboard's EDM source),
// or SNMP with the separate write credentials (reads use them too, so a
// read-back proves they work).
func configOpener(e *env, es *edmSessions) switchcfg.OpenFunc {
	return func(ctx context.Context, d *store.Device) (monitor.Reader, switchcfg.Writer, func(), error) {
		switch d.ConfigSource {
		case store.ConfigEDM:
			s, err := es.session(ctx, d)
			if err != nil {
				return nil, nil, nil, err
			}
			return edmsrc.Reader{S: s}, edmwrite.Writer{S: s.EDM()}, func() {}, nil
		case store.ConfigSNMP:
			if !d.HasSNMPWriteCredentials() {
				return nil, nil, nil, errors.New("no SNMP write credentials stored for this device")
			}
			creds, err := devices.DecryptSNMPWrite(e.key, d)
			if err != nil {
				return nil, nil, nil, err
			}
			r, closeR, err := poller.OpenSNMP(ctx, d, creds)
			if err != nil {
				return nil, nil, nil, err
			}
			host, _ := devices.HostPort(d.URL)
			w, err := snmpwrite.Open(ctx, snmp.Target{Host: host, Port: d.SNMPPort, Version: d.SNMPVersion, Creds: creds,
				Timeout: 5 * time.Second})
			if err != nil {
				closeR()
				return nil, nil, nil, err
			}
			return r, w, func() { w.Close(); closeR() }, nil
		}
		return nil, nil, nil, switchcfg.ErrDisabled
	}
}
