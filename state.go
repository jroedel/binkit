package binkit

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	// DefaultCheckEvery is the minimum interval between upstream update checks.
	DefaultCheckEvery = 7 * 24 * time.Hour

	// EnvNoUpdateCheck disables update checks when set to any non-empty value.
	EnvNoUpdateCheck = "BINKIT_NO_UPDATE_CHECK"

	// checkFailureBackoff is the wait after a failed check before trying again. It
	// exists so an offline week neither retries on every invocation nor suppresses
	// checking for a whole interval after one failure.
	checkFailureBackoff = time.Hour

	// checkTimeout bounds an update check. This is advisory work that must never
	// meaningfully delay a build, so it gets a short leash.
	checkTimeout = 3 * time.Second
)

// checkState is binkit's memory of when it last looked upstream for a tool.
//
// It is deliberately not part of the lock file. The lock file is committed and states
// what the project uses; this is machine-local bookkeeping about what this checkout has
// already asked GitHub. Merging them would drag a per-machine timestamp into version
// control and produce a diff on every build.
type checkState struct {
	// LastCheck is when an upstream query last succeeded.
	LastCheck time.Time `json:"last_check,omitzero"`

	// LastAttempt is when one was last tried, successfully or not.
	LastAttempt time.Time `json:"last_attempt,omitzero"`

	// LatestSeen is the newest version observed upstream.
	LatestSeen string `json:"latest_seen,omitzero"`
}

// readCheckState loads a tool's state. Every failure mode — absent, unreadable,
// corrupt — yields the zero value, which simply means "check now". This is a cache, not
// a source of truth; nothing here justifies failing a build.
func readCheckState(path string) checkState {
	data, err := os.ReadFile(path)
	if err != nil {
		return checkState{}
	}
	var state checkState
	if err := json.Unmarshal(data, &state); err != nil {
		return checkState{}
	}
	return state
}

// writeCheckState saves state, ignoring errors for the same reason.
func writeCheckState(path string, state checkState) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(path, append(data, '\n'), 0o644)
}

func (r *Resolver) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *Resolver) checkEvery() time.Duration {
	if r.CheckEvery > 0 {
		return r.CheckEvery
	}
	return DefaultCheckEvery
}

// isTerminal reports whether f is a character device, i.e. an interactive terminal
// rather than a pipe, file, or CI log.
func isTerminal(f *os.File) bool {
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

// nudgeTarget returns where an update notice should go, and whether showing one is
// appropriate at all.
//
// Notices go to stderr, never stdout — a caller's stdout may be a PDF or a pipe. When
// stderr is the default and not a terminal, there is no one to read the notice, so the
// check is skipped entirely rather than performed and discarded. A [Resolver.Stderr]
// explicitly set to something other than a file is taken as a deliberate choice by the
// caller and always written to; this is also what makes the behaviour testable.
func (r *Resolver) nudgeTarget() (io.Writer, bool) {
	if r.Stderr == nil {
		return os.Stderr, isTerminal(os.Stderr)
	}
	if f, ok := r.Stderr.(*os.File); ok {
		return f, isTerminal(f)
	}
	return r.Stderr, true
}

// checkForUpdate reports a newer upstream release, at most once per check interval.
//
// It never updates anything, never fails a build, and never returns an error: the worst
// outcome of a problem here is that a notice is not shown. Callers therefore ignore it.
func (r *Resolver) checkForUpdate(ctx context.Context, t Tool, entry LockEntry) {
	if r.NoCheck || os.Getenv(EnvNoUpdateCheck) != "" {
		return
	}
	out, canShow := r.nudgeTarget()
	if !canShow {
		return
	}

	cache, err := r.cacheDir()
	if err != nil {
		return
	}
	statePath := filepath.Join(cache, "check", t.Name+".json")
	state := readCheckState(statePath)
	now := r.now()

	// An attempt newer than the last successful check means the most recent try
	// failed; only then does the backoff apply. Testing LastAttempt unconditionally
	// would also suppress checks after a *successful* one whenever CheckEvery is
	// shorter than the backoff.
	lastAttemptFailed := state.LastAttempt.After(state.LastCheck)

	switch {
	case now.Sub(state.LastCheck) < r.checkEvery():
		return // checked recently enough
	case lastAttemptFailed && now.Sub(state.LastAttempt) < checkFailureBackoff:
		return // a recent attempt failed; do not hammer
	}

	reqCtx, cancel := context.WithTimeout(ctx, checkTimeout)
	defer cancel()

	release, err := r.fetchRelease(reqCtx, t.Repo, "")
	if err != nil {
		// A cancelled caller is not an upstream failure, so it should not consume the
		// backoff window.
		if ctx.Err() != nil {
			return
		}
		// Record the attempt but not the check: updating LastCheck here would silence
		// checking for a full interval after a single offline run, while recording
		// nothing would retry on every invocation for as long as the network is down.
		state.LastAttempt = now
		writeCheckState(statePath, state)
		return
	}

	latest := strings.TrimPrefix(release.TagName, "v")
	state.LastCheck = now
	state.LastAttempt = now
	state.LatestSeen = latest
	writeCheckState(statePath, state)

	if compareVersions(latest, entry.Version) <= 0 {
		return
	}

	fmt.Fprintf(out, "binkit: %s %s is pinned; %s is available.\n", t.Name, entry.Version, latest)
	if r.UpdateHint != nil {
		if hint := r.UpdateHint(t.Name); hint != "" {
			fmt.Fprintf(out, "        run: %s\n", hint)
		}
	}
}
