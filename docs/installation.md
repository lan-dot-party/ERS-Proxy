# Installation

## Packages (recommended)

Every [release](https://github.com/lan-dot-party/ERS-Proxy/releases) has packages for amd64,
arm64 and armv7: `.deb` (Debian, Ubuntu), `.rpm` (Fedora, RHEL, Rocky), `.pkg.tar.zst` (Arch)
and `.apk` (Alpine), plus `SHA256SUMS` and a build provenance attestation.

```bash
V=X.Y.Z
curl -LO https://github.com/lan-dot-party/ERS-Proxy/releases/download/v$V/ers-proxy_${V}_amd64.deb
curl -LO https://github.com/lan-dot-party/ERS-Proxy/releases/download/v$V/SHA256SUMS
sha256sum --ignore-missing -c SHA256SUMS
gh attestation verify ers-proxy_${V}_amd64.deb --repo lan-dot-party/ERS-Proxy   # optional
sudo apt install ./ers-proxy_${V}_amd64.deb
```

The package

- installs `/usr/bin/ers-proxy` and the systemd unit `ers-proxy.service` (hardened: own user,
  read-only system, only `/var/lib/ers-proxy` writable),
- creates the system user `ers-proxy`, `/etc/ers-proxy/config.yaml` and the key
  `/etc/ers-proxy/secret.key` that encrypts stored switch passwords,
- does **not** start the service, so that you can check the configuration first.

```bash
sudo nano /etc/ers-proxy/config.yaml           # listen, public_url (see configuration.md)
sudo ers-proxy user create admin --role admin  # asks for the password
sudo systemctl enable --now ers-proxy
```

Then open `public_url` in the browser, log in, and add devices under **Settings → Devices**
(or import a CSV/JSON file).

Updates: install the newer package; the service restarts and applies database migrations.
Make a backup first: `sudo -u ers-proxy ers-proxy -c /etc/ers-proxy/config.yaml backup`.
Removal keeps `/var/lib/ers-proxy` and `/etc/ers-proxy`; `apt purge` deletes them.

## Docker

```bash
docker run -d --name ers-proxy -p 9050:9050 \
  -e ERSPROXY_PUBLIC_URL=https://ers.example.org \
  -v ers-proxy-data:/var/lib/ers-proxy \
  ghcr.io/lan-dot-party/ers-proxy:latest
docker exec -it ers-proxy ers-proxy user create admin --role admin
```

The container listens on `0.0.0.0:9050` without TLS; put a reverse proxy in front of it
([reverse-proxy.md](reverse-proxy.md)). The secret key is created in the data volume.

## From source

```bash
git clone https://github.com/lan-dot-party/ERS-Proxy && cd ERS-Proxy
CGO_ENABLED=0 go build -trimpath -o ers-proxy ./cmd/ers-proxy
./ers-proxy -c config.yaml config init
./ers-proxy -c config.yaml user create admin --role admin
./ers-proxy -c config.yaml server
```

## First device

*Settings › Devices › Add switch* opens one form for every kind of device; choose the kind
there. The section *Credentials* takes the switch login and SNMP access right away (optional;
your own password confirms them once).

- **ERS switch:** kind "ERS switch", address `http://<ip>` or `https://<ip>`. With HTTPS the
  first opening shows the certificate fingerprint; an admin confirms it once. Check what the
  switch supports with `ers-proxy tls-probe https://<ip>`.
- **VSP switch (VOSS):** kind "EDM switch, opened directly", address `https://<ip>`. Its EDM
  works in current browsers, so it is opened directly like a link. For the port dashboard use
  SNMP, or the EDM data source after confirming the certificate on the device's edit page
  (*Check and confirm certificate*) and storing an EDM login there.
- **MikroTik switch with SwOS:** kind "MikroTik SwOS (through proxy)", address `http://<ip>`
  (SwOS has no HTTPS). Enter the web login (`admin` and its password) under *Credentials* when
  adding the device, or later on its edit page – ERS-Proxy logs in for the browser. For the port dashboard choose the source "SwOS
  (web)" (VLANs, duplex, LAGs) or SNMP (ports and traffic only).
- **Any other device:** kind "Link", the full URL of its web interface. It opens directly; if
  the device forbids being embedded, ERS-Proxy opens it in a new browser tab.
