# ERS-Proxy – notes for AI-assisted contributions

ERS-Proxy makes the built-in web interface (EDM) of Avaya/Extreme ERS switches usable in
modern browsers and keeps all network devices of a site in one portal: devices on the left,
their web interfaces in tabs on the right.

**Everything in this repository is in English** – code, comments, docs, commit messages. The
web UI is bilingual; German strings live only in `internal/web/i18n.go`. The repository is
public: never commit credentials, internal hostnames, IP addresses or paths of a specific
installation.

## Read first

1. `docs/ROADMAP.md` – phases with numbered steps; "Current status" says what is done and
   which step is next.
2. `docs/architecture.md` – package layout, request flow, data model, security mechanisms.
   Read this instead of re-exploring the code.
3. `docs/compatibility.md` – which switches and firmware versions were verified, and how.
4. `docs/hardware-tests.md` – the checklist for tests on real switches (read-only, except
   section 9 for configuration changes on an approved test switch).

## "Continue" / "mach weiter"

When asked to continue without further detail:

1. Read "Current status" in `docs/ROADMAP.md`.
2. Implement the step named under **Next** (several steps if they are small).
3. Run `go vet ./...` and `go test ./...`.
4. Update "Current status", `docs/architecture.md` (structural changes) and `CHANGELOG.md`
   (`[Unreleased]`).
5. Report briefly what is done and what comes next.

Do not commit, push or test against real switches unless explicitly asked.

## Rules that must not be broken

- **EDM content is untrusted.** HTML and JavaScript from a switch are served only under
  `/d/{token}/` and always with `Content-Security-Policy: sandbox …` **without**
  `allow-same-origin` (see `docs/security.md`). Never serve proxied content under another
  path, and never relax the sandbox globally. The only exception is the per-device
  `isolation: off` fallback, which the UI must flag.
- **Management requests never come from the sandbox.** The session cookie is
  `SameSite=Strict`; state-changing requests need the CSRF token and a same-origin `Origin`;
  `Origin: null` is rejected.
- **Credentials** (switch passwords) are stored AES-GCM encrypted with associated data bound to
  device and host – never in logs, the audit log, responses or pre-filled forms.
- **Switch cookies stay on the server** (cookie jar per proxy token). Browser cookies are never
  forwarded to a switch.
- **No vendor files in the repository.** EDM files are Avaya/Extreme property. Tests use small
  synthetic fixtures; browser tests against a real webroot run only locally
  (`ERSPROXY_EDM_WEBROOT`).
- **No inline scripts or styles** in the management UI (CSP). Device names and anything a
  switch reports are escaped by `html/template`.
- **Hardware is production equipment.** Never send SNMP sets or EDM writes to a real switch in
  tests. Automated tests (`go test`) never write to real hardware. Manual write tests (phase
  8) only on a switch the maintainer has explicitly approved for writes, on unused ports and
  test VLANs, and revert afterwards.
- **Writes stay contained.** Only `internal/switchcfg` writes to switches, and only when
  `configure.enabled` is set in `config.yaml` – never add another way to enable it (environment
  variable, flag, UI). Monitoring (`internal/monitor`, `internal/snmp`) stays read-only; keep
  the `TestReadOnly` tests and the import/`Set` guards in `internal/switchcfg` passing.
- No change to configuration or data model without matching docs (`docs/configuration.md`,
  `docs/architecture.md`). Never change a released migration – add a new file.

## Commands

```bash
go vet ./...
go test ./...                       # unit tests
go test -short ./...                # skip slower tests
CGO_ENABLED=0 go build -o bin/ers-proxy ./cmd/ers-proxy
./bin/ers-proxy -c config.yaml config init
echo 'a-long-password' | ./bin/ers-proxy -c config.yaml user create admin --role admin
./bin/ers-proxy -c config.yaml server
```

If your installed Go is newer than needed, set `GOTOOLCHAIN=local` so that `go get` does not
pull module versions that require a newer toolchain.

## Releases

Create and publish a release with tag `vX.Y.Z` in the GitHub web UI (write the notes there).
`.github/workflows/release.yml` then builds with GoReleaser (`.goreleaser.yaml`): binaries and
archives for linux amd64/arm64/armv7, `.deb`, `.rpm`, `.pkg.tar.zst` and `.apk` packages,
`SHA256SUMS` and the multi-arch image `ghcr.io/<owner>/ers-proxy`. Every amd64 package is first
installed, started and removed in a fresh container (`packaging/test-install.sh`); only then
are the files attached, the image pushed and the build provenance attested. The build is
reproducible (`-trimpath`, commit timestamps).

## Conventions

- Go 1.24; no dependency that requires a newer toolchain.
- Logging with `log/slog`; wrap errors with context (`fmt.Errorf("…: %w", err)`).
- Timestamps in the database as Unix seconds (UTC).
- UI texts only via keys in `internal/web/i18n.go` (English and German).
- Commit messages in English, imperative mood.
