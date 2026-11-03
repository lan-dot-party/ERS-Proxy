# Contributing

Thank you for helping! Issues and pull requests are welcome. For larger changes, open an issue
first so we can agree on the approach. The plan of record is [docs/ROADMAP.md](docs/ROADMAP.md);
refer to its step numbers in issues and pull requests.

## Development setup

You need Go 1.24 or newer. If your Go is newer, set `GOTOOLCHAIN=local` so that `go get` does
not pull dependencies that require a newer toolchain.

```bash
go test ./...
CGO_ENABLED=0 go build -o bin/ers-proxy ./cmd/ers-proxy
./bin/ers-proxy -c config.yaml config init
echo 'a-long-password' | ./bin/ers-proxy -c config.yaml user create admin --role admin
./bin/ers-proxy -c config.yaml server
```

## Ground rules

- **English** for code, comments, docs and commit messages (imperative mood). UI texts are
  keys in `internal/web/i18n.go` with English and German text.
- **No vendor files.** Do not commit EDM files, firmware images or other Avaya/Extreme
  material, not even as test fixtures. Use small synthetic fixtures that contain only the
  strings a patch looks for.
- **No inline JavaScript or CSS** in templates; the Content Security Policy forbids it. Put
  code into `internal/web/static/`.
- **Keep the security rules** in [CLAUDE.md](CLAUDE.md) ("Rules that must not be broken") and
  [docs/security.md](docs/security.md). Changes to the proxy pipeline need tests for the
  isolation headers.
- **No GPL dependencies.** ERS-Proxy is MIT-licensed.
- **Docs with code.** Configuration changes go into `docs/configuration.md`, structural changes
  into `docs/architecture.md`, user-visible changes into `CHANGELOG.md`.
- **Migrations are immutable** once released; add a new file instead.

## Adding support for a switch model

1. Open the device through ERS-Proxy and look at the diagnostics page: it lists which patches
   applied.
2. If EDM still fails, open the browser console and note the first error.
3. Add a content-based patch in `internal/edmproxy/patches/` with a synthetic test fixture.
4. Record the model and firmware in `docs/compatibility.md`, including how it was verified.

## Translations

Every key in `internal/web/i18n.go` has a German and an English text. A test fails when a
template uses a key that does not exist.

## Packages

`.goreleaser.yaml` builds all packages. To check locally, install
[GoReleaser v2](https://goreleaser.com/install/) and run:

```bash
goreleaser check
goreleaser release --snapshot --clean --skip=publish,docker
sh packaging/test-install.sh deb dist/ers-proxy_*_amd64.deb   # inside a Debian container, as root
```

## Browser test

`tools/e2e/edm_e2e.py` runs EDM through ERS-Proxy in headless Chromium and checks the sandbox.
It needs an EDM webroot from a switch firmware, which is not part of this repository; see the
script's header. Run it before and after changes to the proxy pipeline or the patches.
