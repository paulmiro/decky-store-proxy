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
	manifestAsset   = "decky-store.json"
	manifestSchema  = 2
	maxManifest     = 64 << 10
	fetchWorkers    = 8
	releasesPerPage = 100
	maxReleasePages = 50
)

// Manifest is the decky-store.json asset of a release. It lists every Decky
// plugin built in that release.
type Manifest struct {
	Schema  int      `json:"schema"`
	Plugins []Plugin `json:"plugins"`
}

// Plugin is one plugin version in a manifest.
type Plugin struct {
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
	if m.Schema != manifestSchema {
		return fmt.Errorf("unsupported schema %d", m.Schema)
	}
	if len(m.Plugins) == 0 {
		return errors.New("no plugins")
	}
	names := map[string]bool{}
	for i := range m.Plugins {
		p := &m.Plugins[i]
		if err := p.validate(); err != nil {
			return fmt.Errorf("plugin %d: %w", i, err)
		}
		if names[p.Name] {
			return fmt.Errorf("plugin name %q listed twice", p.Name)
		}
		names[p.Name] = true
	}
	return nil
}

func (p *Plugin) validate() error {
	switch {
	case strings.TrimSpace(p.Name) == "" || strings.ContainsAny(p.Name, "/\\"):
		return fmt.Errorf("invalid name %q", p.Name)
	case !semverRe.MatchString(p.Version):
		return fmt.Errorf("version %q is not semver", p.Version)
	case !sha256Re.MatchString(p.SHA256):
		return fmt.Errorf("sha256 %q is not a lowercase hex sha256", p.SHA256)
	case !strings.HasPrefix(p.Artifact, "https://github.com/") || !strings.Contains(p.Artifact, "/releases/download/"):
		return fmt.Errorf("artifact %q is not a GitHub release download URL", p.Artifact)
	case p.ImageURL != "" && !strings.HasPrefix(p.ImageURL, "https://"):
		return fmt.Errorf("image_url %q is not https", p.ImageURL)
	}
	return nil
}

// Entry is one discovered custom plugin.
type Entry struct {
	Repo   string `json:"repo"`
	Source string `json:"source"` // "github:org:<name>", "github:user:<name>" or "github:repo"
	Latest Plugin `json:"latest"` // from the repo's latest release

	// History holds every known stable version, newest first, including
	// Latest. Decky's store card needs the installed version in this list, or
	// it labels the update as a "downgrade".
	History []Plugin `json:"history"`
	// HistoryComplete is false if the release list couldn't be fetched; it is
	// retried on the next refresh.
	HistoryComplete bool `json:"historyComplete"`
}

// versions returns all known versions, newest first.
func (e *Entry) versions() []Plugin {
	if len(e.History) == 0 {
		return []Plugin{e.Latest}
	}
	return e.History
}

// id returns a stable negative id so it can never collide with upstream ids.
func (e *Entry) id() int64 {
	h := fnv.New32a()
	h.Write([]byte(strings.ToLower(e.Repo) + "\x00" + e.Latest.Name))
	return -int64(h.Sum32()&0x7fffffff) - 1
}

// repoState is everything known about one repo. Entries are derived from it.
type repoState struct {
	Repo   string   `json:"repo"`
	Source string   `json:"source"`
	Latest []Plugin `json:"latest"` // manifest of the latest release

	// Releases are the stable releases with a decky-store.json, newest first.
	Releases []releaseRecord `json:"releases,omitempty"`
	// Complete: Releases reached the end of the release list once, so later
	// fetches only need the newest page(s).
	Complete bool `json:"complete,omitempty"`
	// Stale: the last fetch of the release list failed; retried next refresh.
	Stale bool `json:"stale,omitempty"`
}

// releaseRecord caches one release's manifest. A manifest is downloaded once
// per asset id; a re-uploaded asset gets a new id and is downloaded again.
type releaseRecord struct {
	ID      int64    `json:"id"`
	Tag     string   `json:"tag"`
	Asset   int64    `json:"asset"`
	Plugins []Plugin `json:"plugins,omitempty"`
	Invalid string   `json:"invalid,omitempty"`
}

