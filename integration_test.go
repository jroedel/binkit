//go:build integration

// These tests reach the real GitHub API and download real release assets, so they are
// gated behind the `integration` tag and run only via `make test-integration`. They
// exist because the unit tests serve every byte from an httptest server: nothing else
// in the suite proves that binkit's asset naming, archive handling, and digest
// verification match what a project actually publishes today.
//
// They pin whatever version is currently latest rather than a fixed one, so an upstream
// change to release layout surfaces here as a failure instead of in a user's build.
package binkit_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/jroedel/binkit"
	"github.com/jroedel/binkit/catalog"
)

// offlineTransport fails every request, so a call that completes with it installed
// demonstrably did not reach the network.
type offlineTransport struct{}

func (offlineTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("network access attempted")
}

// newResolver returns a Resolver scoped to a temp cache and lock file, with update
// checks off so the test never depends on advisory behaviour.
func newResolver(t *testing.T) *binkit.Resolver {
	t.Helper()
	dir := t.TempDir()
	return &binkit.Resolver{
		CacheDir: filepath.Join(dir, "cache"),
		Lock:     filepath.Join(dir, "tools.json"),
		NoCheck:  true,
	}
}

// TestIntegrationTypstEndToEnd is the whole contract against a live release: resolve
// the latest version, record digests for every platform, verify, extract, and produce a
// binary that actually executes.
func TestIntegrationTypstEndToEnd(t *testing.T) {
	r := newResolver(t)
	ctx := t.Context()
	tool := catalog.Typst()

	path, err := r.Update(ctx, tool, "")
	if err != nil {
		t.Fatalf("Update typst: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat installed binary: %v", err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o111 == 0 {
		t.Errorf("installed binary mode %04o is not executable", info.Mode().Perm())
	}

	// The point of the whole package: a binary the caller can run.
	out, err := exec.CommandContext(ctx, path, "--version").CombinedOutput()
	if err != nil {
		t.Fatalf("run %s --version: %v\n%s", path, err, out)
	}
	if !strings.Contains(strings.ToLower(string(out)), "typst") {
		t.Errorf("--version output does not mention typst:\n%s", out)
	}

	pins, err := r.Pins()
	if err != nil {
		t.Fatalf("Pins: %v", err)
	}
	pin, ok := pins["typst"]
	if !ok {
		t.Fatalf("Update wrote no typst pin; got %+v", pins)
	}
	if pin.Repo != "typst/typst" {
		t.Errorf("pinned repo = %q, want %q", pin.Repo, "typst/typst")
	}
	if !strings.Contains(string(out), pin.Version) {
		t.Errorf("installed binary reports %q, which does not contain pinned version %q", out, pin.Version)
	}

	// A colleague on another OS must get a verified install from this lock file, which
	// only holds if Update captured digests beyond the current platform.
	for _, p := range binkit.DefaultPlatforms {
		if _, ok := pin.Digests[p.String()]; !ok {
			t.Errorf("no digest recorded for %s; a lock file pinned here would not verify there", p)
		}
	}
}

// TestIntegrationEnsureIsOfflineOnCacheHit exercises the claim the package doc makes:
// with update checks off, a cached tool costs no network access at all.
func TestIntegrationEnsureIsOfflineOnCacheHit(t *testing.T) {
	r := newResolver(t)
	ctx := t.Context()
	tool := catalog.Typst()

	want, err := r.Update(ctx, tool, "")
	if err != nil {
		t.Fatalf("Update typst: %v", err)
	}

	r.HTTP = &http.Client{Transport: offlineTransport{}}

	got, err := r.Ensure(ctx, tool)
	if err != nil {
		t.Fatalf("Ensure on a warm cache attempted the network: %v", err)
	}
	if got != want {
		t.Errorf("Ensure = %q, want cached %q", got, want)
	}
}

// TestIntegrationDigestMismatchIsFatal corrupts a real pin and confirms binkit refuses
// the real asset rather than installing it. This is the security property the package
// is built around, checked against bytes GitHub actually served.
func TestIntegrationDigestMismatchIsFatal(t *testing.T) {
	r := newResolver(t)
	ctx := t.Context()
	tool := catalog.Typst()

	if _, err := r.Update(ctx, tool, ""); err != nil {
		t.Fatalf("Update typst: %v", err)
	}

	pins, err := r.Pins()
	if err != nil {
		t.Fatalf("Pins: %v", err)
	}
	pin := pins["typst"]

	// Poison the digest for this platform and discard the cached install, so Ensure has
	// to download and verify again.
	pin.Digests[binkit.Platform{OS: runtime.GOOS, Arch: runtime.GOARCH}.String()] =
		"sha256:0000000000000000000000000000000000000000000000000000000000000000"
	pins["typst"] = pin
	writePins(t, r, pins)

	cacheDir := filepath.Join(filepath.Dir(r.Lock), "cache")
	if err := os.RemoveAll(filepath.Join(cacheDir, "typst")); err != nil {
		t.Fatalf("clear cache: %v", err)
	}

	path, err := r.Ensure(ctx, tool)
	if !errors.Is(err, binkit.ErrDigestMismatch) {
		t.Fatalf("Ensure with a poisoned digest = (%q, %v), want ErrDigestMismatch", path, err)
	}
	if _, statErr := os.Stat(filepath.Join(cacheDir, "typst", pin.Version)); statErr == nil {
		t.Error("a failed verification left an installed version behind")
	}
}

// writePins rewrites the lock file directly. binkit exposes no setter by design — only
// Update writes a pin — so the test edits the JSON the same way a tampering attacker or
// a bad merge would.
func writePins(t *testing.T, r *binkit.Resolver, pins binkit.LockFile) {
	t.Helper()
	data, err := json.MarshalIndent(pins, "", "  ")
	if err != nil {
		t.Fatalf("encode lock file: %v", err)
	}
	if err := os.WriteFile(r.Lock, append(data, '\n'), 0o644); err != nil {
		t.Fatalf("write lock file: %v", err)
	}
}
