# Consuming binkit from a Go application

This document is written for someone — human or agent — adding binkit to an existing Go
program that needs to shell out to an external tool such as Typst.

Read the whole thing before writing code. The mental model in the first section is what
makes the rest obvious, and getting it wrong produces an integration that appears to work
locally and fails on a colleague's machine or in CI.

## The mental model

binkit splits into two phases that happen at different times, on different machines, run
by different people.

| | Dev time | Run time |
|---|---|---|
| Who runs it | a developer, once per version bump | your program, every start |
| What runs | the `binkit` CLI | the binkit library |
| What it does | resolves a version, records digests | reads the pin, provisions, returns a path |
| Network | yes, GitHub API | only on a cache miss |
| Output | `tools.json`, **committed to your repo** | a filesystem path |

The pin is data in your repository. Your program never decides which version to use — it
reads the decision a human already committed. This is the entire point: changing what your
build runs is a reviewable diff, not a side effect of when someone happened to run it.

**Your program must never call `Update`.** That is the dev-time operation. A program that
calls it at runtime silently changes its own behaviour and rewrites a committed file.

## Step 1 — install the CLI

binkit is itself a Go program, so `go tool` can manage it — which is the thing binkit
exists to work around for tools that *aren't* Go.

```sh
go get -tool github.com/jroedel/binkit/cmd/binkit@latest
```

Then invoke it as `go tool binkit`. This records the version in your `go.mod`, so everyone
working on the project pins their tooling the same way binkit pins Typst.

Alternatively install it standalone:

```sh
go install github.com/jroedel/binkit/cmd/binkit@latest
```

> **Version note.** The CLI arrived in `v0.2.0`. `v0.1.0` was library-only, so pin to
> `v0.2.0` or later if you pin explicitly.

## Step 2 — pin the tool

From your project root, where `tools.json` should live:

```sh
go tool binkit pin typst
```

```
pinned typst 0.15.1 in tools.json (6 platforms verified)
```

To pin an exact version instead of the latest release:

```sh
go tool binkit pin typst@0.15.1
```

