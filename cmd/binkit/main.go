// Command binkit provisions external CLI binaries from pinned, digest-verified GitHub
// releases.
//
// It is the dev-time half of the library: `binkit pin` establishes the pin that a Go
// program's [binkit.Resolver.Ensure] call later resolves at runtime. A project that
// never runs this command has no tools.json, and Ensure has nothing to read.
//
// Output discipline matters here, because the whole point is command substitution.
// Machine-consumable output — paths, the pin table — goes to stdout and nothing else
// ever does. Status, progress, and update notices go to stderr. That is what makes
//
//	TYPST=$(binkit ensure typst)
//
// safe regardless of what binkit has to say while it works.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"os"
	"os/signal"
	"slices"
	"strings"
	"text/tabwriter"

	"github.com/jroedel/binkit"
	"github.com/jroedel/binkit/catalog"
)

const usage = `binkit provisions external CLI binaries from pinned GitHub releases.

usage:
  binkit pin <tool>[@version]   resolve, install, and record the pin in the lock file
  binkit ensure <tool>          install if needed, print the path to stdout
  binkit path <tool>            print the path of an installed tool; never uses the network
  binkit list                   list the pins in the lock file
  binkit help                   show this message

options (accepted by every command):
  -lock <path>    lock file to read and write (default "tools.json")
  -cache <path>   cache directory (default $BINKIT_CACHE, else the user cache dir)

stdout carries only paths and the pin table, so command substitution is safe:
  TYPST=$(binkit ensure typst) && "$TYPST" compile in.typ out.pdf

environment:
  BINKIT_<TOOL>            bypass binkit entirely and use this path
  BINKIT_CACHE             cache directory
  BINKIT_NO_UPDATE_CHECK   disable the weekly "a newer release exists" notice
  GITHUB_TOKEN             optional; lifts the API rate limit, never required to install`

// errUsage marks a command-line mistake rather than a provisioning failure, so scripts
// can tell the two apart: it exits 2, everything else exits 1.
var errUsage = errors.New("usage")

