package binkit

import (
	"bytes"
	"encoding/json"
	"io"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var testClock = time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)

const oldVersion = "1.0.0"

// nudgeEnv wires a Resolver whose clock and stderr the test controls, pinned at one
// version while upstream offers another.
type nudgeEnv struct {
	gh        *fakeGitHub
	r         *Resolver
	stderr    *bytes.Buffer
	tool      Tool
	now       time.Time
	statePath string
}

func newNudgeEnv(t *testing.T, pinnedVersion, latestVersion string) *nudgeEnv {
	t.Helper()

	tool := widgetTool()
	assets := widgetAssets(t, pinnedVersion, DefaultPlatforms)
	maps.Copy(assets, widgetAssets(t, latestVersion, DefaultPlatforms))

	env := &nudgeEnv{
		gh:     newFakeGitHub(t, "v"+latestVersion, assets, true),
		stderr: &bytes.Buffer{},
		tool:   tool,
		now:    testClock,
	}
	env.r = env.gh.resolver(t)
	env.r.Stderr = env.stderr
	env.r.Now = func() time.Time { return env.now }
	env.statePath = filepath.Join(env.r.CacheDir, "check", tool.Name+".json")

	pin(t, env.r, tool, pinnedVersion, assets)
	return env
}

func (e *nudgeEnv) ensure(t *testing.T) {
	t.Helper()
	if _, err := e.r.Ensure(t.Context(), e.tool); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
}

func (e *nudgeEnv) state(t *testing.T) checkState {
	t.Helper()

	data, err := os.ReadFile(e.statePath)
	if err != nil {
		t.Fatalf("read check state: %v", err)
	}
	var s checkState
	if err := json.Unmarshal(data, &s); err != nil {
		t.Fatalf("parse check state: %v", err)
	}
	return s
}

func TestUpdateCheckNudgesWhenNewerAvailable(t *testing.T) {
	env := newNudgeEnv(t, oldVersion, testVersion)
	env.ensure(t)

	out := env.stderr.String()
	for _, want := range []string{"widget", oldVersion, testVersion, "pinned", "available"} {
		if !strings.Contains(out, want) {
			t.Errorf("notice %q does not mention %q", out, want)
		}
	}
	if got := env.gh.apiHits.Load(); got != 1 {
		t.Errorf("API calls = %d, want 1", got)
	}

	s := env.state(t)
	if !s.LastCheck.Equal(testClock) {
		t.Errorf("LastCheck = %v, want %v", s.LastCheck, testClock)
	}
	if s.LatestSeen != testVersion {
		t.Errorf("LatestSeen = %q, want %q", s.LatestSeen, testVersion)
	}
}

func TestUpdateCheckIncludesHintWhenSet(t *testing.T) {
	env := newNudgeEnv(t, oldVersion, testVersion)
	env.r.UpdateHint = func(name string) string { return "massprep --update-tools " + name }
	env.ensure(t)

	if out := env.stderr.String(); !strings.Contains(out, "massprep --update-tools widget") {
		t.Errorf("notice %q does not include the update hint", out)
	}
}

func TestUpdateCheckOmitsHintLineWhenUnset(t *testing.T) {
	env := newNudgeEnv(t, oldVersion, testVersion)
	env.ensure(t)

	if out := env.stderr.String(); strings.Contains(out, "run:") {
		t.Errorf("notice %q has a run: line despite no UpdateHint", out)
	}
}

func TestUpdateCheckSilentWhenUpToDate(t *testing.T) {
	env := newNudgeEnv(t, testVersion, testVersion)
	env.ensure(t)

	if out := env.stderr.String(); out != "" {
		t.Errorf("notice shown while up to date: %q", out)
	}
	// The check still ran and was recorded, so it will not repeat before the interval.
	if got := env.gh.apiHits.Load(); got != 1 {
		t.Errorf("API calls = %d, want 1", got)
	}
	if s := env.state(t); !s.LastCheck.Equal(testClock) {
		t.Errorf("LastCheck = %v, want %v", s.LastCheck, testClock)
	}
}

