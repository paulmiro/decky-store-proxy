package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	manifestAsset  = "decky-store.json"
	manifestSchema = 1
	maxManifest    = 64 << 10
	fetchWorkers   = 8
)

// Manifest is the decky-store.json asset attached to every plugin release.
type Manifest struct {
	Schema      int       `json:"schema"`
	Name        string    `json:"name"`
	Version     string    `json:"version"`
	Artifact    string    `json:"artifact"`
	SHA256      string    `json:"sha256"`
	Author      string    `json:"author"`
	Description string    `json:"description"`
	Tags        []string  `json:"tags"`
	ImageURL    string    `json:"image_url"`
	Created     time.Time `json:"created"`
}

var (
	// Strict SemVer 2.0.0, no leading "v" (Decky compares against package.json).
	semverRe = regexp.MustCompile(`^(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)(?:-((?:0|[1-9]\d*|\d*[a-zA-Z-][0-9a-zA-Z-]*)(?:\.(?:0|[1-9]\d*|\d*[a-zA-Z-][0-9a-zA-Z-]*))*))?(?:\+([0-9a-zA-Z-]+(?:\.[0-9a-zA-Z-]+)*))?$`)
	sha256Re = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

func (m *Manifest) validate() error {
	switch {
	case m.Schema != manifestSchema:
		return fmt.Errorf("unsupported schema %d", m.Schema)
	case strings.TrimSpace(m.Name) == "" || strings.ContainsAny(m.Name, "/\\"):
		return fmt.Errorf("invalid name %q", m.Name)
	case !semverRe.MatchString(m.Version):
		return fmt.Errorf("version %q is not semver", m.Version)
	case !sha256Re.MatchString(m.SHA256):
		return fmt.Errorf("sha256 %q is not a lowercase hex sha256", m.SHA256)
	case !strings.HasPrefix(m.Artifact, "https://github.com/") || !strings.Contains(m.Artifact, "/releases/download/"):
		return fmt.Errorf("artifact %q is not a GitHub release download URL", m.Artifact)
	case m.ImageURL != "" && !strings.HasPrefix(m.ImageURL, "https://"):
		return fmt.Errorf("image_url %q is not https", m.ImageURL)
	}
	return nil
}

// Entry is one discovered custom plugin.
type Entry struct {
	Repo     string   `json:"repo"`
	Source   string   `json:"source"`   // "org:<name>" or "extra"
	Manifest Manifest `json:"manifest"` // latest release

	// History holds all stable releases, newest first, including Manifest.
	// Decky's store card needs the installed version in this list, or it
	// labels the update as a "downgrade".
	History []Manifest `json:"history,omitempty"`
	// HistoryComplete is false if the release list couldn't be fetched; it is
	// retried on the next refresh.
	HistoryComplete bool `json:"historyComplete,omitempty"`
}

// versions returns all known versions, newest first.
func (e *Entry) versions() []Manifest {
	if len(e.History) == 0 {
		return []Manifest{e.Manifest}
	}
	return e.History
}

// id returns a stable negative id so it can never collide with upstream ids.
func (e *Entry) id() int64 {
	h := fnv.New32a()
	h.Write([]byte(strings.ToLower(e.Repo)))
	return -int64(h.Sum32()&0x7fffffff) - 1
}

// Catalog is the result of one refresh. It is immutable once published.
type Catalog struct {
	Updated time.Time `json:"updated"`
	Plugins []Entry   `json:"plugins"`
	Errors  []string  `json:"errors,omitempty"`

	byName map[string]*Entry
}

func (c *Catalog) index() {
	c.byName = make(map[string]*Entry, len(c.Plugins))
	for i := range c.Plugins {
		c.byName[c.Plugins[i].Manifest.Name] = &c.Plugins[i]
	}
}

func (c *Catalog) lookup(name string) *Entry {
	if c == nil {
		return nil
	}
	return c.byName[name]
}

func (c *Catalog) empty() bool { return c == nil || len(c.Plugins) == 0 }

// Discoverer builds catalogs from GitHub.
type Discoverer struct {
	cfg    *Config
	client *http.Client

	mu      sync.Mutex
	current *Catalog
	etags   map[string]cachedResponse
	refresh sync.Mutex // serializes refreshes
}

type cachedResponse struct {
	etag string
	body []byte
}

func newDiscoverer(cfg *Config, client *http.Client) *Discoverer {
	d := &Discoverer{cfg: cfg, client: client, etags: map[string]cachedResponse{}}
	d.current = &Catalog{}
	d.current.index()
	return d
}

func (d *Discoverer) Catalog() *Catalog {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.current
}

func (d *Discoverer) cachePath() string {
	if d.cfg.CacheDir == "" {
		return ""
	}
	return filepath.Join(d.cfg.CacheDir, "catalog.json")
}

// loadCache restores the last good catalog so the store works while offline.
func (d *Discoverer) loadCache() {
	p := d.cachePath()
	if p == "" {
		return
	}
	b, err := os.ReadFile(p)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			slog.Warn("reading cache", "err", err)
		}
		return
	}
	var c Catalog
	if err := json.Unmarshal(b, &c); err != nil {
		slog.Warn("ignoring corrupt cache", "path", p, "err", err)
		return
	}
	// Drop cached plugins whose source is no longer configured.
	c.Plugins = slices.DeleteFunc(c.Plugins, func(e Entry) bool { return !d.sourceConfigured(e) })
	c.index()
	d.mu.Lock()
	d.current = &c
	d.mu.Unlock()
	slog.Info("loaded cached catalog", "plugins", len(c.Plugins), "updated", c.Updated)
}

