package binkit

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"cmp"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/ulikunitz/xz"
)

const testVersion = "1.4.2"

// widgetTool is a synthetic tool shaped like a real one: per-platform asset names, a
// target-named directory inside the archive, zip on Windows and tar.xz elsewhere, and
// one platform it is simply not published for.
func widgetTool() Tool {
	return Tool{
		Name: "widget",
		Repo: "acme/widget",
		Asset: func(version, goos, goarch string) (string, error) {
			if goos == "plan9" {
				return "", fmt.Errorf("widget publishes no build for %s/%s", goos, goarch)
			}
			if goos == "windows" {
				return fmt.Sprintf("widget-%s-%s-%s.zip", version, goos, goarch), nil
			}
			return fmt.Sprintf("widget-%s-%s-%s.tar.xz", version, goos, goarch), nil
		},
		BinaryPath: func(version, goos, goarch string) string {
			bin := "widget"
			if goos == "windows" {
				bin = "widget.exe"
			}
			return fmt.Sprintf("widget-%s-%s-%s/%s", version, goos, goarch, bin)
		},
	}
}

func widgetPayload(p Platform) []byte {
	return fmt.Appendf(nil, "#!/bin/sh\necho widget %s\n", p)
}

// widgetAssets builds a genuine archive per platform, so tests exercise real xz and
// zip decoding rather than a stub.
func widgetAssets(t *testing.T, version string, platforms []Platform) map[string][]byte {
	t.Helper()

	tool := widgetTool()
	assets := make(map[string][]byte, len(platforms))
	for _, p := range platforms {
		name, err := tool.Asset(version, p.OS, p.Arch)
		if err != nil {
			continue
		}
		inner := tool.BinaryPath(version, p.OS, p.Arch)
		payload := widgetPayload(p)
		if p.OS == "windows" {
			assets[name] = buildZip(t, inner, payload)
			continue
		}
		assets[name] = buildTarXZ(t, inner, payload)
	}
	return assets
}

func buildTarXZ(t *testing.T, inner string, payload []byte) []byte {
	t.Helper()

	var buf bytes.Buffer
	xw, err := xz.NewWriter(&buf)
	if err != nil {
		t.Fatalf("new xz writer: %v", err)
	}
	writeTar(t, xw, inner, payload)
	if err := xw.Close(); err != nil {
		t.Fatalf("close xz writer: %v", err)
	}
	return buf.Bytes()
}

func buildTarGZ(t *testing.T, inner string, payload []byte) []byte {
	t.Helper()

	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	writeTar(t, gw, inner, payload)
	if err := gw.Close(); err != nil {
		t.Fatalf("close gzip writer: %v", err)
	}
	return buf.Bytes()
}

func writeTar(t *testing.T, w io.Writer, inner string, payload []byte) {
	t.Helper()

	tw := tar.NewWriter(w)
	// Real release archives carry a directory entry ahead of the binary; include one
	// so the extractor is exercised against a stream containing non-regular entries.
	if dir := path.Dir(inner); dir != "." {
		if err := tw.WriteHeader(&tar.Header{Name: dir + "/", Mode: 0o755, Typeflag: tar.TypeDir}); err != nil {
			t.Fatalf("write tar dir header: %v", err)
		}
	}
	hdr := &tar.Header{Name: inner, Mode: 0o755, Size: int64(len(payload)), Typeflag: tar.TypeReg}
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatalf("write tar header: %v", err)
	}
	if _, err := tw.Write(payload); err != nil {
		t.Fatalf("write tar body: %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar writer: %v", err)
	}
}

