package binkit_test

import (
	"context"
	"errors"
	"fmt"
	"log"
	"maps"
	"os/exec"
	"slices"

	"github.com/jroedel/binkit"
	"github.com/jroedel/binkit/catalog"
)

// The common case: resolve a pinned tool to a path, then run it. Ensure downloads and
// verifies against the pinned digest on a cache miss, and returns the cached path
// otherwise.
func Example() {
	ctx := context.Background()
	var resolver binkit.Resolver

	path, err := resolver.Ensure(ctx, catalog.Typst())
	if err != nil {
		log.Fatal(err)
	}

	cmd := exec.CommandContext(ctx, path, "compile", "in.typ", "out.pdf")
	if err := cmd.Run(); err != nil {
		log.Fatal(err)
	}
}

// A tool with no pin is reported as [binkit.ErrNotPinned] rather than silently fetched,
// so a CLI can tell the user which command would establish the pin.
func ExampleResolver_Ensure_unpinned() {
	var resolver binkit.Resolver

	_, err := resolver.Ensure(context.Background(), catalog.Typst())
	switch {
	case errors.Is(err, binkit.ErrNotPinned):
		fmt.Println("run: myapp --update-tools typst")
	case err != nil:
		log.Fatal(err)
	}
}

// Update resolves a version — the latest release when version is empty — installs it,
// and rewrites the lock file with digests for every platform in Resolver.Platforms, so
// one run on Linux produces a lock file that verifies on macOS and Windows too.
func ExampleResolver_Update() {
	resolver := binkit.Resolver{Lock: "tools.json"}

	path, err := resolver.Update(context.Background(), catalog.Typst(), "")
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println("installed", path)
}

// Pins reads the lock file without touching the network. It is how a CLI reports the
// versions it is pinned to, since Ensure returns only a path.
func ExampleResolver_Pins() {
	var resolver binkit.Resolver

	pins, err := resolver.Pins()
	if err != nil {
		log.Fatal(err)
	}
	for _, name := range slices.Sorted(maps.Keys(pins)) {
		fmt.Printf("%s %s (%s)\n", name, pins[name].Version, pins[name].Repo)
	}
}

// Update notices are advisory. UpdateHint supplies the second line, because binkit
// cannot know a consuming CLI's flags.
func ExampleResolver_updateNotices() {
	resolver := binkit.Resolver{
		UpdateHint: func(tool string) string {
			return "myapp --update-tools " + tool
		},
	}

	// binkit: typst 0.15.1 is pinned; 0.16.0 is available.
	//         run: myapp --update-tools typst
	_, _ = resolver.Ensure(context.Background(), catalog.Typst())
}

// A tool the catalog does not cover is an ordinary struct literal — nothing in the
// catalog is privileged. The asset naming below is illustrative; consult the project's
// own releases page for the real thing.
//
// Returning an error from Asset means "not published for this platform", which Update
// treats as a platform to skip rather than as a failure.
func ExampleTool() {
	targets := map[string]string{
		"linux/amd64":  "linux-x64",
		"darwin/arm64": "macos-arm64",
	}

	widget := binkit.Tool{
		Name: "widget",
		Repo: "acme/widget",

		// Default is "v" + version; override when a project tags differently.
		Tag: func(version string) string { return "release-" + version },

		Asset: func(version, goos, goarch string) (string, error) {
			target, ok := targets[goos+"/"+goarch]
			if !ok {
				return "", fmt.Errorf("widget publishes no build for %s/%s", goos, goarch)
			}
			return fmt.Sprintf("widget-%s-%s.tar.gz", version, target), nil
		},

		BinaryPath: func(version, goos, goarch string) string {
			return fmt.Sprintf("widget-%s-%s/widget", version, targets[goos+"/"+goarch])
		},
	}

	fmt.Println(widget.EnvKey())
	// Output: BINKIT_WIDGET
}

// EnvKey reports the variable that bypasses binkit entirely for one tool — worth
// printing in a CLI's help text so users can point at a distro or Nix build.
func ExampleTool_EnvKey() {
	fmt.Println(binkit.Tool{Name: "typst"}.EnvKey())
	fmt.Println(binkit.Tool{Name: "go-task"}.EnvKey())
	// Output:
	// BINKIT_TYPST
	// BINKIT_GO_TASK
}
