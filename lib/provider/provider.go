// Package provider describes the terraform provider being profiled: where its releases and source live, where tfpp
// caches downloads and results, and which releases are selected for profiling.
package provider

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/katbyte/tf-provider-profile/lib/clog"
)

// Provider identifies a terraform provider and where tfpp keeps its data.
type Provider struct {
	Name     string // short name, e.g. azurerm
	Repo     string // github owner/name, e.g. hashicorp/terraform-provider-azurerm
	Source   string // registry source address, e.g. hashicorp/azurerm
	CacheDir string // root cache dir, e.g. .cache/azurerm

	// DataDir holds the per-release result files, the one thing worth committing (default data/<name>)
	DataDir string
}

// New builds a Provider from its short name, defaulting the repo and source to the hashicorp conventions.
func New(name, repo, cacheRoot, dataRoot string) (*Provider, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, errors.New("provider name is required")
	}
	if repo == "" {
		repo = "hashicorp/terraform-provider-" + name
	}
	owner, _, _ := strings.Cut(repo, "/")

	abs, err := filepath.Abs(filepath.Join(cacheRoot, name))
	if err != nil {
		return nil, fmt.Errorf("resolving cache dir: %w", err)
	}
	data, err := filepath.Abs(filepath.Join(dataRoot, name))
	if err != nil {
		return nil, fmt.Errorf("resolving data dir: %w", err)
	}

	return &Provider{
		Name:     name,
		Repo:     repo,
		Source:   owner + "/" + name,
		CacheDir: abs,
		DataDir:  data,
	}, nil
}

// BinaryName is the executable name prefix used inside release zips, e.g. terraform-provider-azurerm.
func (p *Provider) BinaryName() string { return "terraform-provider-" + p.Name }

// Cache layout helpers - every path tfpp writes lives under CacheDir.

func (p *Provider) BinariesDir() string  { return filepath.Join(p.CacheDir, "binaries") }
func (p *Provider) SrcDir() string       { return filepath.Join(p.CacheDir, "src") }
func (p *Provider) ResultsDir() string   { return p.DataDir }
func (p *Provider) SchemaDir() string    { return filepath.Join(p.CacheDir, "schema") }
func (p *Provider) GoCacheDir() string   { return filepath.Join(p.CacheDir, "gocache") }
func (p *Provider) BuildOutDir() string  { return filepath.Join(p.CacheDir, "build") }
func (p *Provider) ReleasesFile() string { return filepath.Join(p.CacheDir, "releases.json") }

// ReleaseDir is where a release's zips and extracted binaries live: binaries/<version>/<platform>/...
func (p *Provider) ReleaseDir(version string) string {
	return filepath.Join(p.BinariesDir(), version)
}

// ZipName is the release asset name for a platform, e.g. terraform-provider-azurerm_5.4.0_linux_amd64.zip
func (p *Provider) ZipName(version, platform string) string {
	return fmt.Sprintf("%s_%s_%s.zip", p.BinaryName(), strings.TrimPrefix(version, "v"), platform)
}

// ZipURL is the releases.hashicorp.com download url for a release asset.
func (p *Provider) ZipURL(version, platform string) string {
	v := strings.TrimPrefix(version, "v")
	return fmt.Sprintf("https://releases.hashicorp.com/%s/%s/%s", p.BinaryName(), v, p.ZipName(version, platform))
}

// NativePlatform is the GOOS_GOARCH tfpp itself runs on; timing stages only run against this platform's binary.
func NativePlatform() string { return runtime.GOOS + "_" + runtime.GOARCH }

// Release is a published provider release.
type Release struct {
	Version string    `json:"version"` // tag, e.g. v5.4.0
	Date    time.Time `json:"date"`
}

// ParseVersion splits a vX.Y.Z tag into ints for sorting. Non-numeric parts sort as 0.
func ParseVersion(v string) [3]int {
	var out [3]int
	rest := strings.TrimPrefix(v, "v")
	for i := range out {
		part, tail, _ := strings.Cut(rest, ".")
		rest = tail
		// strip any pre-release suffix (e.g. 1.2.3-rc1)
		if idx := strings.IndexAny(part, "-+"); idx >= 0 {
			part = part[:idx]
		}
		out[i], _ = strconv.Atoi(part)
	}
	return out
}

// CompareVersion orders versions numerically (v4.10.0 after v4.9.0); a pre-release sorts before its final version.
func CompareVersion(a, b string) int {
	va, vb := ParseVersion(a), ParseVersion(b)
	for i := range 3 {
		if va[i] != vb[i] {
			return cmp.Compare(va[i], vb[i])
		}
	}
	preA, preB := strings.ContainsAny(a, "-+"), strings.ContainsAny(b, "-+")
	if preA != preB {
		if preA {
			return -1
		}
		return 1
	}
	return cmp.Compare(a, b)
}

// SortReleases orders releases oldest first.
func SortReleases(rs []Release) {
	slices.SortFunc(rs, func(a, b Release) int { return CompareVersion(a.Version, b.Version) })
}