func buildZip(t *testing.T, inner string, payload []byte) []byte {
	t.Helper()

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create(inner)
	if err != nil {
		t.Fatalf("create zip entry: %v", err)
	}
	if _, err := w.Write(payload); err != nil {
		t.Fatalf("write zip entry: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("close zip writer: %v", err)
	}
	return buf.Bytes()
}

// fakeGitHub serves both the release API and the asset downloads, counting each
// separately so tests can assert on exactly which kind of traffic occurred.
type fakeGitHub struct {
	srv         *httptest.Server
	tag         string
	assets      map[string][]byte
	digests     map[string]string
	withDigests bool

	apiHits atomic.Int64
	dlHits  atomic.Int64
}

func newFakeGitHub(t *testing.T, tag string, assets map[string][]byte, withDigests bool) *fakeGitHub {
	t.Helper()

	f := &fakeGitHub{
		tag:         tag,
		assets:      assets,
		digests:     make(map[string]string, len(assets)),
		withDigests: withDigests,
	}
	for name, body := range assets {
		sum := sha256.Sum256(body)
		f.digests[name] = "sha256:" + hex.EncodeToString(sum[:])
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/{owner}/{repo}/releases/latest", f.serveRelease)
	mux.HandleFunc("GET /repos/{owner}/{repo}/releases/tags/{tag}", f.serveRelease)
	mux.HandleFunc("GET /{owner}/{repo}/releases/download/{tag}/{asset}", f.serveDownload)

	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeGitHub) serveRelease(w http.ResponseWriter, r *http.Request) {
	f.apiHits.Add(1)

	if tag := r.PathValue("tag"); tag != "" && tag != f.tag {
		http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
		return
	}
	owner, repo := r.PathValue("owner"), r.PathValue("repo")

	release := ghRelease{TagName: f.tag}
	for name := range f.assets {
		asset := ghAsset{
			Name: name,
			URL:  fmt.Sprintf("%s/%s/%s/releases/download/%s/%s", f.srv.URL, owner, repo, f.tag, name),
		}
		if f.withDigests {
			asset.Digest = f.digests[name]
		}
		release.Assets = append(release.Assets, asset)
	}
	slices.SortFunc(release.Assets, func(a, b ghAsset) int { return cmp.Compare(a.Name, b.Name) })

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(release)
}

func (f *fakeGitHub) serveDownload(w http.ResponseWriter, r *http.Request) {
	f.dlHits.Add(1)

	body, ok := f.assets[r.PathValue("asset")]
	if !ok {
		http.Error(w, "no such asset", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	_, _ = w.Write(body)
}

func (f *fakeGitHub) resolver(t *testing.T) *Resolver {
	t.Helper()

	return &Resolver{
		CacheDir:     t.TempDir(),
		Lock:         filepath.Join(t.TempDir(), "tools.json"),
		apiBase:      f.srv.URL,
		downloadBase: f.srv.URL,
	}
}

// pin writes a lock file directly, so a test can reach a cold-cache-but-pinned state
// without going through Update and its download.
func pin(t *testing.T, r *Resolver, tool Tool, version string, assets map[string][]byte) {
	t.Helper()

	name, err := tool.Asset(version, runtime.GOOS, runtime.GOARCH)
	if err != nil {
		t.Fatalf("asset name for current platform: %v", err)
	}
	sum := sha256.Sum256(assets[name])

	lock := LockFile{tool.Name: {
		Version: version,
		Repo:    tool.Repo,
		Digests: map[string]string{currentPlatform().String(): "sha256:" + hex.EncodeToString(sum[:])},
	}}
	if err := writeLock(r.lockPath(), lock); err != nil {
		t.Fatalf("write lock: %v", err)
	}
}

func TestEnsureRejectsInvalidTool(t *testing.T) {
	tests := []struct {
		name string
		tool Tool
	}{
		{"no name", Tool{Repo: "a/b", Asset: okAsset, BinaryPath: okBinaryPath}},
		{"name with separator", Tool{Name: "a/b", Repo: "a/b", Asset: okAsset, BinaryPath: okBinaryPath}},
		{"no repo", Tool{Name: "x", Asset: okAsset, BinaryPath: okBinaryPath}},
		{"repo not owner/name", Tool{Name: "x", Repo: "justname", Asset: okAsset, BinaryPath: okBinaryPath}},
		{"repo with too many slashes", Tool{Name: "x", Repo: "a/b/c", Asset: okAsset, BinaryPath: okBinaryPath}},
		{"no asset func", Tool{Name: "x", Repo: "a/b", BinaryPath: okBinaryPath}},
		{"no binary path func", Tool{Name: "x", Repo: "a/b", Asset: okAsset}},
	}

	r := &Resolver{}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := r.Ensure(t.Context(), tc.tool); !errors.Is(err, ErrInvalidTool) {
				t.Errorf("Ensure error = %v, want ErrInvalidTool", err)
			}
		})
	}
}

func okAsset(_, _, _ string) (string, error) { return "x.tar.xz", nil }
func okBinaryPath(_, _, _ string) string     { return "x" }

func TestEnsureUnpinnedToolIsAnError(t *testing.T) {
	gh := newFakeGitHub(t, "v"+testVersion, nil, true)
	r := gh.resolver(t)

	_, err := r.Ensure(t.Context(), widgetTool())
	if !errors.Is(err, ErrNotPinned) {
		t.Fatalf("Ensure error = %v, want ErrNotPinned", err)
	}
	if hits := gh.apiHits.Load() + gh.dlHits.Load(); hits != 0 {
		t.Errorf("unpinned Ensure made %d requests, want 0 — it must never fall back to 'latest'", hits)
	}
}

func TestEnsureEnvOverrideWins(t *testing.T) {
	gh := newFakeGitHub(t, "v"+testVersion, nil, true)
	r := gh.resolver(t)
	tool := widgetTool()

	const override = "/usr/local/bin/widget"
	t.Setenv(tool.EnvKey(), override)

	got, err := r.Ensure(t.Context(), tool)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if got != override {
		t.Errorf("Ensure = %q, want %q", got, override)
	}
	if hits := gh.apiHits.Load() + gh.dlHits.Load(); hits != 0 {
		t.Errorf("override still made %d requests, want 0", hits)
	}
}

func TestEnsureInstallsVerifiesAndCaches(t *testing.T) {
	assets := widgetAssets(t, testVersion, DefaultPlatforms)
	gh := newFakeGitHub(t, "v"+testVersion, assets, true)
	r := gh.resolver(t)
	tool := widgetTool()
	pin(t, r, tool, testVersion, assets)

	binPath, err := r.Ensure(t.Context(), tool)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}

	// A pinned install downloads directly and must never touch the API.
	if got := gh.apiHits.Load(); got != 0 {
		t.Errorf("pinned Ensure made %d API calls, want 0", got)
	}
	if got := gh.dlHits.Load(); got != 1 {
		t.Errorf("downloads = %d, want 1", got)
	}

	content, err := os.ReadFile(binPath)
	if err != nil {
		t.Fatalf("read installed binary: %v", err)
	}
	if want := widgetPayload(currentPlatform()); !bytes.Equal(content, want) {
		t.Errorf("installed content = %q, want %q", content, want)
	}

	info, err := os.Stat(binPath)
	if err != nil {
		t.Fatalf("stat installed binary: %v", err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Errorf("installed binary is not executable: mode %v", info.Mode().Perm())
	}

	// The staged archive must not survive into the cache.
	entries, err := os.ReadDir(filepath.Dir(binPath))
	if err != nil {
		t.Fatalf("read version dir: %v", err)
	}
	if len(entries) != 1 {
		names := make([]string, len(entries))
		for i, e := range entries {
			names[i] = e.Name()
		}
		t.Errorf("version dir holds %v, want only the binary", names)
	}

	// Second call is a pure cache hit.
	again, err := r.Ensure(t.Context(), tool)
	if err != nil {
		t.Fatalf("second Ensure: %v", err)
	}
	if again != binPath {
		t.Errorf("second Ensure = %q, want %q", again, binPath)
	}
	if got := gh.dlHits.Load(); got != 1 {
		t.Errorf("cached Ensure downloaded again: total downloads = %d, want 1", got)
	}
}

func TestEnsureDigestMismatchLeavesNothingInstalled(t *testing.T) {
	assets := widgetAssets(t, testVersion, DefaultPlatforms)
	gh := newFakeGitHub(t, "v"+testVersion, assets, true)
	r := gh.resolver(t)
	tool := widgetTool()

	lock := LockFile{tool.Name: {
		Version: testVersion,
		Repo:    tool.Repo,
		Digests: map[string]string{currentPlatform().String(): "sha256:" + strings.Repeat("00", 32)},
	}}
	if err := writeLock(r.lockPath(), lock); err != nil {
		t.Fatalf("write lock: %v", err)
	}

	if _, err := r.Ensure(t.Context(), tool); !errors.Is(err, ErrDigestMismatch) {
		t.Fatalf("Ensure error = %v, want ErrDigestMismatch", err)
	}

	versionDir := filepath.Join(r.CacheDir, tool.Name, testVersion)
	if _, err := os.Stat(versionDir); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("version directory exists after failed verification (stat err = %v); "+
			"a rejected download must leave nothing installed", err)
	}

	staged, err := os.ReadDir(filepath.Join(r.CacheDir, "tmp"))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("read staging dir: %v", err)
	}
	if len(staged) != 0 {
		t.Errorf("staging directory still holds %d entries, want 0", len(staged))
	}
}