// entries derives one Entry per plugin of the latest release. Older versions
// come from older releases' manifests, matched by plugin name. If several
// releases contain the same version, the newest release's copy is used, with
// the creation time of the oldest.
func (s *repoState) entries() []Entry {
	var out []Entry
	for _, latest := range s.Latest {
		byVersion := map[string]int{latest.Version: 0}
		hist := []Plugin{latest}
		for _, rec := range s.Releases {
			for _, p := range rec.Plugins {
				if p.Name != latest.Name || compareSemver(p.Version, latest.Version) > 0 {
					continue
				}
				i, ok := byVersion[p.Version]
				if !ok {
					byVersion[p.Version] = len(hist)
					hist = append(hist, p)
					continue
				}
				if !p.Created.IsZero() && (hist[i].Created.IsZero() || p.Created.Before(hist[i].Created)) {
					hist[i].Created = p.Created
				}
			}
		}
		slices.SortStableFunc(hist, func(a, b Plugin) int { return compareSemver(b.Version, a.Version) })
		out = append(out, Entry{
			Repo:            s.Repo,
			Source:          s.Source,
			Latest:          hist[slices.IndexFunc(hist, func(p Plugin) bool { return p.Version == latest.Version })],
			History:         hist,
			HistoryComplete: s.Complete && !s.Stale,
		})
	}
	return out
}

// cacheVersion is bumped whenever the cache format changes; other versions
// are ignored.
const cacheVersion = 2

// Catalog is the result of one refresh. It is immutable once published. Only
// Repos is saved to the cache; Plugins is derived from it.
type Catalog struct {
	Version int                   `json:"version"`
	Updated time.Time             `json:"updated"`
	Errors  []string              `json:"errors,omitempty"`
	Repos   map[string]*repoState `json:"repos"` // key: lowercase owner/repo
	Plugins []Entry               `json:"-"`

	byName map[string]*Entry
}

// newCatalog derives the plugin list from repos.
func newCatalog(updated time.Time, repos map[string]*repoState, errs []string) (*Catalog, []string) {
	var found []Entry
	for _, st := range repos {
		found = append(found, st.entries()...)
	}
	plugins, conflictErrs := resolveConflicts(found)
	c := &Catalog{Version: cacheVersion, Updated: updated, Errors: append(errs, conflictErrs...), Repos: repos, Plugins: plugins}
	c.index()
	return c, conflictErrs
}

func (c *Catalog) index() {
	c.byName = make(map[string]*Entry, len(c.Plugins))
	for i := range c.Plugins {
		c.byName[c.Plugins[i].Latest.Name] = &c.Plugins[i]
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
	d.current, _ = newCatalog(time.Time{}, map[string]*repoState{}, nil)
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
	var saved Catalog
	if err := json.Unmarshal(b, &saved); err != nil {
		slog.Warn("ignoring corrupt cache", "path", p, "err", err)
		return
	}
	if saved.Version != cacheVersion {
		slog.Info("ignoring cache from another version", "path", p)
		return
	}
	// Drop cached repos whose source is no longer configured.
	repos := map[string]*repoState{}
	for k, st := range saved.Repos {
		if st != nil && d.sourceConfigured(st.Repo, st.Source) {
			repos[k] = st
		}
	}
	c, _ := newCatalog(saved.Updated, repos, saved.Errors)
	d.mu.Lock()
	d.current = c
	d.mu.Unlock()
	slog.Info("loaded cached catalog", "plugins", len(c.Plugins), "updated", c.Updated)
}

// Source labels, also shown by /_proxy/status.
const (
	sourceGitHubRepo = "github:repo"
	sourceGitHubOrg  = "github:org:"
	sourceGitHubUser = "github:user:"
)

func (d *Discoverer) sourceConfigured(repo, source string) bool {
	gh := &d.cfg.Sources.GitHub
	in := func(list []string, s string) bool {
		return slices.ContainsFunc(list, func(x string) bool { return strings.EqualFold(x, s) })
	}
	if source == sourceGitHubRepo {
		return in(gh.Repos, repo)
	}
	if org, ok := strings.CutPrefix(source, sourceGitHubOrg); ok {
		return in(gh.Orgs, org)
	}
	if user, ok := strings.CutPrefix(source, sourceGitHubUser); ok {
		return in(gh.Users, user)
	}
	return false
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
	gh := &d.cfg.Sources.GitHub
	// Explicit repos first: on a duplicate repo, the explicit entry wins.
	for _, r := range gh.Repos {
		add(repoRef{r, sourceGitHubRepo})
	}
	type owner struct{ kind, name, source string }
	var owners []owner
	for _, o := range gh.Orgs {
		owners = append(owners, owner{"org", o, sourceGitHubOrg + o})
	}
	for _, u := range gh.Users {
		owners = append(owners, owner{"user", u, sourceGitHubUser + u})
	}
	for _, o := range owners {
		names, err := d.listOwnerRepos(ctx, o.kind, o.name)
		if err != nil {
			msg := fmt.Sprintf("github %s %s: %v (keeping previous plugins)", o.kind, o.name, err)
			slog.Warn(msg)
			errs = append(errs, msg)
			for _, st := range prev.Repos {
				if st.Source == o.source {
					add(repoRef{st.Repo, st.Source})
				}
			}
			continue
		}
		for _, n := range names {
			add(repoRef{n, o.source})
		}
	}

	type result struct {
		state *repoState
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
			results[i].state, results[i].err = d.refreshRepo(ctx, r, prev.Repos[strings.ToLower(r.name)])
		}()
	}
	wg.Wait()

	states := map[string]*repoState{}
	for _, r := range results {
		if r.err != "" {
			slog.Warn(r.err)
			errs = append(errs, r.err)
		}
		if r.state != nil {
			states[strings.ToLower(r.state.Repo)] = r.state
		}
	}
	c, conflictErrs := newCatalog(time.Now().UTC(), states, errs)
	for _, e := range conflictErrs {
		slog.Warn(e)
	}
	d.mu.Lock()
	d.current = c
	d.mu.Unlock()
	d.saveCache(c)
	slog.Info("refreshed catalog", "plugins", len(c.Plugins), "errors", len(c.Errors))
	return c
}

