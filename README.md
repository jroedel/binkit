# binkit

Provision, verify, pin, and update external CLI binaries from Go.

Some tools you depend on aren't written in Go, so `go tool` can't manage them. binkit fetches
them from GitHub releases, verifies them against a pinned SHA-256, caches them per version,
and hands your program back a path.

```go
path, err := resolver.Ensure(ctx, catalog.Typst)
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

`Ensure` never resolves "latest" and never silently reaches the network — a pinned version
downloads by direct URL, which also means no GitHub API rate limit and no token. Only an
explicit `Update` call changes the pin.

## Update checks

Once every seven days, binkit checks whether a newer release exists and prints a one-line
notice — to **stderr**, and only when stderr is a TTY, so it never pollutes piped output or
CI logs. It never updates anything on its own. Set `BINKIT_NO_UPDATE_CHECK=1` to disable.

## Escape hatch

`BINKIT_<TOOLNAME>` (e.g. `BINKIT_TYPST=/usr/bin/typst`) short-circuits everything and uses
that binary. Useful for CI images, Nix, and distro packages.

## License

MIT