That "6 platforms verified" number matters. `pin` records a SHA-256 for every platform the
tool publishes for, taken from the one release response, so a lock file generated on Linux
still verifies on a colleague's macOS machine. If the count is lower than you expect, some
platform will fail with `ErrNoDigest` later — see [Failure modes](#failure-modes).

## Step 3 — commit `tools.json`

```sh
git add tools.json && git commit -m "build: pin typst 0.15.1"
```

**This is not optional.** Without it, every other developer and every CI run gets
`ErrNotPinned`. The file is small, sorted, diff-stable, and written `0644`.

## Step 4 — add the library

```sh
go get github.com/jroedel/binkit
```

```go
package tooling

import (
	"context"
	"fmt"
	"os/exec"

	"github.com/jroedel/binkit"
	"github.com/jroedel/binkit/catalog"
)

// resolver is safe for concurrent use and must not be copied after first use, so keep
// one and share it by pointer.
var resolver = &binkit.Resolver{
	// Update checks make a GitHub API call and can only ever print to a terminal.
	// A service has no terminal, so turn them off explicitly rather than relying on
	// the terminal check to skip them.
	NoCheck: true,
}

// CompilePDF typesets src into dst using the pinned Typst.
func CompilePDF(ctx context.Context, src, dst string) error {
	path, err := resolver.Ensure(ctx, catalog.Typst())
	if err != nil {
		return fmt.Errorf("provision typst: %w", err)
	}

	cmd := exec.CommandContext(ctx, path, "compile", src, dst)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("typst compile: %w: %s", err, out)
	}
	return nil
}
```

That is the whole integration. `Ensure` downloads and verifies on a cache miss and returns
the cached path otherwise. **binkit never executes anything** — what to run, with what
arguments, is yours.

## Where the lock file is read from

`Resolver.Lock` defaults to `"tools.json"`, **relative to the process working directory**.

For a CLI that runs in a project root, the default is right. For a server, a daemon, or
anything whose working directory is not guaranteed, it is a bug waiting to happen: the
program works in development and reports `ErrNotPinned` in production.

Pick one:

```go
// Explicit absolute path, resolved from config or an install prefix.
resolver := &binkit.Resolver{Lock: filepath.Join(installDir, "tools.json")}
```

```go
// Or embed the pin in the binary, write it out at startup, and point at that.
//go:embed tools.json
var toolsJSON []byte
```

Embedding is the more robust option for a deployed service: the pin travels with the
binary, and there is no file to forget to ship.

## Resolving once

`Ensure` re-reads and re-parses the lock file on every call. On a warm cache it is still
just a file read plus a `stat`, so calling it per request is not expensive — but it is not
free either, and it can fail.

For a long-running service, resolve at startup and fail fast:

```go
func New(ctx context.Context) (*Service, error) {
	typst, err := resolver.Ensure(ctx, catalog.Typst())
	if err != nil {
		return nil, fmt.Errorf("provision typst: %w", err)
	}
	return &Service{typst: typst}, nil
}
```

A missing or corrupt tool should stop startup, not surface as a failed request an hour in.

## Failure modes

Every error is wrapped, so match with `errors.Is`. These are the ones worth handling
distinctly — the rest are ordinary I/O and network failures.

| Error | Means | What to do |
|---|---|---|
| `ErrNotPinned` | no entry in `tools.json` | tell the user to run `binkit pin <tool>`; usually a missing commit |
| `ErrNotCached` | pinned, not installed (`CachedPath` only) | call `Ensure`, or tell the user to |
| `ErrNoDigest` | pinned, but no digest for *this* platform | re-pin on a machine where the tool publishes for this platform |
| `ErrDigestMismatch` | bytes did not match the pin | **do not retry, do not work around it** — treat as a supply-chain event |
| `ErrUnsupportedPlatform` | the tool has no build for this GOOS/GOARCH | degrade or fail with a clear message; no amount of retrying helps |
| `ErrInvalidTool` | the `Tool` value is malformed | a programming error in your code |

`ErrDigestMismatch` deserves emphasis. It means the archive GitHub served did not hash to
what was recorded at pin time. The legitimate cause is an upstream re-publish of the same
tag; the illegitimate one is exactly what the digest is there to catch. Surface it loudly.
Nothing is installed when it happens.

```go
path, err := resolver.Ensure(ctx, catalog.Typst())
switch {
case errors.Is(err, binkit.ErrNotPinned):
	return fmt.Errorf("typst is not pinned; run: binkit pin typst")
case errors.Is(err, binkit.ErrDigestMismatch):
	return fmt.Errorf("typst failed verification, refusing to run it: %w", err)
case err != nil:
	return fmt.Errorf("provision typst: %w", err)
}
```

## Checking without installing

`CachedPath` answers "is this already provisioned?" without downloading, verifying, or
touching the network. Use it for a health check, a `doctor` command, or a startup probe on
a machine that is deliberately offline.

```go
if _, err := resolver.CachedPath(catalog.Typst()); errors.Is(err, binkit.ErrNotCached) {
	log.Println("typst not yet downloaded; first render will fetch it")
}
```

`Pins` reports what the lock file holds — version, repo, and per-platform digests —
without any network access. `Ensure` returns only a path, so this is how you report the
version you are pinned to.

```go
pins, err := resolver.Pins()
if err != nil {
	return err
}
fmt.Printf("typst %s\n", pins["typst"].Version)
```

## The environment escape hatch

`BINKIT_<TOOLNAME>` short-circuits everything: when set, `Ensure` and `CachedPath` return
its value untouched, with no lock file, no cache, and no verification.

```sh
BINKIT_TYPST=/usr/bin/typst ./yourapp
```

This exists for CI images, Nix, and distro packages that already provide the tool. Get the
variable name from the tool rather than hardcoding it:

```go
fmt.Printf("set %s to use your own build\n", catalog.Typst().EnvKey()) // BINKIT_TYPST
```

The name is upper-cased with every character outside `A-Z0-9` replaced by `_`. Two names
are unavailable because they would collide with binkit's own variables: a tool called
`cache` or `no-update-check` would produce `BINKIT_CACHE` and `BINKIT_NO_UPDATE_CHECK`.

## Testing your application

Point the escape hatch at a stub. No network, no cache, no downloads:

```go
func TestCompilePDF(t *testing.T) {
	stub := filepath.Join(t.TempDir(), "typst")
	if err := os.WriteFile(stub, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BINKIT_TYPST", stub)

	if err := CompilePDF(t.Context(), "in.typ", "out.pdf"); err != nil {
		t.Fatal(err)
	}
}
```

Use `t.Setenv`, not `os.Setenv` — it restores the previous value and blocks parallel tests
that would otherwise race on the variable. The shell-script stub is POSIX-only; on Windows
point at a real executable or skip.

## CI

- **Commit `tools.json`.** Everything else follows from this.
- **No token is needed.** A pinned tool downloads by direct URL, so provisioning is not
  subject to the GitHub API rate limit. `GITHUB_TOKEN` is read if present, and only lifts
  the limit for the API calls that `Update` and update checks make.
- **Cache `~/.cache/binkit`** (or set `BINKIT_CACHE` to a path you cache) to skip repeat
  downloads. Correctness does not depend on it — a cold cache just re-downloads and
  re-verifies.
- **Set `BINKIT_NO_UPDATE_CHECK=1`** if you have set `Resolver.Stderr` to anything that is
  not an `*os.File`. See the warning below.

## Update notices

Once every seven days, `Ensure` may check whether a newer release exists and print to
stderr:

```
binkit: typst 0.15.1 is pinned; 0.16.0 is available.
```

It never updates anything, never fails, and never touches stdout. A failed check backs off
for an hour rather than burning the seven-day window.

Control it with `Resolver.NoCheck`, `Resolver.CheckEvery`, or `BINKIT_NO_UPDATE_CHECK=1`.
Add a second line naming your own update command with `Resolver.UpdateHint`:

```go
resolver := &binkit.Resolver{
	UpdateHint: func(tool string) string { return "yourapp --update-tools " + tool },
}
```

> **Gotcha worth knowing.** By default the check is skipped entirely when stderr is not a
> terminal, so a service or a CI job never performs it. But setting `Resolver.Stderr` to
> anything that is **not** an `*os.File` — a `bytes.Buffer`, a log writer — is read as a
> deliberate choice to display notices, and turns the check back on, network call and all.
> If you route binkit's output into your logger, set `NoCheck: true` as well.

## Tools the catalog does not have

The catalog is a convenience, not a gate. `catalog.Names()` lists what it knows. Anything
else is a struct literal you pass to the same methods:

```go
widget := binkit.Tool{
	Name: "widget",                 // keys the cache, lock file, and BINKIT_WIDGET
	Repo: "acme/widget",            // GitHub owner/name
	Tag:  func(v string) string { return "release-" + v }, // default is "v" + version

	// Returning an error means "not published for this platform", which pin treats as
	// a platform to skip rather than as a failure.
	Asset: func(version, goos, goarch string) (string, error) {
		if goos != "linux" {
			return "", fmt.Errorf("widget publishes no build for %s/%s", goos, goarch)
		}
		return fmt.Sprintf("widget-%s-%s.tar.gz", version, goarch), nil
	},

	// Path of the executable *inside* the archive.
	BinaryPath: func(version, goos, goarch string) string {
		return fmt.Sprintf("widget-%s-%s/widget", version, goarch)
	},
}
```

Supported archives: `.tar.xz`, `.txz`, `.tar.gz`, `.tgz`, `.tar`, `.zip`. Only the single
expected entry is extracted, to a filename binkit chooses, after the digest is verified.

A tool defined this way can only be pinned from Go code — the CLI is catalog-driven. Either
add it to the catalog upstream, or call `Update` from a small `//go:build ignore` program
or a dev-only subcommand of your own.

## Checklist

- [ ] `go tool binkit pin <tool>` run from the project root
- [ ] `tools.json` committed
- [ ] `Resolver.Lock` set to an absolute or embedded path if the working directory is not guaranteed
- [ ] One shared `*binkit.Resolver`, not copied
- [ ] `NoCheck: true` for services, or `Stderr` left alone
- [ ] `Ensure` called once at startup for long-running processes
- [ ] `ErrNotPinned` and `ErrDigestMismatch` handled distinctly
- [ ] Tests use `t.Setenv("BINKIT_<TOOL>", stub)`
- [ ] `Update` called nowhere in application code

## Reference

| Call | Network | Purpose |
|---|---|---|
| `Resolver.Ensure(ctx, tool)` | on cache miss; plus a weekly check | provision and return a path |
| `Resolver.CachedPath(tool)` | never | path if already installed |
| `Resolver.Pins()` | never | what the lock file holds |
| `Resolver.Update(ctx, tool, version)` | always | **dev time only** — resolve and rewrite the pin |
| `catalog.Typst()` / `catalog.Lookup(name)` / `catalog.Names()` | never | ready-made definitions |
| `Tool.EnvKey()` | never | the override variable's name |

Full API documentation: `go doc github.com/jroedel/binkit`.