func TestUpdateCheckSkippedWithinInterval(t *testing.T) {
	env := newNudgeEnv(t, oldVersion, testVersion)
	env.ensure(t)
	if got := env.gh.apiHits.Load(); got != 1 {
		t.Fatalf("first Ensure API calls = %d, want 1", got)
	}
	env.stderr.Reset()

	// One day later — well inside the default seven-day interval.
	env.now = testClock.Add(24 * time.Hour)
	env.ensure(t)

	if got := env.gh.apiHits.Load(); got != 1 {
		t.Errorf("API calls = %d, want 1 — a check inside the interval must not query", got)
	}
	if out := env.stderr.String(); out != "" {
		t.Errorf("notice repeated inside the interval: %q", out)
	}
}

func TestUpdateCheckRunsAgainAfterInterval(t *testing.T) {
	env := newNudgeEnv(t, oldVersion, testVersion)
	env.ensure(t)
	env.stderr.Reset()

	env.now = testClock.Add(DefaultCheckEvery + time.Minute)
	env.ensure(t)

	if got := env.gh.apiHits.Load(); got != 2 {
		t.Errorf("API calls = %d, want 2 — the check must resume after the interval", got)
	}
	if out := env.stderr.String(); !strings.Contains(out, testVersion) {
		t.Errorf("no notice after the interval elapsed: %q", out)
	}
}

func TestUpdateCheckRespectsCustomInterval(t *testing.T) {
	env := newNudgeEnv(t, oldVersion, testVersion)
	env.r.CheckEvery = time.Minute
	env.ensure(t)

	env.now = testClock.Add(2 * time.Minute)
	env.ensure(t)

	if got := env.gh.apiHits.Load(); got != 2 {
		t.Errorf("API calls = %d, want 2 with a one-minute interval", got)
	}
}

func TestUpdateCheckFailureRecordsAttemptNotCheck(t *testing.T) {
	env := newNudgeEnv(t, oldVersion, testVersion)
	env.gh.failAPI.Store(true)

	// A failing check must not fail the build.
	env.ensure(t)

	if out := env.stderr.String(); out != "" {
		t.Errorf("failed check produced output: %q", out)
	}

	s := env.state(t)
	if !s.LastAttempt.Equal(testClock) {
		t.Errorf("LastAttempt = %v, want %v", s.LastAttempt, testClock)
	}
	if !s.LastCheck.IsZero() {
		t.Errorf("LastCheck = %v, want zero — a failed check must not count as a check, "+
			"or one offline run would silence checking for a whole interval", s.LastCheck)
	}
}

func TestUpdateCheckBacksOffAfterFailure(t *testing.T) {
	env := newNudgeEnv(t, oldVersion, testVersion)
	env.gh.failAPI.Store(true)
	env.ensure(t)
	if got := env.gh.apiHits.Load(); got != 1 {
		t.Fatalf("first attempt API calls = %d, want 1", got)
	}

	// Half an hour later, inside the one-hour backoff.
	env.now = testClock.Add(30 * time.Minute)
	env.ensure(t)
	if got := env.gh.apiHits.Load(); got != 1 {
		t.Errorf("API calls = %d, want 1 — a retry inside the backoff must not query", got)
	}

	// Past the backoff, it tries again, and now upstream is reachable.
	env.now = testClock.Add(checkFailureBackoff + time.Minute)
	env.gh.failAPI.Store(false)
	env.ensure(t)
	if got := env.gh.apiHits.Load(); got != 2 {
		t.Errorf("API calls = %d, want 2 — the check must retry once the backoff expires", got)
	}
	if out := env.stderr.String(); !strings.Contains(out, testVersion) {
		t.Errorf("no notice after a successful retry: %q", out)
	}
}

func TestUpdateCheckSuppressedByEnv(t *testing.T) {
	t.Setenv(EnvNoUpdateCheck, "1")

	env := newNudgeEnv(t, oldVersion, testVersion)
	env.ensure(t)

	if got := env.gh.apiHits.Load(); got != 0 {
		t.Errorf("API calls = %d, want 0 with %s set", got, EnvNoUpdateCheck)
	}
	if out := env.stderr.String(); out != "" {
		t.Errorf("notice shown despite %s: %q", EnvNoUpdateCheck, out)
	}
}

