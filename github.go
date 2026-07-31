package binkit

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
)

const (
	defaultAPIBase      = "https://api.github.com"
	defaultDownloadBase = "https://github.com"
	userAgent           = "binkit (+https://github.com/jroedel/binkit)"
)

// ghAsset is one file attached to a release. Digest is GitHub's "sha256:<hex>" field;
// it is absent on releases published before GitHub began recording it, which is why
// Update has a fallback path that computes the digest itself.
type ghAsset struct {
	Name   string `json:"name"`
	URL    string `json:"browser_download_url"`
	Digest string `json:"digest"`
}

type ghRelease struct {
	TagName string    `json:"tag_name"`
	Assets  []ghAsset `json:"assets"`
}

// fetchRelease reads release metadata. An empty tag means "latest".
//
// This is the only function in binkit that touches the GitHub API. Ensure never calls
// it for a pinned tool — it builds the download URL directly — so ordinary builds are
// immune to the 60 requests/hour unauthenticated rate limit and need no token.
func (r *Resolver) fetchRelease(ctx context.Context, repo, tag string) (ghRelease, error) {
	url := r.apiBaseURL() + "/repos/" + repo + "/releases/latest"
	if tag != "" {
		url = r.apiBaseURL() + "/repos/" + repo + "/releases/tags/" + tag
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return ghRelease{}, fmt.Errorf("build request for %s: %w", url, err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", userAgent)
	// Optional: lifts the rate limit and reaches private repos. Never required.
	if token := os.Getenv("GITHUB_TOKEN"); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := r.httpClient().Do(req)
	if err != nil {
		return ghRelease{}, fmt.Errorf("query %s: %w", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return ghRelease{}, fmt.Errorf("query %s: %s: %s", url, resp.Status, snippet(resp.Body))
	}

	var release ghRelease
	if err := json.NewDecoder(resp.Body).Decode(&release); err != nil {
		return ghRelease{}, fmt.Errorf("decode release from %s: %w", url, err)
	}
	return release, nil
}

// findAsset locates a named asset in a release.
func (rel ghRelease) findAsset(name string) (ghAsset, bool) {
	for _, a := range rel.Assets {
		if a.Name == name {
			return a, true
		}
	}
	return ghAsset{}, false
}

// downloadURL builds a release-asset URL directly, with no API call.
func (r *Resolver) downloadURL(repo, tag, asset string) string {
	return r.downloadBaseURL() + "/" + repo + "/releases/download/" + tag + "/" + asset
}

// download streams a URL to dest.
func (r *Resolver) download(ctx context.Context, url, dest string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("build request for %s: %w", url, err)
	}
	req.Header.Set("User-Agent", userAgent)

	resp, err := r.httpClient().Do(req)
	if err != nil {
		return fmt.Errorf("download %s: %w", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download %s: %s: %s", url, resp.Status, snippet(resp.Body))
	}

	f, err := os.Create(dest)
	if err != nil {
		return fmt.Errorf("create %s: %w", dest, err)
	}
	if _, err := io.Copy(f, resp.Body); err != nil {
		f.Close()
		return fmt.Errorf("write %s: %w", dest, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close %s: %w", dest, err)
	}
	return nil
}

// snippet reads a short prefix of an error response body so failures carry GitHub's
// own explanation ("rate limit exceeded", "Not Found") instead of only a status code.
func snippet(r io.Reader) string {
	b, err := io.ReadAll(io.LimitReader(r, 256))
	if err != nil || len(b) == 0 {
		return "(no response body)"
	}
	return strings.TrimSpace(strings.ReplaceAll(string(b), "\n", " "))
}
