package catalog_test

import (
	"errors"
	"path"
	"slices"
	"strings"
	"testing"

	"github.com/jroedel/binkit"
	"github.com/jroedel/binkit/catalog"
)

// wantTypstAssets are the asset names Typst actually publishes, transcribed from the
// v0.15.1 release. If upstream renames an asset this test fails, which is the point:
// the catalog's whole job is tracking that naming, and a silent mismatch would only
// surface as a 404 at install time.
var wantTypstAssets = map[binkit.Platform]string{
	{OS: "linux", Arch: "amd64"}:   "typst-x86_64-unknown-linux-musl.tar.xz",
	{OS: "linux", Arch: "arm64"}:   "typst-aarch64-unknown-linux-musl.tar.xz",
	{OS: "linux", Arch: "arm"}:     "typst-armv7-unknown-linux-musleabi.tar.xz",
	{OS: "linux", Arch: "riscv64"}: "typst-riscv64gc-unknown-linux-gnu.tar.xz",
	{OS: "darwin", Arch: "amd64"}:  "typst-x86_64-apple-darwin.tar.xz",
	{OS: "darwin", Arch: "arm64"}:  "typst-aarch64-apple-darwin.tar.xz",
	{OS: "windows", Arch: "amd64"}: "typst-x86_64-pc-windows-msvc.zip",
	{OS: "windows", Arch: "arm64"}: "typst-aarch64-pc-windows-msvc.zip",
}

func TestLookupResolvesEveryCataloguedName(t *testing.T) {
	names := catalog.Names()
	if len(names) == 0 {
		t.Fatal("the catalog is empty")
	}

	for _, name := range names {
		tool, ok := catalog.Lookup(name)
		if !ok {
			t.Errorf("Names reported %q but Lookup does not resolve it", name)
			continue
		}
		if tool.Name != name {
			t.Errorf("catalog.Lookup(%q) returned a tool named %q", name, tool.Name)
		}
	}
}

func TestLookupUnknownToolIsNotAnError(t *testing.T) {
	if _, ok := catalog.Lookup("nosuchtool"); ok {
		t.Error("Lookup reported success for a tool the catalog does not have")
	}
}

// TestLookupReturnsIndependentValues mirrors the guarantee the constructors give: one
// caller must not be able to mutate what another caller receives.
func TestLookupReturnsIndependentValues(t *testing.T) {
	first, ok := catalog.Lookup("typst")
	if !ok {
		t.Fatal("typst is not in the catalog")
	}
	second, _ := catalog.Lookup("typst")

	first.Repo = "someone/else"
	if second.Repo == first.Repo {
		t.Error("Lookup handed two callers the same mutable definition")
	}
}

func TestNamesIsSorted(t *testing.T) {
	names := catalog.Names()
	if !slices.IsSorted(names) {
		t.Errorf("catalog.Names() = %v, want sorted order for stable CLI output", names)
	}
}

func TestTypstAssetNames(t *testing.T) {
	typst := catalog.Typst()

	for p, want := range wantTypstAssets {
		t.Run(p.String(), func(t *testing.T) {
			got, err := typst.Asset("0.15.1", p.OS, p.Arch)
			if err != nil {
				t.Fatalf("Asset(%s): %v", p, err)
			}
			if got != want {
				t.Errorf("Asset(%s) = %q, want %q", p, got, want)
			}
		})
	}
}

// TestTypstBinaryPathMatchesAsset checks the layout invariant: Typst archives contain a
// single directory named exactly like the asset minus its extension, holding the binary.
func TestTypstBinaryPathMatchesAsset(t *testing.T) {
	typst := catalog.Typst()

	for p, asset := range wantTypstAssets {
		t.Run(p.String(), func(t *testing.T) {
			inner := typst.BinaryPath("0.15.1", p.OS, p.Arch)

			dir, file := path.Split(inner)
			dir = strings.TrimSuffix(dir, "/")

			wantDir := strings.TrimSuffix(strings.TrimSuffix(asset, ".tar.xz"), ".zip")
			if dir != wantDir {
				t.Errorf("BinaryPath(%s) directory = %q, want %q", p, dir, wantDir)
			}

			wantFile := "typst"
			if p.OS == "windows" {
				wantFile = "typst.exe"
			}
			if file != wantFile {
				t.Errorf("BinaryPath(%s) file = %q, want %q", p, file, wantFile)
			}
		})
	}
}

func TestTypstUnsupportedPlatform(t *testing.T) {
	if _, err := catalog.Typst().Asset("0.15.1", "plan9", "386"); err == nil {
		t.Fatal("Asset for plan9/386 succeeded, want an error")
	}
}

// TestTypstIsAValidTool exercises the definition through the real validation path:
// reaching ErrNotPinned means the Tool itself was accepted.
func TestTypstIsAValidTool(t *testing.T) {
	r := &binkit.Resolver{
		CacheDir: t.TempDir(),
		Lock:     t.TempDir() + "/tools.json",
	}
	_, err := r.Ensure(t.Context(), catalog.Typst())
	if errors.Is(err, binkit.ErrInvalidTool) {
		t.Fatalf("catalog.Typst() is not a valid Tool: %v", err)
	}
	if !errors.Is(err, binkit.ErrNotPinned) {
		t.Fatalf("Ensure error = %v, want ErrNotPinned", err)
	}
}

// TestTypstReturnsIndependentValues is the reason Typst is a function rather than a
// package-level variable: one caller must not be able to mutate another's definition.
func TestTypstReturnsIndependentValues(t *testing.T) {
	first := catalog.Typst()
	first.Name = "mutated"

	if second := catalog.Typst(); second.Name != "typst" {
		t.Errorf("second call returned Name %q, want %q — definitions are shared state", second.Name, "typst")
	}
}
