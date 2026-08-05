# Changelog

Notable changes to binkit. The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and the project follows [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

While the module is v0 the Go API may change in a minor release. The lock file format is
held to a stricter standard — it lives in a consuming project's repository, so a break
there breaks builds. Any change to it will appear here under its own heading.

## [Unreleased]

### Added

- `cmd/binkit`, a CLI with `pin`, `ensure`, `path`, and `list`. Until now a project could
  not create a `tools.json` without writing Go code that called `Resolver.Update`, which
  left the library unable to perform its own first step. Stdout carries only paths and the
  pin table so command substitution is safe; usage errors exit 2 and provisioning failures
  exit 1.
- `Resolver.CachedPath` returns the path of an already-installed tool without downloading,
  verifying, or touching the network, and reports the new `ErrNotCached` when the tool is
  pinned but absent. `Ensure` cannot answer that question, because it installs.
- `catalog.Lookup` and `catalog.Names` turn a string into a `Tool`, so a command-line
  argument does not require a hand-maintained switch statement.
- [INTEGRATION.md](INTEGRATION.md), a guide for adding binkit to an existing Go
  application.

- `Resolver.Pins` reports the pinned version, repo, and digests for every tool in the lock
  file. `LockFile` and `LockEntry` were exported but unreachable: no exported function
  produced or consumed them, so a caller wanting the resolved version had to re-parse
  `tools.json` themselves.
- Runnable godoc examples, including one that defines a `Tool` by hand rather than taking
  it from the catalog.
- Integration tests behind the `integration` build tag, exercising a real download,
  verification, extraction, and execution against live releases. `make test-integration`
  previously re-ran the unit suite, because no file carried the tag.
- A CI workflow running unit tests on Linux, macOS, and Windows, plus gofmt, staticcheck,
  govulncheck, and a guard on the one-dependency promise. Integration tests run weekly and
  on demand rather than per-PR.

### Fixed

- Package documentation stated that `Ensure` never contacts the GitHub API. That stopped
  being true when update checks were added in 0.1.0: `Ensure` queries the API at most once
  per check interval. Provisioning itself still downloads by direct URL and needs no token.
  Behaviour is unchanged; only the documentation was wrong.
- The README's opening example called `catalog.Typst` without parentheses and did not
  compile.

### Documented

- `Tool.EnvKey` maps a tool name into the same `BINKIT_*` namespace binkit uses for its own
  settings, so tools named `cache` or `no-update-check` would collide with `BINKIT_CACHE`
  and `BINKIT_NO_UPDATE_CHECK`. Those two names are unavailable.
- `compareVersions` orders two prereleases of the same version lexically, placing
  `1.0.0-rc10` below `1.0.0-rc9`. This affects only whether an advisory notice is shown.

## [0.1.0] - 2026-08-01

### Added

- Pinned, digest-verified provisioning of external release binaries from GitHub, cached per
  version, with atomic installation and in-process collapsing of concurrent downloads.
- Support for `.tar.xz`, `.tar.gz`, `.tar`, and `.zip` assets, extracting only the single
  expected entry to a name binkit chooses.
- `Resolver.Update` records digests for every platform in `Resolver.Platforms` from one
  release response, so a lock file generated on Linux verifies on macOS and Windows.
- Advisory update checks, at most once every seven days, written to stderr only when a
  terminal is present. They never change what is installed and never fail a build.
- `BINKIT_<TOOLNAME>` escape hatch, `BINKIT_CACHE`, and `BINKIT_NO_UPDATE_CHECK`.
- `catalog` subpackage with a ready-made definition for Typst.

### Fixed

- The lock file is written 0644 rather than inheriting `os.CreateTemp`'s 0600.

[Unreleased]: https://github.com/jroedel/binkit/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/jroedel/binkit/releases/tag/v0.1.0
