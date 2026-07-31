package catalog_test

import (
	"errors"
	"path"
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
