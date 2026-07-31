// Package binkit provisions, verifies, pins, and updates external CLI binaries that a
// Go program depends on but cannot build itself.
//
// The Go toolchain can manage Go-built tools via "go tool", but nothing in the toolchain
// helps with a Rust or C binary you shell out to. binkit fills that gap: it downloads a
// pinned release from GitHub, verifies it against a recorded SHA-256, caches it by
// version, and returns a path.
//
// # Pinning
//
// Pins live in a lock file — tools.json by convention — which belongs in the consuming
// project's repository. [Resolver.Ensure] reads the pin and never resolves "latest",
// never calls the GitHub API, and therefore never depends on API rate limits or a token.
// Only [Resolver.Update] changes a pin.
//
// # Design
//
// binkit knows nothing about any particular tool. A [Tool] is plain data supplied by the
// caller; ready-made definitions live in the separate catalog subpackage, which this
// package deliberately does not import. Ensure returns a path and never executes
// anything — what to run is the caller's decision.
package binkit

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

// Environment variables binkit consults.
const (
	// EnvCacheDir overrides the cache location.
	EnvCacheDir = "BINKIT_CACHE"

	// envToolPrefix is prepended to an upper-cased tool name to form the per-tool
	// override, e.g. BINKIT_TYPST=/usr/bin/typst.
	envToolPrefix = "BINKIT_"
)

// Errors reported by this package. All are wrapped, so match with [errors.Is].
var (
	ErrInvalidTool         = errors.New("binkit: invalid tool definition")
	ErrNotPinned           = errors.New("binkit: tool is not pinned")
	ErrNoDigest            = errors.New("binkit: no pinned digest for this platform")
	ErrDigestMismatch      = errors.New("binkit: digest mismatch")
	ErrUnsupportedPlatform = errors.New("binkit: tool is not published for this platform")
	ErrUnsupportedArchive  = errors.New("binkit: unsupported archive format")
	ErrBinaryNotInArchive  = errors.New("binkit: binary not found in archive")
	ErrAssetNotFound       = errors.New("binkit: release has no such asset")
)

// Platform is a GOOS/GOARCH pair.
type Platform struct {
	OS   string
	Arch string
}

func (p Platform) String() string { return p.OS + "/" + p.Arch }

// DefaultPlatforms is the set [Resolver.Update] records digests for. A tool that does
// not publish for one of these is skipped, so an over-broad list costs nothing.
var DefaultPlatforms = []Platform{
	{OS: "linux", Arch: "amd64"},
	{OS: "linux", Arch: "arm64"},
	{OS: "darwin", Arch: "amd64"},
	{OS: "darwin", Arch: "arm64"},
	{OS: "windows", Arch: "amd64"},
	{OS: "windows", Arch: "arm64"},
}

// Tool describes an external binary and where to obtain it. It is data, not behaviour:
// the two function fields exist only because asset naming varies per project and cannot
// be expressed as a format string in general.
type Tool struct {
	// Name keys the cache and the lock file, and forms the per-tool environment
	// override. Required.
	Name string

	// Repo is the GitHub "owner/name" hosting the releases. Required.
	Repo string

	// Tag maps a version to its release tag. Optional; defaults to "v" + version.
	Tag func(version string) string

	// Asset returns the release asset filename for a version and platform. Returning
	// an error means the tool is not published for that platform, which Update treats
	// as "skip" rather than as a failure. Required.
	Asset func(version, goos, goarch string) (string, error)

	// BinaryPath returns the path of the executable inside the archive. Required.
	BinaryPath func(version, goos, goarch string) string
}