type ghRelease struct {
	TagName     string    `json:"tag_name"`
	Draft       bool      `json:"draft"`
	Prerelease  bool      `json:"prerelease"`
	PublishedAt time.Time `json:"published_at"`
}

// FetchReleases lists published (non-draft, non-prerelease) releases from GitHub, newest first, stopping once
// releases older than `since` are reached. The result is cached to ReleasesFile; on network failure the cache is
// used instead.
func (p *Provider) FetchReleases(ctx context.Context, apiURL, token string, since time.Time) ([]Release, error) {
	rs, err := p.fetchReleasesFromGitHub(ctx, apiURL, token, since)
	if err != nil {
		cached, cerr := p.LoadCachedReleases()
		if cerr != nil {
			return nil, fmt.Errorf("fetching releases: %w (and no usable cache: %w)", err, cerr)
		}
		clog.Log.Warnf("fetching releases from github failed, using cached list: %v", err)
		return filterSince(cached, since), nil
	}

	// merge with cache so an earlier, wider fetch is never lost
	if cached, cerr := p.LoadCachedReleases(); cerr == nil {
		seen := map[string]bool{}
		for _, r := range rs {
			seen[r.Version] = true
		}
		for _, r := range cached {
			if !seen[r.Version] {
				rs = append(rs, r)
			}
		}
	}
	SortReleases(rs)

	if err := os.MkdirAll(p.CacheDir, 0o750); err != nil {
		return nil, fmt.Errorf("creating cache dir: %w", err)
	}
	b, _ := json.MarshalIndent(rs, "", "  ")
	if err := os.WriteFile(p.ReleasesFile(), b, 0o600); err != nil {
		return nil, fmt.Errorf("writing releases cache: %w", err)
	}

	return filterSince(rs, since), nil
}

// LoadCachedReleases reads the cached release list.
func (p *Provider) LoadCachedReleases() ([]Release, error) {
	b, err := os.ReadFile(p.ReleasesFile())
	if err != nil {
		return nil, err
	}
	var rs []Release
	if err := json.Unmarshal(b, &rs); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", p.ReleasesFile(), err)
	}
	SortReleases(rs)
	return rs, nil
}

func filterSince(rs []Release, since time.Time) []Release {
	out := make([]Release, 0, len(rs))
	for _, r := range rs {
		if !r.Date.Before(since) {
			out = append(out, r)
		}
	}
	return DropBackports(out)
}

// DropBackports removes releases published on an older major line after a newer major already shipped (e.g. a
// v3.117.1 patch cut months into v4), since they are not part of the trend and would zig-zag every chart.
// The result is sorted by version, which after the filter is also date order.
func DropBackports(rs []Release) []Release {
	byDate := slices.Clone(rs)
	slices.SortFunc(byDate, func(a, b Release) int { return a.Date.Compare(b.Date) })
	out := make([]Release, 0, len(rs))
	maxMajor := -1
	for _, r := range byDate {
		major := ParseVersion(r.Version)[0]
		if major < maxMajor {
			continue
		}
		maxMajor = major
		out = append(out, r)
	}
	SortReleases(out)
	return out
}

func (p *Provider) fetchReleasesFromGitHub(ctx context.Context, apiURL, token string, since time.Time) ([]Release, error) {
	if apiURL == "" {
		apiURL = "https://api.github.com"
	}
	client := &http.Client{Timeout: 60 * time.Second}

	var out []Release
	for page := 1; page <= 50; page++ {
		url := fmt.Sprintf("%s/repos/%s/releases?per_page=100&page=%d", strings.TrimSuffix(apiURL, "/"), p.Repo, page)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Accept", "application/vnd.github+json")
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}

		resp, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		body, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil {
			return nil, err
		}
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("github api %s: %s: %s", url, resp.Status, strings.TrimSpace(string(body)))
		}

		var ghs []ghRelease
		if err := json.Unmarshal(body, &ghs); err != nil {
			return nil, fmt.Errorf("parsing github releases: %w", err)
		}
		if len(ghs) == 0 {
			break
		}

		done := false
		for _, g := range ghs {
			if g.Draft || g.Prerelease {
				continue
			}
			if g.PublishedAt.Before(since) {
				done = true
				continue
			}
			out = append(out, Release{Version: g.TagName, Date: g.PublishedAt.UTC()})
		}
		if done || len(ghs) < 100 {
			break
		}
	}

	SortReleases(out)
	return out, nil
}

// Sample returns the subset of releases to run expensive stages on: every nth release counting from the oldest,
// always including the newest. n <= 1 returns every release.
func Sample(rs []Release, n int) []Release {
	if n <= 1 || len(rs) == 0 {
		return rs
	}
	out := make([]Release, 0, len(rs)/n+2)
	for i, r := range rs {
		if i%n == 0 || i == len(rs)-1 {
			out = append(out, r)
		}
	}
	return out
}
