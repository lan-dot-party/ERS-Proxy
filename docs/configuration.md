# Configuration

`config.yaml` holds process settings only. Devices, groups and users live in the database and
are managed in the web UI or with `ers-proxy device …` / `ers-proxy user …`.

The file is found via `--config`/`-c`, else `ERSPROXY_CONFIG`, else
`/etc/ers-proxy/config.yaml`. Relative paths are relative to the file. `ers-proxy config init`
writes a commented example ([config.example.yaml](config.example.yaml)).

| Key | Default | Meaning |
|---|---|---|
| `listen` | `127.0.0.1:9050` | HTTP listen address. Behind a reverse proxy, keep it on localhost. |
| `public_url` | `http://<listen>` | URL as the browser sees it: scheme, host, port, **no path**. `https://` marks cookies `Secure`. Every form submission must come from this origin. |
| `trusted_proxies` | `[127.0.0.1, ::1]` | Addresses/networks whose `X-Real-IP` / `X-Forwarded-For` are trusted. |
| `secret_key_file` | `secret.key` | AES-256 key (64 hex characters) for stored switch passwords. Created if missing. Losing it makes stored passwords unusable (devices keep working without auto-login). |
| `storage.path` | `data/ers-proxy.db` | SQLite database. |
| `log.level` | `info` | `debug`, `info`, `warn`, `error`. `debug` logs every request (proxy tokens redacted) and applied patches. |
| `log.format` | `text` | `text` or `json`. |
| `tls.cert_file`, `tls.key_file` | – | Built-in HTTPS instead of a reverse proxy. Both or none. |
| `proxy.request_timeout` | `60s` | Maximum time for one request to a switch (connect, TLS, response headers). |
| `proxy.idle_timeout` | `2h` | An opened device (proxy session) ends after this time without requests. |
| `proxy.max_conns_per_device` | `4` | Parallel connections to one switch; old web servers handle only a few. |
| `proxy.max_body_bytes` | `16777216` | Limit for request bodies and for responses that are patched. |
| `health.enabled` | `true` | Reachability checks (status dots in the sidebar). |
| `health.interval` | `60s` | Time between two checks of a device (at least 10s). |
| `health.timeout` | `5s` | Timeout of one check; also used for the certificate probe. |
| `ui.default_open_mode` | `embedded` | How devices open unless the user or the device chooses: `embedded` (tab in the workspace), `tab` (new browser tab), `window` (popup). |
| `monitor.retention.minute` | `48h` | How long the 1-minute resolution is kept (at least `1h`). |
| `monitor.retention.quarter` | `720h` | How long the 15-minute resolution is kept (not shorter than `minute`). |
| `monitor.retention.hour` | `8760h` | How long the 1-hour resolution is kept (not shorter than `quarter`). |
| `monitor.retention.events` | `8760h` | How long port events (link changes etc.) are kept (at least `24h`). |

### History size

Every physical port and LAG keeps one row per bucket (about 100 bytes each, including the
index): `rows per port = 2880 (minutes in 48 h) + 2880 (quarters in 30 days) + 8760 (hours in
1 year)` with the defaults, plus events. Ten switches with 50 ports are about 7.3 million rows,
roughly 0.7 GB. On small systems (Raspberry Pi, small containers) lower `monitor.retention.quarter`
and `monitor.retention.hour`, for example to `168h` and `2160h`.

## Environment variables

| Variable | Overrides |
|---|---|
| `ERSPROXY_CONFIG` | path of `config.yaml` |
| `ERSPROXY_LISTEN` | `listen` |
| `ERSPROXY_PUBLIC_URL` | `public_url` |
| `ERSPROXY_LOG_LEVEL` | `log.level` |
| `GODEBUG=rsa1024min=0` | (Go runtime) accept RSA keys below 1024 bits on switches |

## Device settings

Per device (web UI or import file):

| Field | Values | Meaning |
|---|---|---|
| kind | `ers-edm`, `link` | Through the EDM proxy, or opened directly. |
| url | `http(s)://host[:port]` | For `ers-edm` without a path. For `link` the full URL. |
| open_mode | `inherit`, `embedded`, `tab`, `window` | `inherit` uses the user's default, then `ui.default_open_mode`. |
| tls_mode | `auto`, `modern`, `legacy` | `auto` tries TLS 1.2+ first and falls back to TLS 1.0–1.2 with old ciphers; the result is remembered. |
| tls_pin | 64 hex characters | SHA-256 of the switch certificate; set by confirming it in the UI. |
| isolation | `sandbox`, `off` | `off` disables the browser sandbox for this device (only if an EDM feature fails in it; see security.md). |
| health | `true`, `false` | Include in the reachability checks. |
| monitor_source | `none`, `snmp`, `edm` | Data source of the port dashboard. `edm` only for kind `ers-edm` (experimental). Default `none`. |
| monitor_driver | `auto`, `generic`, `ers`, `cisco`, `tplink` | Which driver interprets the data. Default `auto`. |
| monitor_interval | 30–3600 (seconds) | Polling interval; at least 60 for source `edm`. Default 60. |
| snmp_version | `v2c`, `v3` | SNMP version. Default `v2c`. |
| snmp_port | 1–65535 | UDP port of the SNMP agent. Default 161. |

The SNMP community or v3 login is set in the web UI (card "SNMP access" on the edit page,
write-only, password re-entry required). It is never part of an export or import file. Test it
with `ers-proxy snmp-test <device-id>`, or ad hoc with
`ERSPROXY_SNMP_COMMUNITY=… ers-proxy snmp-test --host H [--port 161]`. ERS-Proxy only reads via
SNMP; it never sends SET requests.