// resolveConflicts enforces unique plugin names. Explicit repos beat org and
// user repos; two candidates of the same tier are ambiguous and both dropped.
func resolveConflicts(found []Entry) ([]Entry, []string) {
	tier := func(e Entry) int {
		if e.Source == sourceGitHubRepo {
			return 0
		}
		return 1
	}
	groups := map[string][]Entry{}
	for _, e := range found {
		groups[e.Latest.Name] = append(groups[e.Latest.Name], e)
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
	slices.SortFunc(out, func(a, b Entry) int { return strings.Compare(a.Latest.Name, b.Latest.Name) })
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

// listOwnerRepos lists the public, non-archived repos of an org or user
// (kind "org" or "user") that carry at least one configured topic. For users
// only repos they own are listed, not ones they merely contribute to.
func (d *Discoverer) listOwnerRepos(ctx context.Context, kind, owner string) ([]string, error) {
	path := fmt.Sprintf("orgs/%s/repos?type=public", owner)
	if kind == "user" {
		path = fmt.Sprintf("users/%s/repos?type=owner", owner)
	}
	var names []string
	for page := 1; page <= 50; page++ {
		url := fmt.Sprintf("%s/%s&per_page=100&page=%d", d.cfg.githubAPI, path, page)
		body, err := d.getAPI(ctx, url)
		if err != nil {
			return nil, err
		}
		var repos []ghRepo
		if err := json.Unmarshal(body, &repos); err != nil {
			return nil, fmt.Errorf("decoding repo list: %w", err)
		}
		for _, r := range repos {
			if r.Archived || r.Disabled || r.Private || !d.cfg.Sources.GitHub.hasTopic(r.Topics) {
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
			return nil, fmt.Errorf("GitHub API rate limit exceeded (resets at unix %s); consider sources.github.tokenFile", resp.Header.Get("X-RateLimit-Reset"))
		}
		fallthrough
	default:
		return nil, fmt.Errorf("GET %s: HTTP %d", url, resp.StatusCode)
	}
}

func (d *Discoverer) fetchManifest(ctx context.Context, repo string) (*Manifest, error) {
	return d.fetchManifestURL(ctx, fmt.Sprintf("%s/%s/releases/latest/download/%s", d.cfg.githubWeb, repo, manifestAsset))
}

// refreshRepo checks one repo's latest release. prev is its state from the
// last refresh, or nil. A nil state means the repo offers no plugins.
func (d *Discoverer) refreshRepo(ctx context.Context, r repoRef, prev *repoState) (*repoState, string) {
	m, err := d.fetchManifest(ctx, r.name)
	switch {
	case errors.Is(err, errNoRelease):
		return nil, fmt.Sprintf("%s: latest release has no %s", r.name, manifestAsset)
	case errors.Is(err, errInvalid):
		return nil, fmt.Sprintf("%s: latest release: %v", r.name, err)
	case err != nil && prev != nil:
		st := *prev
		st.Source = r.source
		return &st, fmt.Sprintf("%s: %v (keeping previous versions)", r.name, err)
	case err != nil:
		return nil, fmt.Sprintf("%s: %v", r.name, err)
	}

	st := &repoState{Repo: r.name, Source: r.source, Latest: m.Plugins}
	if prev != nil {
		st.Releases, st.Complete, st.Stale = prev.Releases, prev.Complete, prev.Stale
	}
	if !needsHistory(prev, m.Plugins) {
		return st, ""
	}
	if err := d.updateReleases(ctx, st); err != nil {
		st.Stale = true
		return st, fmt.Sprintf("%s: release history: %v (will retry)", r.name, err)
	}
	return st, ""
}

// needsHistory reports whether the release list must be fetched: on the
// first sight of a repo, after a failed fetch, and when the latest release
// has a plugin version that wasn't there before. A latest release that only
// rebuilt the same versions (a new hash) needs no API request.
func needsHistory(prev *repoState, latest []Plugin) bool {
	if prev == nil || !prev.Complete || prev.Stale {
		return true
	}
	for _, p := range latest {
		if !slices.ContainsFunc(prev.Latest, func(q Plugin) bool { return q.Name == p.Name && q.Version == p.Version }) {
			return true
		}
	}
	return false
}

type ghRelease struct {
	ID         int64     `json:"id"`
	TagName    string    `json:"tag_name"`
	Draft      bool      `json:"draft"`
	Prerelease bool      `json:"prerelease"`
	Assets     []ghAsset `json:"assets"`
}

type ghAsset struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
	URL  string `json:"browser_download_url"`
}

// updateReleases refreshes st.Releases from the release list, newest first.
// If st.Releases already covers the whole list, it stops after the first page
// that contains a known release and keeps the older records. Otherwise it
// reads every page. Manifests are only downloaded for releases not seen yet.
// On error, st is left unchanged.
func (d *Discoverer) updateReleases(ctx context.Context, st *repoState) error {
	known := map[int64]int{}
	for i, rec := range st.Releases {
		known[rec.ID] = i
	}
	var fresh []releaseRecord
	seen := map[int64]bool{}
	oldestKnown := -1
	incremental := false
	for page := 1; page <= maxReleasePages; page++ {
		body, err := d.getAPI(ctx, fmt.Sprintf("%s/repos/%s/releases?per_page=%d&page=%d", d.cfg.githubAPI, st.Repo, releasesPerPage, page))
		if err != nil {
			return err
		}
		var rels []ghRelease
		if err := json.Unmarshal(body, &rels); err != nil {
			return fmt.Errorf("decoding release list: %w", err)
		}
		hitKnown := false
		for _, rel := range rels {
			if rel.Draft || rel.Prerelease || seen[rel.ID] {
				continue
			}
			seen[rel.ID] = true
			i, isKnown := known[rel.ID]
			if isKnown {
				hitKnown = true
				oldestKnown = max(oldestKnown, i)
			}
			ai := slices.IndexFunc(rel.Assets, func(a ghAsset) bool { return a.Name == manifestAsset })
			if ai < 0 {
				continue
			}
			asset := rel.Assets[ai]
			if isKnown && st.Releases[i].Asset == asset.ID {
				rec := st.Releases[i]
				rec.Tag = rel.TagName
				fresh = append(fresh, rec)
				continue
			}
			rec := releaseRecord{ID: rel.ID, Tag: rel.TagName, Asset: asset.ID}
			m, err := d.fetchManifestURL(ctx, asset.URL)
			switch {
			case errors.Is(err, errInvalid) || errors.Is(err, errNoRelease):
				slog.Warn("ignoring release", "repo", st.Repo, "tag", rel.TagName, "err", err)
				rec.Invalid = err.Error()
			case err != nil:
				return err
			default:
				rec.Plugins = m.Plugins
			}
			fresh = append(fresh, rec)
		}
		if len(rels) < releasesPerPage {
			break
		}
		if hitKnown && st.Complete {
			incremental = true
			break
		}
	}
	if incremental {
		// Everything after the oldest release seen on these pages is older
		// and was already processed. Known releases newer than that which
		// are no longer listed were deleted and are dropped.
		for _, rec := range st.Releases[oldestKnown+1:] {
			if !seen[rec.ID] {
				fresh = append(fresh, rec)
			}
		}
	}
	st.Releases, st.Complete, st.Stale = fresh, true, false
	return nil
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