func (d *Discoverer) sourceConfigured(e Entry) bool {
	if e.Source == "extra" {
		return slices.ContainsFunc(d.cfg.ExtraRepos, func(r string) bool { return strings.EqualFold(r, e.Repo) })
	}
	org, ok := strings.CutPrefix(e.Source, "org:")
	return ok && slices.ContainsFunc(d.cfg.Orgs, func(o string) bool { return strings.EqualFold(o, org) })
}

func (d *Discoverer) saveCache(c *Catalog) {
	p := d.cachePath()
	if p == "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		slog.Warn("creating cache dir", "err", err)
		return
	}
	b, _ := json.MarshalIndent(c, "", "  ")
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		slog.Warn("writing cache", "err", err)
		return
	}
	if err := os.Rename(tmp, p); err != nil {
		slog.Warn("writing cache", "err", err)
	}
}

type repoRef struct {
	name   string // owner/repo
	source string
}

// Refresh rebuilds the catalog. Transient failures keep the previous data for
// the affected org or repo, so a GitHub hiccup never empties the store.
func (d *Discoverer) Refresh(ctx context.Context) *Catalog {
	d.refresh.Lock()
	defer d.refresh.Unlock()

	prev := d.Catalog()
	prevByRepo := map[string]Entry{}
	for _, e := range prev.Plugins {
		prevByRepo[strings.ToLower(e.Repo)] = e
	}

	var errs []string
	var repos []repoRef
	seen := map[string]bool{}
	add := func(r repoRef) {
		k := strings.ToLower(r.name)
		if !seen[k] {
			seen[k] = true
			repos = append(repos, r)
		}
	}
	// Extras first: on a duplicate repo, the explicit config entry wins.
	for _, r := range d.cfg.ExtraRepos {
		add(repoRef{r, "extra"})
	}
	for _, org := range d.cfg.Orgs {
		names, err := d.listOrgRepos(ctx, org)
		if err != nil {
			msg := fmt.Sprintf("org %s: %v (keeping previous plugins)", org, err)
			slog.Warn(msg)
			errs = append(errs, msg)
			for _, e := range prev.Plugins {
				if e.Source == "org:"+org {
					add(repoRef{e.Repo, e.Source})
				}
			}
			continue
		}
		for _, n := range names {
			add(repoRef{n, "org:" + org})
		}
	}

	type result struct {
		entry *Entry
		err   string
	}
	results := make([]result, len(repos))
	sem := make(chan struct{}, fetchWorkers)
	var wg sync.WaitGroup
	for i, r := range repos {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			m, err := d.fetchManifest(ctx, r.name)
			switch {
			case err == nil:
				prev, hasPrev := prevByRepo[strings.ToLower(r.name)]
				e, histErr := d.withHistory(ctx, Entry{Repo: r.name, Source: r.source, Manifest: *m}, prev, hasPrev)
				results[i].entry = &e
				if histErr != nil {
					results[i].err = fmt.Sprintf("%s: release history: %v (will retry)", r.name, histErr)
				}
			case errors.Is(err, errNoRelease):
				results[i].err = fmt.Sprintf("%s: no release with %s", r.name, manifestAsset)
			case errors.Is(err, errInvalid):
				results[i].err = fmt.Sprintf("%s: %v", r.name, err)
			default:
				if p, ok := prevByRepo[strings.ToLower(r.name)]; ok {
					p.Source = r.source
					results[i].entry = &p
					results[i].err = fmt.Sprintf("%s: %v (keeping previous version %s)", r.name, err, p.Manifest.Version)
				} else {
					results[i].err = fmt.Sprintf("%s: %v", r.name, err)
				}
			}
		}()
	}
	wg.Wait()

	var found []Entry
	for _, r := range results {
		if r.err != "" {
			slog.Warn(r.err)
			errs = append(errs, r.err)
		}
		if r.entry != nil {
			found = append(found, *r.entry)
		}
	}

	plugins, conflictErrs := resolveConflicts(found)
	for _, e := range conflictErrs {
		slog.Warn(e)
	}
	errs = append(errs, conflictErrs...)

	c := &Catalog{Updated: time.Now().UTC(), Plugins: plugins, Errors: errs}
	c.index()
	d.mu.Lock()
	d.current = c
	d.mu.Unlock()
	d.saveCache(c)
	slog.Info("refreshed catalog", "plugins", len(plugins), "errors", len(errs))
	return c
}