func main() {
	// Ctrl-C during a download should abandon it, not leave a partial file: the
	// context reaches the HTTP request, and staging is cleaned up on the way out.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	err := run(ctx, os.Args[1:], os.Stdout, os.Stderr)
	switch {
	case err == nil:
		return
	case errors.Is(err, errUsage):
		fmt.Fprintf(os.Stderr, "binkit: %v\n\n%s\n", err, usage)
		os.Exit(2)
	default:
		fmt.Fprintf(os.Stderr, "binkit: %v\n", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return fmt.Errorf("%w: no command given", errUsage)
	}

	cmd, rest := args[0], args[1:]
	switch cmd {
	case "pin":
		return cmdPin(ctx, rest, stderr)
	case "ensure":
		return cmdEnsure(ctx, rest, stdout, stderr)
	case "path":
		return cmdPath(rest, stdout, stderr)
	case "list":
		return cmdList(rest, stdout, stderr)
	case "help", "-h", "-help", "--help":
		fmt.Fprintln(stdout, usage)
		return nil
	default:
		return fmt.Errorf("%w: unknown command %q", errUsage, cmd)
	}
}

// newResolver builds a flag set carrying the options every subcommand accepts, wired
// directly into the Resolver it returns. Registering them per subcommand rather than
// globally is what lets both "binkit -lock x ensure typst" and the more natural
// "binkit ensure -lock x typst" work.
func newResolver(name string, stderr io.Writer) (*binkit.Resolver, *flag.FlagSet) {
	r := &binkit.Resolver{
		Stderr: stderr,

		// binkit cannot know a consuming CLI's flags, but this *is* the CLI, so the
		// notice can name the exact command that acts on it.
		UpdateHint: func(tool string) string { return "binkit pin " + tool },
	}

	fs := flag.NewFlagSet("binkit "+name, flag.ContinueOnError)
	fs.SetOutput(io.Discard) // errors are reported through errUsage instead
	fs.StringVar(&r.Lock, "lock", "tools.json", "lock file to read and write")
	fs.StringVar(&r.CacheDir, "cache", "", "cache directory")
	return r, fs
}

// parseOne parses flags and requires exactly one positional argument.
func parseOne(fs *flag.FlagSet, args []string, what string) (string, error) {
	if err := fs.Parse(args); err != nil {
		return "", fmt.Errorf("%w: %v", errUsage, err)
	}
	rest := fs.Args()
	if len(rest) != 1 {
		return "", fmt.Errorf("%w: %s takes exactly one %s, got %d", errUsage, fs.Name(), what, len(rest))
	}
	return rest[0], nil
}

// lookup resolves a tool name against the catalog, reporting what is available when it
// cannot. A project needing a tool the catalog lacks defines a binkit.Tool in its own
// code; that is a library path, not a CLI one.
func lookup(name string) (binkit.Tool, error) {
	tool, ok := catalog.Lookup(name)
	if !ok {
		return binkit.Tool{}, fmt.Errorf("%w: unknown tool %q; the catalog has: %s",
			errUsage, name, strings.Join(catalog.Names(), ", "))
	}
	return tool, nil
}

// cmdPin resolves a version, installs it, and records it. This is the only command that
// writes the lock file, which is the same guarantee the library makes: nothing changes
// what a build runs except a deliberate act.
func cmdPin(ctx context.Context, args []string, stderr io.Writer) error {
	r, fs := newResolver("pin", stderr)

	// Update performs the check itself by resolving a version; a notice on top of that
	// would be noise.
	r.NoCheck = true

	spec, err := parseOne(fs, args, "tool")
	if err != nil {
		return err
	}

	name, version, _ := strings.Cut(spec, "@")
	tool, err := lookup(name)
	if err != nil {
		return err
	}

	if _, err := r.Update(ctx, tool, version); err != nil {
		return err
	}

	pins, err := r.Pins()
	if err != nil {
		return err
	}
	pin := pins[name]

	// Status to stderr: stdout stays clean so pin can be used in a script without
	// polluting whatever the script is capturing.
	fmt.Fprintf(stderr, "pinned %s %s in %s (%d platforms verified)\n",
		name, pin.Version, r.Lock, len(pin.Digests))
	return nil
}

// cmdEnsure resolves the pin to a usable path, installing it if necessary.
func cmdEnsure(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	r, fs := newResolver("ensure", stderr)

	name, err := parseOne(fs, args, "tool")
	if err != nil {
		return err
	}
	tool, err := lookup(name)
	if err != nil {
		return err
	}

	path, err := r.Ensure(ctx, tool)
	if err != nil {
		return withPinHint(err, name)
	}

	fmt.Fprintln(stdout, path)
	return nil
}

// cmdPath reports an already-installed tool without touching the network, so it is safe
// on a machine that is deliberately offline and cheap enough to call in a shell prompt.
func cmdPath(args []string, stdout, stderr io.Writer) error {
	r, fs := newResolver("path", stderr)

	name, err := parseOne(fs, args, "tool")
	if err != nil {
		return err
	}
	tool, err := lookup(name)
	if err != nil {
		return err
	}

	path, err := r.CachedPath(tool)
	switch {
	case errors.Is(err, binkit.ErrNotCached):
		return fmt.Errorf("%w; run: binkit ensure %s", err, name)
	case err != nil:
		return withPinHint(err, name)
	}

	fmt.Fprintln(stdout, path)
	return nil
}

// cmdList prints the lock file's contents, including how many platforms each pin can
// verify — a pin recorded for one platform will fail for a colleague on another.
func cmdList(args []string, stdout, stderr io.Writer) error {
	r, fs := newResolver("list", stderr)

	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("%w: %v", errUsage, err)
	}
	if rest := fs.Args(); len(rest) != 0 {
		return fmt.Errorf("%w: list takes no arguments, got %d", errUsage, len(rest))
	}

	pins, err := r.Pins()
	if err != nil {
		return err
	}
	if len(pins) == 0 {
		fmt.Fprintf(stderr, "no pins in %s; run: binkit pin <tool>\n", r.Lock)
		return nil
	}

	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "TOOL\tVERSION\tREPO\tPLATFORMS")
	for _, name := range slices.Sorted(maps.Keys(pins)) {
		pin := pins[name]
		fmt.Fprintf(tw, "%s\t%s\t%s\t%d\n", name, pin.Version, pin.Repo, len(pin.Digests))
	}
	return tw.Flush()
}

// withPinHint appends the command that fixes an unpinned tool. The library cannot know
// it; this binary is it.
func withPinHint(err error, name string) error {
	if errors.Is(err, binkit.ErrNotPinned) {
		return fmt.Errorf("%w; run: binkit pin %s", err, name)
	}
	return err
}