// EnvKey is the environment variable that overrides this tool entirely, e.g.
// BINKIT_TYPST. When set, Ensure returns its value untouched.
func (t Tool) EnvKey() string {
	var b strings.Builder
	b.WriteString(envToolPrefix)
	for _, r := range strings.ToUpper(t.Name) {
		switch {
		case r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

func (t Tool) tag(version string) string {
	if t.Tag != nil {
		return t.Tag(version)
	}
	return "v" + version
}

func (t Tool) binName() string {
	if runtime.GOOS == "windows" {
		return t.Name + ".exe"
	}
	return t.Name
}

func (t Tool) validate() error {
	switch {
	case t.Name == "":
		return fmt.Errorf("%w: Name is required", ErrInvalidTool)
	case strings.ContainsAny(t.Name, `/\`):
		return fmt.Errorf("%w: Name %q must not contain a path separator", ErrInvalidTool, t.Name)
	case t.Repo == "":
		return fmt.Errorf("%w: %s: Repo is required", ErrInvalidTool, t.Name)
	case strings.Count(t.Repo, "/") != 1:
		return fmt.Errorf("%w: %s: Repo must be \"owner/name\", got %q", ErrInvalidTool, t.Name, t.Repo)
	case t.Asset == nil:
		return fmt.Errorf("%w: %s: Asset is required", ErrInvalidTool, t.Name)
	case t.BinaryPath == nil:
		return fmt.Errorf("%w: %s: BinaryPath is required", ErrInvalidTool, t.Name)
	default:
		return nil
	}
}

// Resolver installs tools. The zero value is usable: it caches under the user cache
// directory and reads tools.json from the working directory.
//
// A Resolver is safe for concurrent use and must not be copied after first use.
type Resolver struct {
	// CacheDir overrides the cache location. Falls back to $BINKIT_CACHE, then to
	// <user cache dir>/binkit.
	CacheDir string

	// Lock is the path to the lock file. Defaults to "tools.json".
	Lock string

	// HTTP is the client used for all requests. Defaults to [http.DefaultClient].
	HTTP *http.Client

	// Platforms are the platforms Update records digests for. Defaults to
	// [DefaultPlatforms].
	Platforms []Platform

	// Now returns the current time. Defaults to [time.Now].
	Now func() time.Time

	// Stderr receives update notices. Defaults to [os.Stderr]. Notices never go to
	// stdout, and are suppressed entirely when the default stderr is not a terminal —
	// a CI log has no one to read them.
	Stderr io.Writer

	// CheckEvery is the minimum interval between upstream update checks. Defaults to
	// [DefaultCheckEvery].
	CheckEvery time.Duration

	// NoCheck disables update checks. The BINKIT_NO_UPDATE_CHECK environment variable
	// does the same for an end user.
	NoCheck bool

	// UpdateHint returns the command that updates the named tool, shown as a second
	// line of the update notice. binkit cannot know a consuming CLI's flags, so
	// without this the notice reports versions only.
	UpdateHint func(toolName string) string

	// Test seams: the GitHub hosts to talk to. Empty means the real ones.
	apiBase      string
	downloadBase string

	// mu guards inFlight, which collapses concurrent installs of the same version
	// into one download.
	mu       sync.Mutex
	inFlight map[string]*installCall
}

// installCall is an install in progress that other callers can wait on.
type installCall struct {
	done chan struct{}
	path string
	err  error
}

func (r *Resolver) httpClient() *http.Client {
	return cmp.Or(r.HTTP, http.DefaultClient)
}

func (r *Resolver) lockPath() string {
	return cmp.Or(r.Lock, "tools.json")
}

func (r *Resolver) apiBaseURL() string {
	return cmp.Or(r.apiBase, defaultAPIBase)
}

func (r *Resolver) downloadBaseURL() string {
	return cmp.Or(r.downloadBase, defaultDownloadBase)
}

func (r *Resolver) platforms() []Platform {
	if len(r.Platforms) > 0 {
		return r.Platforms
	}
	return DefaultPlatforms
}

func (r *Resolver) cacheDir() (string, error) {
	if dir := cmp.Or(r.CacheDir, os.Getenv(EnvCacheDir)); dir != "" {
		return dir, nil
	}
	base, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("locate user cache directory: %w", err)
	}
	return filepath.Join(base, "binkit"), nil
}

func currentPlatform() Platform {
	return Platform{OS: runtime.GOOS, Arch: runtime.GOARCH}
}

// Ensure returns the path to the pinned version of t, downloading and verifying it if
// it is not already cached.
//
// It never reaches the network when the tool is already cached, and never contacts the
// GitHub API at all — a pinned version downloads by direct URL. A tool with no pin is
// an error rather than an implicit "fetch latest": changing what a build runs should be
// a deliberate, reviewable act.
func (r *Resolver) Ensure(ctx context.Context, t Tool) (string, error) {
	if err := t.validate(); err != nil {
		return "", err
	}

	// Escape hatch for CI images, Nix, and distro packages.
	if path := os.Getenv(t.EnvKey()); path != "" {
		return path, nil
	}

	lockPath := r.lockPath()
	lock, err := readLock(lockPath)
	if err != nil {
		return "", err
	}
	entry, ok := lock[t.Name]
	if !ok {
		return "", fmt.Errorf("%w: %s (looked in %s)", ErrNotPinned, t.Name, lockPath)
	}

	path, err := r.install(ctx, t, entry)
	if err != nil {
		return "", err
	}

	// Advisory only: this never changes what was installed, and any problem inside it
	// is swallowed rather than allowed to fail a build.
	r.checkForUpdate(ctx, t, entry)

	return path, nil
}

// Update resolves a version — the latest release when version is empty — installs it,
// and rewrites the lock file.
//
// Digests are recorded for every platform in [Resolver.Platforms] from the single
// release response, so one run on Linux produces a lock file that verifies correctly on
// macOS and Windows too.
func (r *Resolver) Update(ctx context.Context, t Tool, version string) (string, error) {
	if err := t.validate(); err != nil {
		return "", err
	}

	tag := ""
	if version != "" {
		tag = t.tag(version)
	}
	release, err := r.fetchRelease(ctx, t.Repo, tag)
	if err != nil {
		return "", err
	}
	resolved := strings.TrimPrefix(release.TagName, "v")

	digests := make(map[string]string, len(r.platforms()))
	for _, p := range r.platforms() {
		assetName, err := t.Asset(resolved, p.OS, p.Arch)
		if err != nil {
			continue // not published for this platform
		}
		asset, ok := release.findAsset(assetName)
		if !ok || asset.Digest == "" {
			continue
		}
		digests[p.String()] = asset.Digest
	}

	// Ensure refuses to install without a digest, so the current platform must have
	// one. GitHub omits the field on releases published before it existed; in that
	// case compute it directly.
	current := currentPlatform()
	if _, ok := digests[current.String()]; !ok {
		digest, err := r.computeAssetDigest(ctx, t, release, resolved)
		if err != nil {
			return "", err
		}
		digests[current.String()] = digest
	}

	lockPath := r.lockPath()
	lock, err := readLock(lockPath)
	if err != nil {
		return "", err
	}
	entry := LockEntry{Version: resolved, Repo: t.Repo, Digests: digests}
	lock[t.Name] = entry
	if err := writeLock(lockPath, lock); err != nil {
		return "", err
	}

	return r.install(ctx, t, entry)
}

// install returns the cached binary, downloading and verifying it on a cache miss.
//
// Concurrent calls for the same tool and version within one process share a single
// download: the first caller performs the work and the rest wait on its result. Across
// processes there is no shared lock — the atomic rename in doInstall settles that race
// instead, at the cost of the losing process having downloaded redundantly. That
// tradeoff is deliberate: a cross-process lock would mean either a dependency or a
// stale-lock recovery problem, and redundant downloads are merely wasteful, not wrong.
func (r *Resolver) install(ctx context.Context, t Tool, entry LockEntry) (string, error) {
	cache, err := r.cacheDir()
	if err != nil {
		return "", err
	}
	versionDir := filepath.Join(cache, t.Name, entry.Version)
	binPath := filepath.Join(versionDir, t.binName())

	if _, err := os.Stat(binPath); err == nil {
		return binPath, nil
	}

	r.mu.Lock()
	if leader, ok := r.inFlight[versionDir]; ok {
		r.mu.Unlock()
		<-leader.done
		return leader.path, leader.err
	}
	if r.inFlight == nil {
		r.inFlight = make(map[string]*installCall)
	}
	call := &installCall{done: make(chan struct{})}
	r.inFlight[versionDir] = call
	r.mu.Unlock()

	call.path, call.err = r.doInstall(ctx, t, entry, cache, versionDir, binPath)

	r.mu.Lock()
	delete(r.inFlight, versionDir)
	r.mu.Unlock()
	close(call.done)

	return call.path, call.err
}

// doInstall downloads, verifies, and atomically installs a single version.
func (r *Resolver) doInstall(ctx context.Context, t Tool, entry LockEntry, cache, versionDir, binPath string) (string, error) {
	current := currentPlatform()
	want, ok := entry.Digests[current.String()]
	if !ok {
		return "", fmt.Errorf("%w: %s %s on %s; re-pin on this platform to record one",
			ErrNoDigest, t.Name, entry.Version, current)
	}

	assetName, err := t.Asset(entry.Version, runtime.GOOS, runtime.GOARCH)
	if err != nil {
		return "", fmt.Errorf("%w: %s on %s: %w", ErrUnsupportedPlatform, t.Name, current, err)
	}

	// Stage on the same filesystem as the cache so the rename below is atomic rather
	// than a cross-device copy. This is why it is not os.TempDir().
	staging := filepath.Join(cache, "tmp")
	if err := os.MkdirAll(staging, 0o755); err != nil {
		return "", fmt.Errorf("create staging directory %s: %w", staging, err)
	}
	tmpDir, err := os.MkdirTemp(staging, t.Name+"-"+entry.Version+"-")
	if err != nil {
		return "", fmt.Errorf("create staging directory: %w", err)
	}
	defer os.RemoveAll(tmpDir) // no-op once the rename below succeeds

	// Base the local filename on the asset name but never trust it as a path.
	archive := filepath.Join(tmpDir, filepath.Base(assetName))
	url := r.downloadURL(entry.Repo, t.tag(entry.Version), assetName)
	if err := r.download(ctx, url, archive); err != nil {
		return "", err
	}
	if err := verifyDigest(archive, want); err != nil {
		return "", fmt.Errorf("verify %s: %w", assetName, err)
	}

	inner := t.BinaryPath(entry.Version, runtime.GOOS, runtime.GOARCH)
	if err := extractBinary(archive, assetName, inner, tmpDir, t.binName()); err != nil {
		return "", err
	}
	if err := os.Remove(archive); err != nil {
		return "", fmt.Errorf("remove staged archive: %w", err)
	}

	if err := os.MkdirAll(filepath.Dir(versionDir), 0o755); err != nil {
		return "", fmt.Errorf("create cache directory: %w", err)
	}
	if err := os.Rename(tmpDir, versionDir); err != nil {
		// A concurrent install of the same version may have won the race. Its result
		// is equally valid and already verified, so adopt it.
		if _, statErr := os.Stat(binPath); statErr == nil {
			return binPath, nil
		}
		return "", fmt.Errorf("install %s %s: %w", t.Name, entry.Version, err)
	}
	return binPath, nil
}

// computeAssetDigest downloads the current platform's asset solely to hash it. Used
// only when GitHub reports no digest, which means the asset is fetched once here and
// again during install — acceptable for a path that should be rare and getting rarer.
func (r *Resolver) computeAssetDigest(ctx context.Context, t Tool, release ghRelease, version string) (string, error) {
	assetName, err := t.Asset(version, runtime.GOOS, runtime.GOARCH)
	if err != nil {
		return "", fmt.Errorf("%w: %s on %s: %w", ErrUnsupportedPlatform, t.Name, currentPlatform(), err)
	}
	if _, ok := release.findAsset(assetName); !ok {
		return "", fmt.Errorf("%w: %s in %s", ErrAssetNotFound, assetName, release.TagName)
	}

	dir, err := os.MkdirTemp("", "binkit-digest-")
	if err != nil {
		return "", fmt.Errorf("create temp directory: %w", err)
	}
	defer os.RemoveAll(dir)

	dest := filepath.Join(dir, filepath.Base(assetName))
	if err := r.download(ctx, r.downloadURL(t.Repo, release.TagName, assetName), dest); err != nil {
		return "", err
	}
	return fileDigest(dest)
}

// fileDigest returns the file's SHA-256 in GitHub's "sha256:<hex>" form.
func fileDigest(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("hash %s: %w", path, err)
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), nil
}

// verifyDigest checks a file against an expected digest, accepting either the
// "sha256:<hex>" form or a bare hex string.
func verifyDigest(path, want string) error {
	expected := want
	if !strings.Contains(expected, ":") {
		expected = "sha256:" + expected
	}
	if algo, _, _ := strings.Cut(expected, ":"); !strings.EqualFold(algo, "sha256") {
		return fmt.Errorf("%w: unsupported digest algorithm %q", ErrDigestMismatch, algo)
	}

	got, err := fileDigest(path)
	if err != nil {
		return err
	}
	if !strings.EqualFold(got, expected) {
		return fmt.Errorf("%w: got %s, want %s", ErrDigestMismatch, got, expected)
	}
	return nil
}
