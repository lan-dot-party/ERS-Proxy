# Security

This document describes what ERS-Proxy protects, against whom, how, and what remains. Report
vulnerabilities as described in [SECURITY.md](../SECURITY.md).

## Assets

- Access to the management interfaces of network switches (configuration changes can take a
  network down).
- Switch passwords, if stored for auto-login, and SNMP credentials (read, and write if
  configuration changes over SNMP are used).
- The ERS-Proxy accounts and the device list (addresses of management interfaces).

## Threats

| # | Threat | Example |
|---|---|---|
| T1 | Unauthenticated access to the portal | Someone on the network opens the portal |
| T2 | Password guessing | Brute force against `/login` |
| T3 | Cross-site request forgery | A web page makes the browser change devices in the portal |
| T4 | **Malicious switch content** | A compromised switch serves JavaScript that tries to use the portal session, read the device list or control other switches |
| T5 | Theft of stored switch passwords | Copy of the database, or redirecting a device to an attacker's host |
| T6 | Man in the middle between ERS-Proxy and a switch | ARP spoofing in the management network |
| T7 | Leaking switch sessions | Switch cookies end up in the browser or in logs |
| T8 | **Misused or mistaken configuration changes** (only with `configure.enabled`) | A stolen admin session moves an uplink into another VLAN; an admin cuts off the management VLAN by mistake; a forged request applies a change the admin never saw |

## Mitigations

### Portal access (T1–T3)

- Every page except `/login`, `/health` and static files requires a login.
- Passwords: Argon2id (m=19 MiB, t=2, p=1), at least 10 characters, no default account. The
  first admin is created on the command line.
- Login lockout per IP address and per username (5 failures in 10 minutes lock for 5 minutes),
  one uniform error message, constant time for unknown users.
- Sessions are stored server-side as SHA-256 of the cookie value. The cookie is `HttpOnly`,
  `SameSite=Strict`, and `Secure` when `public_url` starts with `https://`. Sessions expire
  after 12 hours of inactivity; changing a password ends all sessions of the user.
- Every state-changing request (POST) needs the session's CSRF token and, if the browser sends
  an `Origin` header, it must match `public_url`. `Origin: null` is always rejected.
- Security headers on all portal pages: a strict Content Security Policy without inline
  scripts or styles, `frame-ancestors 'none'`, `X-Content-Type-Options: nosniff`,
  `Referrer-Policy: same-origin`.

### Switch content isolation (T4)

The proxy has to deliver HTML and JavaScript from the switch to the browser. If that content ran
under the portal's origin, it could use the portal like the logged-in user. ERS-Proxy must work
on a single domain and port (typical reverse proxy setups), so a separate origin is not an
option. Instead:

1. **Browser sandbox.** Every response under `/d/…` carries
   `Content-Security-Policy: sandbox allow-scripts allow-forms allow-popups allow-modals allow-downloads`
   – without `allow-same-origin`. The browser gives the document a unique, opaque origin. This
   applies to embedded tabs and to documents opened in a new browser tab or window.
2. **No portal session in the sandbox.** Requests from an opaque origin are cross-site; the
   `SameSite=Strict` session cookie is not sent. The portal additionally rejects
   `Origin: null` and requires the CSRF token.
3. **Unguessable path.** A device is opened through `/open/{id}` (needs a session). ERS-Proxy
   creates a random 128-bit token bound to the user's session and the device and redirects to
   `/d/{token}/`. The token expires after inactivity and with the session. Content of one switch
   cannot learn the token of another switch, so switches are isolated from each other as well.
4. **Cross-origin access only for the token path.** Because the sandboxed document has an opaque
   origin, its XHR requests to `/d/{token}/` are cross-origin. Only these responses carry
   `Access-Control-Allow-Origin: null`; no credentials are involved, the token is the only key.
5. **Framing.** Proxied documents may be framed only by the portal (`frame-ancestors 'self'`)
   and send no referrer.

The sandbox also blocks Web Storage. The shim replaces `localStorage` and `sessionStorage` with
a store that the proxy keeps per proxy session (in memory, at most 64 KiB, dropped with the
session). Only the document of that proxy session can read it (the token is the key), and it is
never sent to the switch. The proxy itself adds only the port to select (`x:port`, an
ifIndex checked as a number).

If a switch's EDM needs a browser feature that the sandbox forbids, an admin can set
`isolation: off` for that device. The portal then shows a permanent warning for the device.

Devices of kind `vsp-edm` (VSP switches with VOSS) and `swos` (MikroTik SwOS) are proxied
exactly like ERS switches. For `swos` devices ERS-Proxy also logs in on the server: SwOS uses
HTTP Digest, and the proxy answers the switch's challenge with the stored web login. The
browser's own `Authorization` header is never forwarded and the switch's `WWW-Authenticate`
header never reaches the browser, so neither a browser login dialog nor the password is
involved in the browser. Content of the sandboxed SwOS page can use the stored login only for
requests to its own switch below its own token path – which is what opening the device grants
anyway. Without a stored login a `swos` device is not opened.
Devices of kind `link` and `edm-direct` (VSP switches opened directly) are not proxied: the browser loads them from their own origin, never under the portal's, so this
section does not apply to them. For `edm-direct` devices ERS-Proxy only reads from the EDM
itself (EDM data source) with the pinned certificate and the stored login (T5, T6).

### Stored switch passwords (T5)

- Storing a switch login or SNMP credentials (and creating users) needs the admin's own
  password again (re-authentication). An unattended browser or a stolen session cookie alone
  cannot add or swap stored secrets; failed confirmations count towards the login lockout.