func TestEnsureRefusesWithoutDigestForThisPlatform(t *testing.T) {
	assets := widgetAssets(t, testVersion, DefaultPlatforms)
	gh := newFakeGitHub(t, "v"+testVersion, assets, true)
	r := gh.resolver(t)
	tool := widgetTool()

	// A lock file pinned on some other platform only.
	lock := LockFile{tool.Name: {
		Version: testVersion,
		Repo:    tool.Repo,
		Digests: map[string]string{"solaris/sparc64": "sha256:" + strings.Repeat("ab", 32)},
	}}
	if err := writeLock(r.lockPath(), lock); err != nil {
		t.Fatalf("write lock: %v", err)
	}

	if _, err := r.Ensure(t.Context(), tool); !errors.Is(err, ErrNoDigest) {
		t.Fatalf("Ensure error = %v, want ErrNoDigest", err)
	}
	if got := gh.dlHits.Load(); got != 0 {
		t.Errorf("downloaded %d times without a digest to verify against, want 0", got)
	}
}

func TestEnsureConcurrentSharesOneDownload(t *testing.T) {
	assets := widgetAssets(t, testVersion, DefaultPlatforms)
	gh := newFakeGitHub(t, "v"+testVersion, assets, true)
	r := gh.resolver(t)
	tool := widgetTool()
	pin(t, r, tool, testVersion, assets)

	const goroutines = 8
	paths := make([]string, goroutines)
	errs := make([]error, goroutines)

	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range goroutines {
		wg.Go(func() {
			<-start
			paths[i], errs[i] = r.Ensure(t.Context(), tool)
		})
	}
	close(start) // release them together to maximise overlap
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("goroutine %d: %v", i, err)
		}
	}
	for i, p := range paths {
		if p != paths[0] {
			t.Errorf("goroutine %d returned %q, want %q — all callers must agree", i, p, paths[0])
		}
	}
	if got := gh.dlHits.Load(); got != 1 {
		t.Errorf("downloads = %d, want 1 — concurrent Ensure calls must share one fetch", got)
	}
}

