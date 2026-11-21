// Package githubrelease fetches release metadata and downloads release assets
// from GitHub over unauthenticated, SSRF-hardened HTTPS connections.
package githubrelease

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/db-branch/db-branch/internal/domain"
	"github.com/db-branch/db-branch/internal/infrastructure/artifact"
)

const (
	// DefaultAPIBase is the default GitHub REST API base URL.
	DefaultAPIBase = "https://api.github.com"
	// MaxReleaseJSONBytes caps the size of a GitHub Releases API JSON response (2 MiB).
	MaxReleaseJSONBytes int64 = 2 << 20

	userAgent        = "db-branch"
	githubAccept     = "application/vnd.github+json"
	githubAPIVersion = "2022-11-28"
)

var (
	// ErrDraftRelease is returned when a release is marked as a draft.
	ErrDraftRelease = errors.New("github release is a draft")
	// ErrPrerelease is returned when a release is marked as a prerelease.
	ErrPrerelease = errors.New("github release is a prerelease")
	// ErrRateLimited is returned when the GitHub API unauthenticated rate limit is exhausted.
	ErrRateLimited = errors.New("github API rate limit exceeded")
	// ErrResponseTooLarge is returned when an API response or downloaded asset exceeds its size cap.
	ErrResponseTooLarge = errors.New("github response exceeds size limit")
)

var (
	allowedAPIHosts = []string{
		"api.github.com",
	}
	allowedAssetHosts = []string{
		"github.com",
		"api.github.com",
		"objects.githubusercontent.com",
		"release-assets.githubusercontent.com",
		"github-releases.githubusercontent.com",
	}

	defaultAPIClient   = artifact.NewPublicOnlyHostClient(allowedAPIHosts)
	defaultAssetClient = artifact.NewPublicOnlyHostClient(allowedAssetHosts)
)

// AllowedAPIHosts returns the allowlist of hosts permitted for GitHub API requests.
func AllowedAPIHosts() []string {
	return append([]string(nil), allowedAPIHosts...)
}

// AllowedAssetHosts returns the allowlist of hosts permitted for GitHub release asset downloads.
func AllowedAssetHosts() []string {
	return append([]string(nil), allowedAssetHosts...)
}

// Asset describes one downloadable file attached to a GitHub Release.
type Asset struct {
	Name        string
	DownloadURL string
	Size        int64
}

// Release describes a GitHub Release returned by the GitHub REST API.
type Release struct {
	Tag        string
	Prerelease bool
	Draft      bool
	Assets     []Asset
}

// Client queries GitHub Releases and downloads release assets using separate
// host-restricted, public-only HTTPS clients for API calls and asset downloads.
type Client struct {
	// APIBase overrides DefaultAPIBase when non-empty.
	APIBase string
	// HTTP is used for GitHub API requests (default: public-only client restricted to api.github.com).
	HTTP *http.Client
	// AssetHTTP is used for asset downloads (default: public-only client restricted to AllowedAssetHosts).
	AssetHTTP *http.Client
	// Resolve looks up host names for domain.ValidateCatalogURL (default: artifact.SystemResolver).
	Resolve domain.Resolver
}

func (c *Client) apiBase() string {
	if c.APIBase != "" {
		return strings.TrimRight(c.APIBase, "/")
	}
	return DefaultAPIBase
}

func (c *Client) apiClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return defaultAPIClient
}

func (c *Client) assetClient() *http.Client {
	if c.AssetHTTP != nil {
		return c.AssetHTTP
	}
	return defaultAssetClient
}

func (c *Client) resolve() domain.Resolver {
	if c.Resolve != nil {
		return c.Resolve
	}
	return artifact.SystemResolver
}

func (c *Client) validateURL(ctx context.Context, rawURL string, hosts []string) error {
	res := c.resolve()
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("%w %q: %v", domain.ErrUnsafeURL, rawURL, err)
	}
	if u.Scheme != "https" || u.User != nil {
		return fmt.Errorf("%w %q: only https without credentials is allowed", domain.ErrUnsafeURL, u.Redacted())
	}
	if p := u.Port(); p != "" && p != "443" {
		return fmt.Errorf("%w %q: port %q is not allowed (only 443)", domain.ErrUnsafeURL, u.Redacted(), p)
	}
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	if !hostAllowed(host, hosts) {
		return fmt.Errorf("%w %q: host %q is not allowed", domain.ErrUnsafeURL, u.Redacted(), host)
	}
	return domain.ValidateCatalogURL(ctx, rawURL, res)
}

func hostAllowed(host string, allowed []string) bool {
	if host == "" {
		return false
	}
	for _, h := range allowed {
		if host == strings.TrimSuffix(strings.ToLower(h), ".") {
			return true
		}
	}
	return false
}

type apiRelease struct {
	TagName    string     `json:"tag_name"`
	Draft      bool       `json:"draft"`
	Prerelease bool       `json:"prerelease"`
	Assets     []apiAsset `json:"assets"`
}

type apiAsset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
	Size               int64  `json:"size"`
}