// resolveConflicts enforces unique plugin names. Extra repos beat org repos;
// two candidates of the same tier are ambiguous and both are dropped.
func resolveConflicts(found []Entry) ([]Entry, []string) {
	tier := func(e Entry) int {
		if e.Source == "extra" {
			return 0
		}
		return 1
	}
	groups := map[string][]Entry{}
	for _, e := range found {
		groups[e.Manifest.Name] = append(groups[e.Manifest.Name], e)
	}
	var out []Entry
	var errs []string
	for name, g := range groups {
		best := slices.MinFunc(g, func(a, b Entry) int { return tier(a) - tier(b) })
		var top []string
		for _, e := range g {
			if tier(e) == tier(best) {
				top = append(top, e.Repo)
			}
		}
		if len(top) > 1 {
			slices.Sort(top)
			errs = append(errs, fmt.Sprintf("plugin name %q is claimed by %s; skipping all of them", name, strings.Join(top, ", ")))
			continue
		}
		out = append(out, best)
	}
	slices.SortFunc(out, func(a, b Entry) int { return strings.Compare(a.Manifest.Name, b.Manifest.Name) })
	slices.Sort(errs)
	return out, errs
}

var (
	errNoRelease = errors.New("no release")
	errInvalid   = errors.New("invalid manifest")
)

type ghRepo struct {
	FullName string   `json:"full_name"`
	Archived bool     `json:"archived"`
	Disabled bool     `json:"disabled"`
	Private  bool     `json:"private"`
	Topics   []string `json:"topics"`
}

