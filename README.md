# binkit

Provision, verify, pin, and update external CLI binaries from Go.

Some tools you depend on aren't written in Go, so `go tool` can't manage them. binkit fetches
them from GitHub releases, verifies them against a pinned SHA-256, caches them per version,
and hands your program back a path.

```go
path, err := resolver.Ensure(ctx, catalog.Typst())
if err != nil {
    return err
}
cmd := exec.CommandContext(ctx, path, "compile", "in.typ", "out.pdf")
```

## The one-dependency promise

`go.mod` has exactly one `require` line: `github.com/ulikunitz/xz`, which is itself
stdlib-only with zero transitive dependencies.

That dependency exists for one reason — many projects (Typst among them) ship `.tar.xz`, and
the Go standard library has `archive/tar`, `archive/zip`, and `compress/gzip` but no xz
decoder. Shelling out to `tar -xJf` would trade a compile-time dependency for a *runtime
environment* dependency and break on Windows and minimal containers, so it isn't done.

Everything else is stdlib or deliberately hand-rolled: semver comparison instead of
`golang.org/x/mod`, TTY detection instead of `mattn/go-isatty`, atomic directory rename
instead of a file-locking library.

## Pinning

`tools.json` lives in your repo and is checked in:

```json
{
  "typst": {
    "version": "0.15.1",
    "repo": "typst/typst",
    "digests": {
      "linux/amd64":   "sha256:...",
      "darwin/arm64":  "sha256:...",
      "windows/amd64": "sha256:..."
    }
  }
}
```

Digests are captured for every platform at pin time, so a colleague on macOS gets a verified
install from a lockfile generated on Linux.

`Ensure` never resolves "latest": a pinned version downloads by direct URL, so provisioning
makes no GitHub API call, needs no token, and cannot hit the API rate limit. The one API
request `Ensure` can make is the advisory update check below, which changes nothing and is
disableable. Only an explicit `Update` call changes the pin.

## Update checks

Once every seven days, binkit checks whether a newer release exists and reports it — to
**stderr**, and only when stderr is a terminal, so it never pollutes piped output or CI
logs. When there is nowhere to show a notice the check is skipped entirely rather than
performed and discarded.

```
binkit: typst 0.15.1 is pinned; 0.16.0 is available.
        run: massprep --update-tools typst
```

The second line appears only if you set `Resolver.UpdateHint` — binkit cannot know your
CLI's flags.

A failed check backs off for an hour rather than consuming the seven-day window, so being
offline for a week neither retries on every invocation nor silences the next real check.
Nothing here can fail a build, and nothing is ever updated automatically. Set
`BINKIT_NO_UPDATE_CHECK=1`, or `Resolver.NoCheck`, to disable.

## Escape hatch

`BINKIT_<TOOLNAME>` (e.g. `BINKIT_TYPST=/usr/bin/typst`) short-circuits everything and uses
that binary. Useful for CI images, Nix, and distro packages.

The tool name is upper-cased and every character outside `A-Z0-9` becomes an underscore, so
`go-task` reads `BINKIT_GO_TASK`. Don't name a tool `cache` or `no-update-check` — those
collide with `BINKIT_CACHE` and `BINKIT_NO_UPDATE_CHECK`.

## License

MIT
