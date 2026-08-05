package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// exec runs the CLI with a fresh pair of buffers, which is also how the tests assert the
// stdout/stderr split that command substitution depends on.
func exec(t *testing.T, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	var out, errBuf bytes.Buffer
	err = run(t.Context(), args, &out, &errBuf)
	return out.String(), errBuf.String(), err
}

// writeLock puts a lock file in a temp directory and returns its path. The pin points at
// nothing real, which is fine: every test here stops before it would download.
func writeLock(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "tools.json")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write lock: %v", err)
	}
	return path
}

func TestUsageErrorsExitDistinctly(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{"no command", nil},
		{"unknown command", []string{"frobnicate"}},
		{"unknown tool", []string{"ensure", "nosuchtool"}},
		{"ensure with no tool", []string{"ensure"}},
		{"ensure with two tools", []string{"ensure", "typst", "extra"}},
		{"list with an argument", []string{"list", "typst"}},
		{"unknown flag", []string{"ensure", "-nope", "typst"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			stdout, _, err := exec(t, tc.args...)
			if !errors.Is(err, errUsage) {
				t.Errorf("err = %v, want errUsage", err)
			}
			if stdout != "" {
				t.Errorf("a usage error wrote %q to stdout, which would corrupt $(binkit ...)", stdout)
			}
		})
	}
}

// TestUnknownToolNamesTheCatalog keeps the error actionable: a caller who guessed wrong
// should not have to go read the catalog source to find out what exists.
func TestUnknownToolNamesTheCatalog(t *testing.T) {
	_, _, err := exec(t, "ensure", "typstt")
	if err == nil {
		t.Fatal("expected an error for an unknown tool")
	}
	if !strings.Contains(err.Error(), "typst") {
		t.Errorf("error %q does not mention the available tools", err)
	}
}

func TestHelpGoesToStdoutAndSucceeds(t *testing.T) {
	for _, arg := range []string{"help", "-h", "--help"} {
		t.Run(arg, func(t *testing.T) {
			stdout, _, err := exec(t, arg)
			if err != nil {
				t.Fatalf("help returned %v", err)
			}
			if !strings.Contains(stdout, "binkit pin") {
				t.Errorf("help output does not document the commands:\n%s", stdout)
			}
		})
	}
}

// TestListEmptyKeepsStdoutClean matters for scripting: `binkit list` piped into a parser
// must yield nothing rather than a friendly sentence.
func TestListEmptyKeepsStdoutClean(t *testing.T) {
	lock := filepath.Join(t.TempDir(), "tools.json")

	stdout, stderr, err := exec(t, "list", "-lock", lock)
	if err != nil {
		t.Fatalf("list on a missing lock file returned %v", err)
	}
	if stdout != "" {
		t.Errorf("stdout = %q, want empty", stdout)
	}
	if !strings.Contains(stderr, "binkit pin") {
		t.Errorf("stderr does not tell the user how to fix it: %q", stderr)
	}
}

func TestListReportsPins(t *testing.T) {
	lock := writeLock(t, `{
	  "typst": {
	    "version": "0.15.1",
	    "repo": "typst/typst",
	    "digests": {"linux/amd64": "sha256:aa", "darwin/arm64": "sha256:bb"}
	  }
	}`)

	stdout, _, err := exec(t, "list", "-lock", lock)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, want := range []string{"typst", "0.15.1", "typst/typst", "2"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("list output is missing %q:\n%s", want, stdout)
		}
	}
}

// TestPathHonoursEnvOverride covers the escape hatch end to end through the CLI: an
// operator who has pointed binkit at a distro build should get that path back without
// binkit consulting a lock file or a cache at all.
func TestPathHonoursEnvOverride(t *testing.T) {
	t.Setenv("BINKIT_TYPST", "/usr/bin/typst")

	stdout, _, err := exec(t, "path", "-lock", filepath.Join(t.TempDir(), "none.json"), "typst")
	if err != nil {
		t.Fatalf("path with an env override: %v", err)
	}
	if got := strings.TrimSpace(stdout); got != "/usr/bin/typst" {
		t.Errorf("path = %q, want %q", got, "/usr/bin/typst")
	}
}

// TestPathUnpinnedSuggestsPin and its sibling below check that the two failure modes a
// user actually hits each name the command that resolves them.
func TestPathUnpinnedSuggestsPin(t *testing.T) {
	_, _, err := exec(t, "path", "-lock", filepath.Join(t.TempDir(), "none.json"), "typst")
	if err == nil {
		t.Fatal("expected an error for an unpinned tool")
	}
	if !strings.Contains(err.Error(), "binkit pin typst") {
		t.Errorf("error %q does not suggest pinning", err)
	}
}

func TestPathPinnedButNotInstalledSuggestsEnsure(t *testing.T) {
	lock := writeLock(t, `{"typst": {"version": "0.15.1", "repo": "typst/typst"}}`)
	cache := t.TempDir()

	_, _, err := exec(t, "path", "-lock", lock, "-cache", cache, "typst")
	if err == nil {
		t.Fatal("expected an error for a pinned but uninstalled tool")
	}
	if !strings.Contains(err.Error(), "binkit ensure typst") {
		t.Errorf("error %q does not suggest ensure", err)
	}
}

// TestPathNeverTouchesTheNetwork is the property that lets `path` be called freely — in
// a shell prompt, in a loop, on a disconnected machine. An empty cache with a valid pin
// must fail fast rather than start downloading.
func TestPathNeverTouchesTheNetwork(t *testing.T) {
	lock := writeLock(t, `{
	  "typst": {"version": "0.15.1", "repo": "typst/typst", "digests": {"linux/amd64": "sha256:aa"}}
	}`)
	cache := t.TempDir()

	if _, _, err := exec(t, "path", "-lock", lock, "-cache", cache, "typst"); err == nil {
		t.Fatal("expected an error rather than a download")
	}

	entries, err := os.ReadDir(cache)
	if err != nil {
		t.Fatalf("read cache: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("path wrote %d entries into the cache; it should never install", len(entries))
	}
}

// TestFlagsAcceptedAfterSubcommand pins the ergonomics choice: flags are registered per
// subcommand so the natural ordering works.
func TestFlagsAcceptedAfterSubcommand(t *testing.T) {
	lock := writeLock(t, `{"typst": {"version": "1.2.3", "repo": "typst/typst"}}`)

	stdout, _, err := exec(t, "list", "-lock", lock)
	if err != nil {
		t.Fatalf("list -lock after subcommand: %v", err)
	}
	if !strings.Contains(stdout, "1.2.3") {
		t.Errorf("the -lock flag after the subcommand was not honoured:\n%s", stdout)
	}
}