- Encrypted with AES-256-GCM. The key lives in a separate file (`secret_key_file`), so a copy of
  the database alone reveals nothing.
- The ciphertext is bound to the device ID, host and port as associated data. Changing the
  host of a device makes the stored password unusable; it cannot be redirected to another host.
- Password fields are write-only: they are never shown, returned or logged. Setting one requires
  re-entering the user's own password.

### Stored SNMP credentials (T5)

- The v2c community or v3 login is encrypted like the switch password (AES-256-GCM, separate key
  file) and bound to the device ID, host and SNMP port as associated data. Changing the host or
  the SNMP port makes it unusable; the portal deletes it then.
- The form is write-only: values are never shown, returned, logged, audited or exported.
  Setting or clearing requires re-entering the user's own password.
- **SNMPv2c sends the community unencrypted.** Use a read-only community that the switch
  restricts to this host, and prefer SNMPv3 with `authPriv`.
- These read credentials are only used for reading: `internal/snmp` has no SET API (checked
  by a test). Configuration changes over SNMP use separate write credentials (see T8), which
  exist only when an admin stores them.
- SNMP write credentials are stored the same way (AES-256-GCM, bound to device ID, host and
  SNMP port with their own associated data, so read and write ciphertexts cannot be swapped),
  write-only, need the admin's password, and are deleted when the host or SNMP port changes.

### Connection to the switch (T6)

- HTTPS is preferred. Switches use self-signed certificates, so ERS-Proxy pins the SHA-256
  fingerprint of the certificate when the device is added (trust on first use, confirmed by the
  admin). A different certificate blocks the connection until an admin confirms it again.
- TLS mode `auto` tries modern TLS first and falls back to the legacy profile (TLS 1.0–1.2,
  RSA key exchange, AES-CBC, 3DES, RC4) only if the switch supports nothing better. The device
  page shows the negotiated version and cipher and warns below TLS 1.2.
- With HTTP the connection is unencrypted, including the EDM login. The portal marks such
  devices. SwOS speaks HTTP only; its Digest login does not send the password itself, but MD5
  Digest can be attacked offline from a captured exchange – keep SwOS switches in a management
  network.
- The SwOS data source sends only GET requests for six read-only files and never asks for
  `snmp.b` (which contains the SNMP community); a test checks that the reader contains no write
  methods.

### Configuration changes (T8)

Changing switch settings from the dashboard ([switch-configuration.md](switch-configuration.md))
is off by default. Port monitoring alone never writes to a switch.

- **Opt-in in two places.** `configure.enabled: true` must be set in `config.yaml`; no
  environment variable, command-line flag or UI setting can turn it on (a test checks the
  environment). Without it the server registers no configuration routes, shows no controls
  and does not start the manager, which also refuses every operation itself. In addition each
  device needs a configuration channel (`config_source`: EDM or SNMP); setting it needs the
  admin's password.
- **Admins only**, through the same protections as every admin action: session, CSRF token,
  same-origin check (`Origin: null` is rejected). Nothing in a switch's sandboxed EDM can reach
  these routes.
- **What is written is what was shown.** A preview is bound to the user and to a hash of the
  planned writes; applying takes only the preview ID and the hash. Before writing, the values
  are read again; if the switch changed since the preview, nothing is written.
- **Guards.** Warnings for uplinks (LLDP neighbour), LAG/MLT members and the management VLAN
  need an explicit acknowledgement; when the management VLAN cannot be determined (EDM), every
  change that could cut the switch off needs it. Deleting VLAN 1 or the management VLAN is
  refused.
- **Commit confirmed.** Every change is read back and stays pending until it is confirmed;
  otherwise it is reverted at the deadline, also after a restart of ERS-Proxy (the plan and
  the previous values are stored before the first write). The configuration is saved on the
  switch only after confirmation, so a reboot restores the previous state if a change cut the
  switch off.
- **Contained code.** Only `internal/switchcfg` writes to switches; tests check that only the
  web layer and the server command import it, and that gosnmp's `Set` is called only in its
  SNMP writer. EDM writes are never retried automatically.
- **Audit.** Apply, confirm, revert (also automatic ones, as `system`) and failures are in the
  audit log with a readable summary, never with credentials; each switch also has a change
  history.

### Switch sessions (T7)

- Switch cookies are kept in a per-token cookie jar on the server. They are never sent to the
  browser and never forwarded between devices. EDM reads one cookie through `document.cookie`;
  ERS-Proxy provides it through a small script for that document only.
- Browser cookies (including the portal session) are removed from requests to the switch.

## Residual risks

- **Tokens in URLs.** Proxy tokens appear in the reverse proxy's access log and, with "new
  browser tab", in the browser history. They are short-lived and bound to a session; restrict
  access to the logs.
- **Legacy TLS.** TLS 1.0 with RC4 or 3DES is weak. Use a management network.
- **Users can do what EDM allows.** ERS-Proxy does not restrict what a logged-in user does
  inside a switch's EDM; that is decided by the switch account. SwOS has only one account with
  full rights, so every portal user who can open a `swos` device can change that switch.
- **`isolation: off`** removes the protection against T4 for that device.
- **Configuration changes** (with `configure.enabled`): an admin account can change switches
  through ERS-Proxy, so its password protects the network. A change that cuts ERS-Proxy off
  from the switch cannot be reverted by ERS-Proxy; only a restart of the switch restores the
  saved configuration. Confirming a change saves the whole running configuration, including
  unsaved changes others made on the switch.