func TestUpdateRecordsDigestsForAllPlatforms(t *testing.T) {
	assets := widgetAssets(t, testVersion, DefaultPlatforms)
	gh := newFakeGitHub(t, "v"+testVersion, assets, true)
	r := gh.resolver(t)
	tool := widgetTool()

	if _, err := r.Update(t.Context(), tool, ""); err != nil {
		t.Fatalf("Update: %v", err)
	}

	lock, err := readLock(r.lockPath())
	if err != nil {
		t.Fatalf("read lock: %v", err)
	}
	entry, ok := lock[tool.Name]
	if !ok {
		t.Fatalf("lock has no entry for %s", tool.Name)
	}
	if entry.Version != testVersion {
		t.Errorf("pinned version = %q, want %q (the leading v must be stripped)", entry.Version, testVersion)
	}
	if entry.Repo != tool.Repo {
		t.Errorf("pinned repo = %q, want %q", entry.Repo, tool.Repo)
	}

	for _, p := range DefaultPlatforms {
		assetName, err := tool.Asset(testVersion, p.OS, p.Arch)
		if err != nil {
			continue
		}
		sum := sha256.Sum256(assets[assetName])
		want := "sha256:" + hex.EncodeToString(sum[:])
		if got := entry.Digests[p.String()]; got != want {
			t.Errorf("digest for %s = %q, want %q", p, got, want)
		}
	}

	// The whole point of reading digests from release metadata: every platform is
	// pinned from a single API response, with only the local binary downloaded.
	if got := gh.apiHits.Load(); got != 1 {
		t.Errorf("API calls = %d, want 1", got)
	}
	if got := gh.dlHits.Load(); got != 1 {
		t.Errorf("downloads = %d, want 1 (only the current platform)", got)
	}
}