// GetRelease fetches the latest non-draft, non-prerelease GitHub Release when
// ref.Tag is empty, or the release for ref.Tag otherwise.
func (c *Client) GetRelease(ctx context.Context, ref domain.GitHubRef) (Release, error) {
	if _, err := domain.ParseGitHubRef(domain.GitHubRef{Owner: ref.Owner, Repo: ref.Repo}.String()); err != nil {
		return Release{}, err
	}
	client := c.apiClient()

	var endpoint string
	if ref.Tag == "" {
		endpoint = fmt.Sprintf("%s/repos/%s/%s/releases/latest", c.apiBase(), url.PathEscape(ref.Owner), url.PathEscape(ref.Repo))
	} else {
		endpoint = fmt.Sprintf("%s/repos/%s/%s/releases/tags/%s", c.apiBase(), url.PathEscape(ref.Owner), url.PathEscape(ref.Repo), url.PathEscape(ref.Tag))
	}

	if err := c.validateURL(ctx, endpoint, allowedAPIHosts); err != nil {
		return Release{}, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return Release{}, err
	}
	req.Header.Set("Accept", githubAccept)
	req.Header.Set("X-GitHub-Api-Version", githubAPIVersion)
	req.Header.Set("User-Agent", userAgent)

	resp, err := client.Do(req)
	if err != nil {
		return Release{}, fmt.Errorf("fetch github release %s: %w", ref, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return Release{}, classifyAPIError(resp, ref)
	}
	if resp.ContentLength > MaxReleaseJSONBytes {
		return Release{}, fmt.Errorf("%w: github release JSON Content-Length %d exceeds %d bytes", ErrResponseTooLarge, resp.ContentLength, MaxReleaseJSONBytes)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, MaxReleaseJSONBytes+1))
	if err != nil {
		return Release{}, fmt.Errorf("read github release %s: %w", ref, err)
	}
	if int64(len(body)) > MaxReleaseJSONBytes {
		return Release{}, fmt.Errorf("%w: github release JSON exceeds %d bytes", ErrResponseTooLarge, MaxReleaseJSONBytes)
	}

	var raw apiRelease
	if err := json.Unmarshal(body, &raw); err != nil {
		return Release{}, fmt.Errorf("decode github release %s: %w", ref, err)
	}
	if raw.Draft {
		return Release{}, fmt.Errorf("%w: %s (%q)", ErrDraftRelease, ref, raw.TagName)
	}
	if raw.Prerelease {
		return Release{}, fmt.Errorf("%w: %s (%q)", ErrPrerelease, ref, raw.TagName)
	}
	if raw.TagName == "" {
		return Release{}, fmt.Errorf("decode github release %s: missing tag_name", ref)
	}

	assets := make([]Asset, len(raw.Assets))
	for i, a := range raw.Assets {
		assets[i] = Asset{
			Name:        a.Name,
			DownloadURL: a.BrowserDownloadURL,
			Size:        a.Size,
		}
	}
	return Release{
		Tag:        raw.TagName,
		Prerelease: raw.Prerelease,
		Draft:      raw.Draft,
		Assets:     assets,
	}, nil
}

func classifyAPIError(resp *http.Response, ref domain.GitHubRef) error {
	if (resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusTooManyRequests) &&
		strings.TrimSpace(resp.Header.Get("X-RateLimit-Remaining")) == "0" {
		if reset := strings.TrimSpace(resp.Header.Get("X-RateLimit-Reset")); reset != "" {
			return fmt.Errorf("%w: unauthenticated GitHub API limit reached for %s (resets at unix %s); retry later", ErrRateLimited, ref, reset)
		}
		return fmt.Errorf("%w: unauthenticated GitHub API limit reached for %s; retry later", ErrRateLimited, ref)
	}
	if resp.StatusCode == http.StatusNotFound {
		if ref.Tag == "" {
			return fmt.Errorf("%w: latest release for %s not found on GitHub", domain.ErrNotFound, ref)
		}
		return fmt.Errorf("%w: release tag %q for %s/%s not found on GitHub", domain.ErrNotFound, ref.Tag, ref.Owner, ref.Repo)
	}
	return fmt.Errorf("github API returned %s for %s", resp.Status, ref)
}

// DownloadAsset downloads asset into dst over HTTPS from an allowed GitHub host,
// enforcing maxBytes both via Content-Length pre-check and a hard stream limit.
func (c *Client) DownloadAsset(ctx context.Context, asset Asset, maxBytes int64, dst io.Writer) (int64, error) {
	if dst == nil {
		return 0, errors.New("download destination writer must not be nil")
	}
	if maxBytes <= 0 {
		return 0, fmt.Errorf("maxBytes must be positive, got %d", maxBytes)
	}
	client := c.assetClient()
	if err := c.validateURL(ctx, asset.DownloadURL, allowedAssetHosts); err != nil {
		return 0, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, asset.DownloadURL, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("User-Agent", userAgent)

	resp, err := client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("download asset %q: %w", asset.Name, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("download asset %q: unexpected HTTP status %s", asset.Name, resp.Status)
	}
	if resp.ContentLength > maxBytes {
		return 0, fmt.Errorf("%w: asset %q Content-Length %d exceeds limit %d", ErrResponseTooLarge, asset.Name, resp.ContentLength, maxBytes)
	}

	n, err := io.Copy(dst, io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return n, fmt.Errorf("download asset %q: %w", asset.Name, err)
	}
	if n > maxBytes {
		return n, fmt.Errorf("%w: asset %q exceeds limit of %d bytes", ErrResponseTooLarge, asset.Name, maxBytes)
	}
	return n, nil
}