func TestUpdateCheckSuppressedByNoCheckField(t *testing.T) {
	env := newNudgeEnv(t, oldVersion, testVersion)
	env.r.NoCheck = true
	env.ensure(t)

	if got := env.gh.apiHits.Load(); got != 0 {
		t.Errorf("API calls = %d, want 0 with NoCheck set", got)
	}
}

// TestUpdateCheckSkippedWhenStderrIsNotATerminal covers the CI case: with nowhere to
// display a notice, binkit should not spend a request discovering one.
func TestUpdateCheckSkippedWhenStderrIsNotATerminal(t *testing.T) {
	env := newNudgeEnv(t, oldVersion, testVersion)

	logFile, err := os.CreateTemp(t.TempDir(), "stderr-*.log")
	if err != nil {
		t.Fatalf("create temp stderr: %v", err)
	}
	t.Cleanup(func() { logFile.Close() })
	env.r.Stderr = logFile

	env.ensure(t)

	if got := env.gh.apiHits.Load(); got != 0 {
		t.Errorf("API calls = %d, want 0 when stderr is a regular file", got)
	}
	info, err := logFile.Stat()
	if err != nil {
		t.Fatalf("stat temp stderr: %v", err)
	}
	if info.Size() != 0 {
		t.Errorf("wrote %d bytes to a non-terminal stderr, want 0", info.Size())
	}
}

// TestUpdateCheckNeverWritesToStdout is the load-bearing guarantee: a consumer may be
// piping a generated document out of stdout, and a stray notice would corrupt it.
func TestUpdateCheckNeverWritesToStdout(t *testing.T) {
	orig := os.Stdout
	rd, wr, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stdout = wr
	t.Cleanup(func() { os.Stdout = orig })

	captured := make(chan string, 1)
	go func() {
		b, _ := io.ReadAll(rd)
		captured <- string(b)
	}()

	env := newNudgeEnv(t, oldVersion, testVersion)
	env.ensure(t)

	if err := wr.Close(); err != nil {
		t.Fatalf("close stdout pipe: %v", err)
	}
	os.Stdout = orig

	if got := <-captured; got != "" {
		t.Errorf("wrote %q to stdout; update notices must go to stderr only", got)
	}
	if env.stderr.Len() == 0 {
		t.Error("nothing written to stderr — the test proved nothing")
	}
}

func TestReadCheckStateToleratesGarbage(t *testing.T) {
	dir := t.TempDir()

	t.Run("missing file", func(t *testing.T) {
		if s := readCheckState(filepath.Join(dir, "absent.json")); s != (checkState{}) {
			t.Errorf("missing state = %+v, want zero", s)
		}
	})

	t.Run("corrupt file", func(t *testing.T) {
		path := filepath.Join(dir, "corrupt.json")
		if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
			t.Fatalf("write corrupt state: %v", err)
		}
		if s := readCheckState(path); s != (checkState{}) {
			t.Errorf("corrupt state = %+v, want zero", s)
		}
	})
}

func TestWriteCheckStateRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "widget.json")
	want := checkState{LastCheck: testClock, LastAttempt: testClock, LatestSeen: "9.9.9"}

	writeCheckState(path, want)

	got := readCheckState(path)
	if !got.LastCheck.Equal(want.LastCheck) || !got.LastAttempt.Equal(want.LastAttempt) {
		t.Errorf("timestamps = %+v, want %+v", got, want)
	}
	if got.LatestSeen != want.LatestSeen {
		t.Errorf("LatestSeen = %q, want %q", got.LatestSeen, want.LatestSeen)
	}
}

// TestCheckStateIsNotInTheLockFile guards the separation the design depends on:
// reproducible pins are committed, per-machine timestamps are not.
func TestCheckStateIsNotInTheLockFile(t *testing.T) {
	env := newNudgeEnv(t, oldVersion, testVersion)
	env.ensure(t)

	raw, err := os.ReadFile(env.r.lockPath())
	if err != nil {
		t.Fatalf("read lock: %v", err)
	}
	for _, forbidden := range []string{"last_check", "last_attempt", "latest_seen"} {
		if bytes.Contains(raw, []byte(forbidden)) {
			t.Errorf("lock file contains %q; check state must stay machine-local:\n%s", forbidden, raw)
		}
	}
	if _, err := os.Stat(env.statePath); err != nil {
		t.Errorf("check state not written to the cache directory: %v", err)
	}
}