func TestUpdateComputesDigestWhenReleaseOmitsIt(t *testing.T) {
	assets := widgetAssets(t, testVersion, DefaultPlatforms)
	gh := newFakeGitHub(t, "v"+testVersion, assets, false) // no digests in metadata
	r := gh.resolver(t)
	tool := widgetTool()

	if _, err := r.Update(t.Context(), tool, ""); err != nil {
		t.Fatalf("Update: %v", err)
	}

	lock, err := readLock(r.lockPath())
	if err != nil {
		t.Fatalf("read lock: %v", err)
	}
	entry := lock[tool.Name]

	current := currentPlatform().String()
	assetName, err := tool.Asset(testVersion, runtime.GOOS, runtime.GOARCH)
	if err != nil {
		t.Fatalf("asset for current platform: %v", err)
	}
	sum := sha256.Sum256(assets[assetName])
	if want := "sha256:" + hex.EncodeToString(sum[:]); entry.Digests[current] != want {
		t.Errorf("computed digest = %q, want %q", entry.Digests[current], want)
	}

	// Only the current platform can be verified without metadata — the others are
	// deliberately left unpinned rather than recorded unverified.
	if len(entry.Digests) != 1 {
		t.Errorf("recorded %d digests, want 1 (only the platform we could hash)", len(entry.Digests))
	}

	// This path costs an extra download: one to hash, one to install.
	if got := gh.dlHits.Load(); got != 2 {
		t.Errorf("downloads = %d, want 2 (hash then install)", got)
	}
}

func TestUpdatePinsRequestedVersion(t *testing.T) {
	assets := widgetAssets(t, testVersion, DefaultPlatforms)
	gh := newFakeGitHub(t, "v"+testVersion, assets, true)
	r := gh.resolver(t)
	tool := widgetTool()

	if _, err := r.Update(t.Context(), tool, testVersion); err != nil {
		t.Fatalf("Update: %v", err)
	}
	lock, err := readLock(r.lockPath())
	if err != nil {
		t.Fatalf("read lock: %v", err)
	}
	if got := lock[tool.Name].Version; got != testVersion {
		t.Errorf("pinned version = %q, want %q", got, testVersion)
	}
}

func TestUpdateUnknownVersionFails(t *testing.T) {
	assets := widgetAssets(t, testVersion, DefaultPlatforms)
	gh := newFakeGitHub(t, "v"+testVersion, assets, true)
	r := gh.resolver(t)

	if _, err := r.Update(t.Context(), widgetTool(), "9.9.9"); err == nil {
		t.Fatal("Update with an unknown version succeeded, want an error")
	}
	if _, err := os.Stat(r.lockPath()); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a failed Update wrote a lock file (stat err = %v)", err)
	}
}

