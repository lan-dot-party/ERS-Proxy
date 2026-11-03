# ERS-Proxy

**One portal for the web interfaces of your switches – and a fix that makes the EDM of old
Avaya/Extreme ERS switches work in today's browsers.**

> Status: in development, see [docs/ROADMAP.md](docs/ROADMAP.md).

## What it does

- **Repairs EDM in modern browsers.** The Enterprise Device Manager built into ERS 5600 (and
  related ERS families) breaks in current Firefox, Chrome and Edge: Ext JS 2.2 reads response
  headers case-sensitively, modern browsers report them in lower case, and every request
  fails after the login. ERS-Proxy fetches EDM from the switch, fixes the affected files on
  the fly and serves them to your browser. Nothing is changed on the switch.
- **All devices in one place.** Add your switches on the left – ERS switches through the
  proxy, any other device (modern switches, firewalls, access points) as a direct link. Their
  web interfaces open on the right in tabs, in a new browser tab or in a popup window; each
  user and each device can choose.
- **Safe by design.** Switch content runs in a browser sandbox and cannot reach the portal or
  other switches; switch cookies never reach the browser; stored switch passwords are
  encrypted; HTTPS to the switch works with the old TLS versions these devices speak, with
  certificate pinning instead of blind trust. See [docs/security.md](docs/security.md).
- **Port dashboard.** Every port of every monitored switch at a glance: link, speed, duplex,
  VLANs, PoE, LAG, LLDP neighbour and traffic with history – read-only via SNMP, or via the EDM
  interface on ERS switches. Vendors plug in as drivers (generic Q-BRIDGE, ERS, Cisco, TP-Link).
- **Easy to run.** One static binary, SQLite, packages for Debian/Ubuntu, Fedora/RHEL, Arch and
  Alpine, and a Docker image. Works behind a reverse proxy on a single domain and port.

## Install

Packages are attached to each [GitHub release](https://github.com/lan-dot-party/ERS-Proxy/releases):

```bash
curl -LO https://github.com/lan-dot-party/ERS-Proxy/releases/download/vX.Y.Z/ers-proxy_X.Y.Z_amd64.deb
sudo apt install ./ers-proxy_X.Y.Z_amd64.deb
sudo nano /etc/ers-proxy/config.yaml              # listen, public_url
sudo ers-proxy user create admin --role admin
sudo systemctl enable --now ers-proxy
```

Details, Docker and reverse proxy setup: [docs/installation.md](docs/installation.md),
[docs/reverse-proxy.md](docs/reverse-proxy.md), [docs/configuration.md](docs/configuration.md).

## Supported devices

See [docs/compatibility.md](docs/compatibility.md). Please report your model and firmware with
the "Device report" issue template – the diagnostics page shows what to paste.

## Documentation

| Document | Content |
|---|---|
| [docs/ROADMAP.md](docs/ROADMAP.md) | Phases, steps and current status |
| [docs/architecture.md](docs/architecture.md) | Packages, request flow, data model |
| [docs/security.md](docs/security.md) | Threat model, mitigations, residual risks |
| [docs/configuration.md](docs/configuration.md) | `config.yaml` reference |
| [docs/installation.md](docs/installation.md) | Packages, Docker, first steps |
| [docs/reverse-proxy.md](docs/reverse-proxy.md) | Nginx, Caddy, Apache |
| [docs/compatibility.md](docs/compatibility.md) | Verified switches and firmware |
| [docs/monitoring.md](docs/monitoring.md) | Port dashboard: enabling, testing, history, troubleshooting |
| [docs/drivers.md](docs/drivers.md) | Writing a port dashboard driver for another vendor |
| [docs/hardware-tests.md](docs/hardware-tests.md) | Checklist for verifying a switch model on real hardware |

## Development

```bash
go test ./...
CGO_ENABLED=0 go build -o bin/ers-proxy ./cmd/ers-proxy
./bin/ers-proxy -c config.yaml config init
echo 'a-long-password' | ./bin/ers-proxy -c config.yaml user create admin --role admin
./bin/ers-proxy -c config.yaml server          # http://127.0.0.1:9050
```

See [CONTRIBUTING.md](CONTRIBUTING.md).

## Related projects

ERS-Proxy is part of the lan-dot-party tool suite with
[LANLeecher](https://github.com/lan-dot-party/LANLeecher),
[LANPilot](https://github.com/lan-dot-party/LANPilot) and
[FlowGauge](https://github.com/lan-dot-party/FlowGauge).

## License

MIT, see [LICENSE](LICENSE). Avaya, Extreme Networks, ERS and EDM are trademarks of their
owners; this project is not affiliated with them and ships none of their files.