func (d *Discoverer) listOrgRepos(ctx context.Context, org string) ([]string, error) {
	var names []string
	for page := 1; page <= 50; page++ {
		url := fmt.Sprintf("%s/orgs/%s/repos?type=public&per_page=100&page=%d", d.cfg.githubAPI, org, page)
		body, err := d.getAPI(ctx, url)
		if err != nil {
			return nil, err
		}
		var repos []ghRepo
		if err := json.Unmarshal(body, &repos); err != nil {
			return nil, fmt.Errorf("decoding repo list: %w", err)
		}
		for _, r := range repos {
			if r.Archived || r.Disabled || r.Private || !slices.Contains(r.Topics, d.cfg.Topic) {
				continue
			}
			names = append(names, r.FullName)
		}
		if len(repos) < 100 {
			break
		}
	}
	return names, nil
}

// getAPI does a GitHub API GET with ETag caching (304s don't count against
// the rate limit).
func (d *Discoverer) getAPI(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", userAgent)
	if d.cfg.githubToken != "" {
		req.Header.Set("Authorization", "Bearer "+d.cfg.githubToken)
	}
	d.mu.Lock()
	cached, haveCached := d.etags[url]
	d.mu.Unlock()
	if haveCached {
		req.Header.Set("If-None-Match", cached.etag)
	}
	resp, err := d.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotModified && haveCached:
		return cached.body, nil
	case resp.StatusCode == http.StatusOK:
		body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
		if err != nil {
			return nil, err
		}
		if et := resp.Header.Get("ETag"); et != "" {
			d.mu.Lock()
			d.etags[url] = cachedResponse{et, body}
			d.mu.Unlock()
		}
		return body, nil
	case resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusTooManyRequests:
		if resp.Header.Get("X-RateLimit-Remaining") == "0" {
			return nil, fmt.Errorf("GitHub API rate limit exceeded (resets at unix %s); consider githubTokenFile", resp.Header.Get("X-RateLimit-Reset"))
		}
		fallthrough
	default:
		return nil, fmt.Errorf("GET %s: HTTP %d", url, resp.StatusCode)
	}
}

func (d *Discoverer) fetchManifest(ctx context.Context, repo string) (*Manifest, error) {
	return d.fetchManifestURL(ctx, fmt.Sprintf("%s/%s/releases/latest/download/%s", d.cfg.githubWeb, repo, manifestAsset))
}

// withHistory fills e.History. The releases API is only called when the
// latest release changed (or the history is incomplete), so a steady state
// costs no API requests per repo.
func (d *Discoverer) withHistory(ctx context.Context, e Entry, prev Entry, hasPrev bool) (Entry, error) {
	var known []Manifest
	if hasPrev && prev.Manifest.Name == e.Manifest.Name {
		known = prev.versions()
		if prev.HistoryComplete && prev.Manifest.Version == e.Manifest.Version && prev.Manifest.SHA256 == e.Manifest.SHA256 {
			e.History, e.HistoryComplete = prev.History, true
			return e, nil
		}
	}
	hist, err := d.fetchHistory(ctx, e.Repo, e.Manifest, known)
	if err != nil {
		// Degrade gracefully: latest plus whatever older versions we knew.
		hist = []Manifest{e.Manifest}
		for _, k := range known {
			if compareSemver(k.Version, e.Manifest.Version) < 0 {
				hist = append(hist, k)
			}
		}
		e.History = hist
		return e, err
	}
	e.History, e.HistoryComplete = hist, true
	return e, nil
}

type ghRelease struct {
	TagName    string `json:"tag_name"`
	Draft      bool   `json:"draft"`
	Prerelease bool   `json:"prerelease"`
	Assets     []struct {
		Name string `json:"name"`
		URL  string `json:"browser_download_url"`
	} `json:"assets"`
}