func TestToolEnvKey(t *testing.T) {
	tests := []struct{ name, want string }{
		{"typst", "BINKIT_TYPST"},
		{"go-task", "BINKIT_GO_TASK"},
		{"a.b", "BINKIT_A_B"},
		{"qpdf2", "BINKIT_QPDF2"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := (Tool{Name: tc.name}).EnvKey(); got != tc.want {
				t.Errorf("EnvKey() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestToolTagDefaultsToVPrefix(t *testing.T) {
	if got := widgetTool().tag("1.2.3"); got != "v1.2.3" {
		t.Errorf("default tag = %q, want %q", got, "v1.2.3")
	}

	custom := widgetTool()
	custom.Tag = func(v string) string { return "release-" + v }
	if got := custom.tag("1.2.3"); got != "release-1.2.3" {
		t.Errorf("custom tag = %q, want %q", got, "release-1.2.3")
	}
}

func TestVerifyDigest(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "payload")
	body := []byte("hello binkit")
	if err := os.WriteFile(file, body, 0o600); err != nil {
		t.Fatalf("write payload: %v", err)
	}
	sum := sha256.Sum256(body)
	hexSum := hex.EncodeToString(sum[:])

	t.Run("prefixed form", func(t *testing.T) {
		if err := verifyDigest(file, "sha256:"+hexSum); err != nil {
			t.Errorf("verifyDigest: %v", err)
		}
	})
	t.Run("bare hex form", func(t *testing.T) {
		if err := verifyDigest(file, hexSum); err != nil {
			t.Errorf("verifyDigest: %v", err)
		}
	})
	t.Run("uppercase hex", func(t *testing.T) {
		if err := verifyDigest(file, strings.ToUpper(hexSum)); err != nil {
			t.Errorf("verifyDigest: %v", err)
		}
	})
	t.Run("mismatch", func(t *testing.T) {
		if err := verifyDigest(file, "sha256:"+strings.Repeat("00", 32)); !errors.Is(err, ErrDigestMismatch) {
			t.Errorf("verifyDigest error = %v, want ErrDigestMismatch", err)
		}
	})
	t.Run("unsupported algorithm", func(t *testing.T) {
		if err := verifyDigest(file, "md5:"+strings.Repeat("00", 16)); !errors.Is(err, ErrDigestMismatch) {
			t.Errorf("verifyDigest error = %v, want ErrDigestMismatch", err)
		}
	})
}

func TestLockRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tools.json")

	empty, err := readLock(path)
	if err != nil {
		t.Fatalf("read missing lock: %v", err)
	}
	if len(empty) != 0 {
		t.Errorf("missing lock returned %d entries, want 0", len(empty))
	}

	want := LockFile{
		"widget": {Version: "1.0.0", Repo: "acme/widget", Digests: map[string]string{"linux/amd64": "sha256:aa"}},
		"gadget": {Version: "2.0.0", Repo: "acme/gadget", Digests: map[string]string{"darwin/arm64": "sha256:bb"}},
	}
	if err := writeLock(path, want); err != nil {
		t.Fatalf("write lock: %v", err)
	}
	got, err := readLock(path)
	if err != nil {
		t.Fatalf("read lock: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("read back %d entries, want %d", len(got), len(want))
	}
	for name, wantEntry := range want {
		gotEntry := got[name]
		if gotEntry.Version != wantEntry.Version || gotEntry.Repo != wantEntry.Repo {
			t.Errorf("%s = %+v, want %+v", name, gotEntry, wantEntry)
		}
	}

	// Committed files should be diff-stable, so keys must marshal in sorted order.
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read raw lock: %v", err)
	}
	if !bytes.Contains(raw, []byte("\"gadget\"")) ||
		bytes.Index(raw, []byte("\"gadget\"")) > bytes.Index(raw, []byte("\"widget\"")) {
		t.Errorf("lock keys are not sorted:\n%s", raw)
	}
	if !bytes.HasSuffix(raw, []byte("\n")) {
		t.Error("lock file does not end in a newline")
	}
}
