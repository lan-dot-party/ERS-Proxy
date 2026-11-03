# Security policy

ERS-Proxy sits between browsers and the management interfaces of network switches. It can store
switch passwords and gives its users access to device configuration. Security reports are
taken seriously and handled with priority.

## Supported versions

Only the latest release and the `main` branch receive security fixes.

## Reporting a vulnerability

Please **do not open a public issue** for security problems.

Report privately through GitHub: open the repository's **Security** tab and choose
**Report a vulnerability** (private vulnerability reporting). Include:

- what you found and where (file, endpoint, version or commit),
- how to reproduce it,
- what an attacker could achieve.

You can expect an acknowledgement within 7 days. Once a fix is available it is released together
with an advisory that credits you, unless you prefer to stay anonymous.

## Security model in brief

The threat model, mitigations and residual risks are in [docs/security.md](docs/security.md).

- Every page requires a login. Passwords are hashed with Argon2id; failed logins lead to a
  temporary lockout. Sessions are server-side; the cookie is `HttpOnly`, `SameSite=Strict`
  and `Secure` with an `https://` public URL.
- Web content from a switch is treated as untrusted. It is served only in a browser sandbox
  (`Content-Security-Policy: sandbox` without `allow-same-origin`), under an unguessable
  per-session path, and cannot use the portal's session.
- Switch cookies are kept on the server and never reach the browser.
- Stored switch passwords are encrypted with AES-256-GCM, bound to device and host, with a key
  stored outside the database.
- HTTPS connections to switches use certificate pinning (trust on first use). Old TLS versions
  are only used when a switch supports nothing newer, and the UI says so.

## Known limitations

- Many ERS switches only speak HTTP or TLS 1.0. With HTTP, the EDM login and all data between
  ERS-Proxy and the switch travel **unencrypted**. Put switches into a management network and
  prefer HTTPS.
- The EDM login sends the password in plain text inside the (TLS) connection; this is how the
  switch firmware works.

## Out of scope

- Vulnerabilities in switches or their firmware.
- Attacks that require an already compromised admin account or root on the ERS-Proxy host.
