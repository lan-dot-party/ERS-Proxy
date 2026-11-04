# Security

This document describes what ERS-Proxy protects, against whom, how, and what remains. Report
vulnerabilities as described in [SECURITY.md](../SECURITY.md).

## Assets

- Access to the management interfaces of network switches (configuration changes can take a
  network down).
- Switch passwords, if stored for auto-login.
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

Devices of kind `vsp-edm` (VSP switches with VOSS) are proxied exactly like ERS switches.
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
- ERS-Proxy only reads: `internal/snmp` has no SET API (checked by a test), so a leaked or
  misused community cannot be used to change a switch through ERS-Proxy.

### Connection to the switch (T6)

- HTTPS is preferred. Switches use self-signed certificates, so ERS-Proxy pins the SHA-256
  fingerprint of the certificate when the device is added (trust on first use, confirmed by the
  admin). A different certificate blocks the connection until an admin confirms it again.
- TLS mode `auto` tries modern TLS first and falls back to the legacy profile (TLS 1.0–1.2,
  RSA key exchange, AES-CBC, 3DES, RC4) only if the switch supports nothing better. The device
  page shows the negotiated version and cipher and warns below TLS 1.2.
- With HTTP the connection is unencrypted, including the EDM login. The portal marks such
  devices.

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
  inside a switch's EDM; that is decided by the switch account.
- **`isolation: off`** removes the protection against T4 for that device.
