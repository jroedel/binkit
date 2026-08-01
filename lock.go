package binkit

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// LockEntry is one tool's pinned state.
//
// Digests are keyed by "GOOS/GOARCH" and hold the SHA-256 of the *release asset*, not
// of the extracted binary. They are captured for every platform the tool supports at
// pin time, so a lock file generated on Linux still yields a verified install on macOS.
type LockEntry struct {
	Version string            `json:"version"`
	Repo    string            `json:"repo"`
	Digests map[string]string `json:"digests,omitzero"`
}

// LockFile maps tool name to pin. It belongs in the consuming project's repository and
// is meant to be committed — it is what makes a build reproducible.
type LockFile map[string]LockEntry

// readLock loads the lock file. A missing file is not an error: it yields an empty
// LockFile, so a project that has never pinned anything gets a clear "not pinned"
// error from Ensure rather than a confusing I/O failure.
func readLock(path string) (LockFile, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return LockFile{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read lock file %s: %w", path, err)
	}

	lock := LockFile{}
	if err := json.Unmarshal(data, &lock); err != nil {
		return nil, fmt.Errorf("parse lock file %s: %w", path, err)
	}
	return lock, nil
}

// writeLock saves the lock file, staging through a temp file in the same directory so
// an interrupted write cannot truncate an existing pin. Map keys marshal in sorted
// order, which keeps the committed file diff-stable.
func writeLock(path string, lock LockFile) error {
	data, err := json.MarshalIndent(lock, "", "  ")
	if err != nil {
		return fmt.Errorf("encode lock file: %w", err)
	}
	data = append(data, '\n')

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create lock file directory %s: %w", dir, err)
	}

	tmp, err := os.CreateTemp(dir, ".binkit-lock-*.json")
	if err != nil {
		return fmt.Errorf("stage lock file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once the rename below succeeds

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write lock file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close staged lock file: %w", err)
	}

	// os.CreateTemp makes the file 0600, and the rename below would preserve that. A
	// lock file is committed and read by whoever can read the repository, so widen it
	// explicitly rather than inheriting the temp file's private mode.
	if err := os.Chmod(tmpName, 0o644); err != nil {
		return fmt.Errorf("set lock file permissions: %w", err)
	}

	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("install lock file %s: %w", path, err)
	}
	return nil
}