// fetchHistory returns latest plus all older stable releases of the same
// plugin, newest first. Manifests in known are reused instead of downloaded.
func (d *Discoverer) fetchHistory(ctx context.Context, repo string, latest Manifest, known []Manifest) ([]Manifest, error) {
	body, err := d.getAPI(ctx, fmt.Sprintf("%s/repos/%s/releases?per_page=100", d.cfg.githubAPI, repo))
	if err != nil {
		return nil, err
	}
	var rels []ghRelease
	if err := json.Unmarshal(body, &rels); err != nil {
		return nil, fmt.Errorf("decoding release list: %w", err)
	}
	byVersion := map[string]Manifest{}
	for _, k := range known {
		byVersion[k.Version] = k
	}
	hist := []Manifest{latest}
	seen := map[string]bool{latest.Version: true}
	for _, rel := range rels {
		v := strings.TrimPrefix(rel.TagName, "v")
		if rel.Draft || rel.Prerelease || seen[v] || !semverRe.MatchString(v) || compareSemver(v, latest.Version) > 0 {
			continue
		}
		if k, ok := byVersion[v]; ok {
			hist = append(hist, k)
			seen[v] = true
			continue
		}
		var url string
		for _, a := range rel.Assets {
			if a.Name == manifestAsset {
				url = a.URL
			}
		}
		if url == "" {
			continue // released before using the workflow
		}
		m, err := d.fetchManifestURL(ctx, url)
		switch {
		case errors.Is(err, errInvalid) || errors.Is(err, errNoRelease):
			slog.Warn("skipping old release", "repo", repo, "tag", rel.TagName, "err", err)
			continue
		case err != nil:
			return nil, err
		case m.Version != v || m.Name != latest.Name:
			slog.Warn("skipping old release with mismatched manifest", "repo", repo, "tag", rel.TagName, "name", m.Name, "version", m.Version)
			continue
		}
		hist = append(hist, *m)
		seen[v] = true
	}
	slices.SortStableFunc(hist, func(a, b Manifest) int { return compareSemver(b.Version, a.Version) })
	return hist, nil
}

// compareSemver compares two SemVer strings (assumed valid) by precedence.
func compareSemver(a, b string) int {
	pa, pb := semverRe.FindStringSubmatch(a), semverRe.FindStringSubmatch(b)
	if pa == nil || pb == nil {
		return strings.Compare(a, b)
	}
	for i := 1; i <= 3; i++ {
		if c := compareNumeric(pa[i], pb[i]); c != 0 {
			return c
		}
	}
	switch preA, preB := pa[4], pb[4]; {
	case preA == preB:
		return 0
	case preA == "":
		return 1
	case preB == "":
		return -1
	default:
		fa, fb := strings.Split(preA, "."), strings.Split(preB, ".")
		for i := 0; i < len(fa) && i < len(fb); i++ {
			na, errA := strconv.Atoi(fa[i])
			nb, errB := strconv.Atoi(fb[i])
			var c int
			switch {
			case errA == nil && errB == nil:
				c = na - nb
			case errA == nil:
				c = -1
			case errB == nil:
				c = 1
			default:
				c = strings.Compare(fa[i], fb[i])
			}
			if c != 0 {
				return max(-1, min(1, c))
			}
		}
		return len(fa) - len(fb)
	}
}

func compareNumeric(a, b string) int {
	if len(a) != len(b) {
		return len(a) - len(b) // no leading zeros in SemVer
	}
	return strings.Compare(a, b)
}

func (d *Discoverer) fetchManifestURL(ctx context.Context, url string) (*Manifest, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := d.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, errNoRelease
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: HTTP %d", url, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxManifest+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxManifest {
		return nil, fmt.Errorf("%w: larger than %d bytes", errInvalid, maxManifest)
	}
	var m Manifest
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, fmt.Errorf("%w: %v", errInvalid, err)
	}
	if err := m.validate(); err != nil {
		return nil, fmt.Errorf("%w: %v", errInvalid, err)
	}
	return &m, nil
}
