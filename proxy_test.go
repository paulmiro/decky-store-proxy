package main

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// fakeUpstream imitates plugins.deckbrew.xyz (FastAPI with CORSMiddleware).
type fakeUpstream struct {
	*httptest.Server
	mu         sync.Mutex
	increments []string
	lastQuery  string
	down       atomic.Bool
}

const upstreamList = `[{"id":1,"name":"Alpha","author":"a","description":"d","tags":["x"],"versions":[{"name":"1.0.0","hash":"aa","created":"2024-01-01T00:00:00Z","downloads":5,"updates":0}],"visible":true,"image_url":"https://img/a","downloads":50,"updates":1,"created":"2024-01-01T00:00:00Z","updated":"2024-01-01T00:00:00Z"},{"id":2,"name":"Overridden","author":"b","description":"d","tags":[],"versions":[],"visible":true,"image_url":"","downloads":30,"updates":0,"created":"2024-06-01T00:00:00Z","updated":null},{"id":3,"name":"Zeta","author":"c","description":"d","tags":[],"versions":[],"visible":true,"image_url":"","downloads":10,"updates":0,"created":"2025-01-01T00:00:00Z","updated":null}]`

// sortedUpstream sorts like the real store (SQL ORDER BY); without sort_by the
// fixture's id order is returned byte-for-byte.
func sortedUpstream(by, dir string) []byte {
	if by == "" {
		return []byte(upstreamList)
	}
	var list []map[string]any
	json.Unmarshal([]byte(upstreamList), &list)
	field := map[string]string{"name": "name", "date": "created", "downloads": "downloads"}[by]
	sort.SliceStable(list, func(i, j int) bool {
		a, b := list[i][field], list[j][field]
		var less bool
		switch a := a.(type) {
		case string:
			less = a < b.(string)
		case float64:
			less = a < b.(float64)
		}
		if dir == "desc" {
			return !less && a != b
		}
		return less
	})
	b, _ := json.Marshal(list)
	return b
}

func newFakeUpstream(t *testing.T) *fakeUpstream {
	u := &fakeUpstream{}
	u.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if u.down.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Access-Control-Allow-Origin", "https://upstream-says-something-else")
		u.mu.Lock()
		u.lastQuery = r.URL.RawQuery
		u.mu.Unlock()
		if r.Header.Get("X-Decky-Version") == "v3.0.0" {
			w.WriteHeader(http.StatusUpgradeRequired)
			io.WriteString(w, "Unsupported loader version, please upgrade.")
			return
		}
		switch {
		case r.Method == "GET" && r.URL.Path == "/plugins":
			w.Header().Set("Content-Type", "application/json")
			w.Write(sortedUpstream(r.URL.Query().Get("sort_by"), r.URL.Query().Get("sort_direction")))
		case r.Method == "GET" && r.URL.Path == "/plugins/versions":
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"Alpha":["1.0.0","aa"],"Overridden":["0.1.0","bb"]}`)
		case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/increment"):
			u.mu.Lock()
			u.increments = append(u.increments, r.URL.EscapedPath()+"?"+r.URL.RawQuery)
			u.mu.Unlock()
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(u.Close)
	return u
}

// fakeGitHub serves both api.github.com and github.com release downloads.
type fakeGitHub struct {
	*httptest.Server
	mu        sync.Mutex
	repos     []ghRepo            // org listing
	userRepos map[string][]ghRepo // user -> repos
	manifests map[string]string   // repo -> body; missing = 404
	failing   map[string]bool     // repo -> 500
	releases  map[string][]fakeRelease
	apiCalls  int // org listing
	relCalls  int // release listing pages
	downloads int // old release manifest downloads
	apiDown   bool
}

type fakeRelease struct {
	tag               string
	prerelease, draft bool
	manifest          string // "" = no decky-store.json asset
}

func fakeID(parts ...string) int64 {
	h := fnv.New32a()
	h.Write([]byte(strings.Join(parts, "\x00")))
	return int64(h.Sum32())
}

func newFakeGitHub(t *testing.T) *fakeGitHub {
	g := &fakeGitHub{manifests: map[string]string{}, failing: map[string]bool{}, releases: map[string][]fakeRelease{}, userRepos: map[string][]ghRepo{}}
	g.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		defer g.mu.Unlock()
		if strings.HasPrefix(r.URL.Path, "/api/orgs/") {
			g.apiCalls++
			if g.apiDown {
				w.WriteHeader(http.StatusBadGateway)
				return
			}
			if r.Header.Get("If-None-Match") == `"v1"` {
				w.WriteHeader(http.StatusNotModified)
				return
			}
			w.Header().Set("ETag", `"v1"`)
			json.NewEncoder(w).Encode(g.repos)
			return
		}
		if rest, ok := strings.CutPrefix(r.URL.Path, "/api/users/"); ok {
			if r.URL.Query().Get("type") != "owner" {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			json.NewEncoder(w).Encode(g.userRepos[strings.TrimSuffix(rest, "/repos")])
			return
		}
		if rest, ok := strings.CutPrefix(r.URL.Path, "/api/repos/"); ok {
			repo := strings.TrimSuffix(rest, "/releases")
			g.relCalls++
			if g.apiDown {
				w.Header().Set("X-RateLimit-Remaining", "0")
				w.WriteHeader(http.StatusForbidden)
				return
			}
			var perPage, page int
			fmt.Sscan(r.URL.Query().Get("per_page"), &perPage)
			fmt.Sscan(r.URL.Query().Get("page"), &page)
			rels := g.releases[repo]
			lo, hi := min(len(rels), (page-1)*perPage), min(len(rels), page*perPage)
			out := []map[string]any{}
			for _, rel := range rels[lo:hi] {
				assets := []map[string]any{{"id": fakeID(repo, rel.tag, "zip"), "name": "plugin.zip", "browser_download_url": g.URL + "/zip"}}
				if rel.manifest != "" {
					assets = append(assets, map[string]any{"id": fakeID(repo, rel.tag, rel.manifest), "name": "decky-store.json", "browser_download_url": g.URL + "/assets/" + repo + "@" + rel.tag})
				}
				out = append(out, map[string]any{"id": fakeID(repo, rel.tag), "tag_name": rel.tag, "prerelease": rel.prerelease, "draft": rel.draft, "assets": assets})
			}
			json.NewEncoder(w).Encode(out)
			return
		}
		if rest, ok := strings.CutPrefix(r.URL.Path, "/assets/"); ok && strings.Contains(rest, "@") {
			repo, tag, _ := strings.Cut(rest, "@")
			g.downloads++
			for _, rel := range g.releases[repo] {
				if rel.tag == tag {
					io.WriteString(w, rel.manifest)
					return
				}
			}
			w.WriteHeader(http.StatusNotFound)
			return
		}
		// /<owner>/<repo>/releases/latest/download/decky-store.json -> redirect like GitHub
		if repo, ok := strings.CutSuffix(strings.TrimPrefix(r.URL.Path, "/"), "/releases/latest/download/decky-store.json"); ok {
			if g.failing[repo] {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			if _, ok := g.manifests[repo]; !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			http.Redirect(w, r, "/assets/"+repo, http.StatusFound)
			return
		}
		if repo, ok := strings.CutPrefix(r.URL.Path, "/assets/"); ok {
			io.WriteString(w, g.manifests[repo])
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(g.Close)
	return g
}

// plugin returns a valid manifest entry. created may be "" (zero time).
func plugin(name, version, repo, created string, tags ...string) Plugin {
	p := Plugin{
		Name: name, Version: version,
		Artifact: "https://github.com/" + repo + "/releases/download/v" + version + "/plugin.zip",
		SHA256:   strings.Repeat("ab", 32), Author: "me", Description: "desc " + name,
		Tags: tags, ImageURL: "https://example.com/i.png",
	}
	if created != "" {
		if err := p.Created.UnmarshalText([]byte(created)); err != nil {
			panic(err)
		}
	}
	return p
}

func manifestOf(plugins ...Plugin) string {
	b, _ := json.Marshal(Manifest{Schema: manifestSchema, Plugins: plugins})
	return string(b)
}

// manifest returns a decky-store.json with a single plugin.
func manifest(name, version, repo, created string, tags ...string) string {
	return manifestOf(plugin(name, version, repo, created, tags...))
}

type harness struct {
	up   *fakeUpstream
	gh   *fakeGitHub
	disc *Discoverer
	srv  *httptest.Server
	cfg  *Config
}

func newHarness(t *testing.T, mutate func(*Config)) *harness {
	up := newFakeUpstream(t)
	gh := newFakeGitHub(t)
	cfg := &Config{Upstream: up.URL + "/plugins", CacheDir: t.TempDir(), githubAPI: gh.URL + "/api", githubWeb: gh.URL}
	if mutate != nil {
		mutate(cfg)
	}
	t.Setenv("GITHUB_TOKEN", "")
	if err := cfg.finalize(); err != nil {
		t.Fatal(err)
	}
	client := &http.Client{}
	disc := newDiscoverer(cfg, client)
	s, err := newServer(cfg, disc, client)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)
	return &harness{up, gh, disc, srv, cfg}
}

func (h *harness) do(t *testing.T, method, path string, hdr map[string]string) (*http.Response, string) {
	t.Helper()
	req, _ := http.NewRequest(method, h.srv.URL+path, nil)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

var steam = map[string]string{"Origin": steamOrigin, "X-Decky-Version": "v3.3.1"}

func names(t *testing.T, body string) []string {
	t.Helper()
	var list []map[string]any
	if err := json.Unmarshal([]byte(body), &list); err != nil {
		t.Fatalf("not a list: %v: %s", err, body)
	}
	var out []string
	for _, p := range list {
		out = append(out, p["name"].(string))
	}
	return out
}

func TestPassThroughIsByteIdentical(t *testing.T) {
	h := newHarness(t, nil)

	resp, body := h.do(t, "GET", "/plugins?sort_by=name&sort_direction=asc", steam)
	if resp.StatusCode != 200 || body != string(sortedUpstream("name", "asc")) {
		t.Fatalf("got %d %q", resp.StatusCode, body)
	}
	if h.up.lastQuery != "sort_by=name&sort_direction=asc" {
		t.Errorf("query not forwarded: %q", h.up.lastQuery)
	}
	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != steamOrigin {
		t.Errorf("CORS origin = %q (upstream's own header must be replaced)", got)
	}
	if resp.Header.Get("Access-Control-Allow-Credentials") != "true" {
		t.Error("missing allow-credentials")
	}

	// Decky's backend increments without an Origin header.
	resp, _ = h.do(t, "POST", "/plugins/Alpha/versions/1.0.0/increment?isUpdate=False", nil)
	if resp.StatusCode != 200 || len(h.up.increments) != 1 || h.up.increments[0] != "/plugins/Alpha/versions/1.0.0/increment?isUpdate=False" {
		t.Fatalf("increment not forwarded: %d %v", resp.StatusCode, h.up.increments)
	}
	if resp.Header.Get("Access-Control-Allow-Origin") != "" {
		t.Error("CORS headers on a request without Origin")
	}

	// Old loader versions get upstream's 426 untouched.
	resp, body = h.do(t, "GET", "/plugins", map[string]string{"X-Decky-Version": "v3.0.0"})
	if resp.StatusCode != 426 || !strings.Contains(body, "upgrade") {
		t.Fatalf("got %d %q", resp.StatusCode, body)
	}
}

func TestPreflight(t *testing.T) {
	h := newHarness(t, nil)
	resp, _ := h.do(t, "OPTIONS", "/plugins?sort_by=name", map[string]string{
		"Origin": steamOrigin, "Access-Control-Request-Method": "GET",
		"Access-Control-Request-Headers": "x-decky-version",
	})
	if resp.StatusCode != 200 ||
		resp.Header.Get("Access-Control-Allow-Origin") != steamOrigin ||
		resp.Header.Get("Access-Control-Allow-Headers") != "x-decky-version" ||
		!strings.Contains(resp.Header.Get("Access-Control-Allow-Methods"), "GET") {
		t.Fatalf("bad preflight: %d %v", resp.StatusCode, resp.Header)
	}
	if resp.Header.Get("Access-Control-Allow-Private-Network") != "" {
		t.Error("PNA header sent without being requested")
	}
	resp, _ = h.do(t, "OPTIONS", "/plugins", map[string]string{
		"Origin": "https://evil.example", "Access-Control-Request-Method": "GET",
	})
	if resp.Header.Get("Access-Control-Allow-Origin") != "" {
		t.Error("foreign origin allowed")
	}
}

func setupOrg(h *harness) {
	h.gh.repos = []ghRepo{
		{FullName: "org/plugin-b", Topics: []string{"decky-plugin"}},
		{FullName: "org/override", Topics: []string{"decky-plugin"}},
		{FullName: "org/archived", Topics: []string{"decky-plugin"}, Archived: true},
		{FullName: "org/no-topic"},
		{FullName: "org/no-release", Topics: []string{"decky-plugin"}},
		{FullName: "org/broken", Topics: []string{"decky-plugin"}},
	}
	h.gh.manifests["org/plugin-b"] = manifest("Bravo", "1.2.0", "org/plugin-b", "2024-03-01T00:00:00Z", "test")
	h.gh.manifests["org/override"] = manifest("Overridden", "2.0.0", "org/override", "")
	h.gh.manifests["org/archived"] = manifest("Archived", "1.0.0", "org/archived", "")
	h.gh.manifests["org/no-topic"] = manifest("NoTopic", "1.0.0", "org/no-topic", "")
	h.gh.manifests["org/broken"] = `{"schema":2,"plugins":[{"name":"Broken","version":"v1.0.0"}]}`
}

func TestDiscoveryAndMerge(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.Sources.GitHub.Orgs = []string{"org"} })
	setupOrg(h)
	cat := h.disc.Refresh(context.Background())

	var got []string
	for _, e := range cat.Plugins {
		got = append(got, e.Latest.Name)
	}
	if fmt.Sprint(got) != "[Bravo Overridden]" {
		t.Fatalf("catalog = %v, errors = %v", got, cat.Errors)
	}
	if len(cat.Errors) != 2 { // no-release + broken
		t.Errorf("errors = %v", cat.Errors)
	}

	cases := map[string]string{
		"sort_by=name&sort_direction=asc":       "[Alpha Bravo Overridden Zeta]",
		"sort_by=name&sort_direction=desc":      "[Zeta Overridden Bravo Alpha]",
		"sort_by=date&sort_direction=desc":      "[Zeta Bravo Alpha Overridden]",
		"sort_by=downloads&sort_direction=desc": "[Alpha Zeta Bravo Overridden]",
		"":                                      "[Alpha Zeta Bravo Overridden]",
		"query=bra":                             "[Bravo]",
	}
	for q, want := range cases {
		_, body := h.do(t, "GET", "/plugins?"+q, steam)
		if q == "query=bra" {
			// upstream ignores filters in the fake; only check custom filtering
			if !strings.Contains(body, `"Bravo"`) || strings.Contains(body, `"name":"Overridden"`) {
				t.Errorf("%s: %s", q, body)
			}
			continue
		}
		if got := fmt.Sprint(names(t, body)); got != want {
			t.Errorf("%s: got %s want %s", q, got, want)
		}
	}

	// The custom "Overridden" replaces upstream's and carries its artifact.
	_, body := h.do(t, "GET", "/plugins?sort_by=name", steam)
	var list []map[string]any
	json.Unmarshal([]byte(body), &list)
	for _, p := range list {
		if p["name"] == "Overridden" {
			v := p["versions"].([]any)[0].(map[string]any)
			if v["name"] != "2.0.0" || !strings.HasPrefix(v["artifact"].(string), "https://github.com/org/override/") || p["id"].(float64) >= 0 {
				t.Errorf("bad override entry: %v", p)
			}
		}
	}

	resp, body := h.do(t, "GET", "/plugins/versions", steam)
	var versions map[string][]string
	json.Unmarshal([]byte(body), &versions)
	if resp.Header.Get("Access-Control-Allow-Origin") != steamOrigin ||
		fmt.Sprint(versions["Alpha"]) != "[1.0.0 aa]" ||
		versions["Overridden"][0] != "2.0.0" || len(versions["Overridden"]) != 3 ||
		versions["Bravo"][0] != "1.2.0" {
		t.Fatalf("versions = %v", versions)
	}

	// Increments: custom answered locally, upstream forwarded.
	h.do(t, "POST", "/plugins/Overridden/versions/2.0.0/increment?isUpdate=False", nil)
	h.do(t, "POST", "/plugins/Alpha/versions/1.0.0/increment?isUpdate=True", nil)
	if len(h.up.increments) != 1 || !strings.Contains(h.up.increments[0], "/Alpha/") {
		t.Fatalf("increments = %v", h.up.increments)
	}

	// Second refresh uses the ETag.
	h.disc.Refresh(context.Background())
	if h.gh.apiCalls != 2 {
		t.Errorf("api calls = %d", h.gh.apiCalls)
	}
}

func TestTransientFailuresKeepPreviousData(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.Sources.GitHub.Orgs = []string{"org"} })
	setupOrg(h)
	h.disc.Refresh(context.Background())

	h.gh.mu.Lock()
	h.gh.failing["org/plugin-b"] = true
	h.gh.mu.Unlock()
	cat := h.disc.Refresh(context.Background())
	if cat.lookup("Bravo") == nil {
		t.Fatal("Bravo dropped on a transient error")
	}

	h.gh.mu.Lock()
	h.gh.apiDown = true
	h.gh.mu.Unlock()
	h.disc.etags = map[string]cachedResponse{}
	cat = h.disc.Refresh(context.Background())
	if cat.lookup("Overridden") == nil || cat.lookup("Bravo") == nil {
		t.Fatalf("org listing failure dropped plugins: %v", cat.Plugins)
	}

	// Upstream down: custom plugins are still served.
	h.up.down.Store(true)
	resp, body := h.do(t, "GET", "/plugins?sort_by=name", steam)
	if resp.StatusCode != 200 || fmt.Sprint(names(t, body)) != "[Bravo Overridden]" {
		t.Fatalf("got %d %s", resp.StatusCode, body)
	}
}

func TestCacheSurvivesRestart(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.Sources.GitHub.Orgs = []string{"org"} })
	setupOrg(h)
	h.disc.Refresh(context.Background())

	d2 := newDiscoverer(h.cfg, &http.Client{})
	d2.loadCache()
	if d2.Catalog().lookup("Bravo") == nil {
		t.Fatal("cache not restored")
	}

	// Removing the org from config drops its cached plugins.
	cfg := *h.cfg
	cfg.Sources.GitHub = GitHubSource{Repos: []string{"someone/else"}, Topics: []string{"decky-plugin"}}
	d3 := newDiscoverer(&cfg, &http.Client{})
	d3.loadCache()
	if !d3.Catalog().empty() {
		t.Fatal("stale cached plugins kept")
	}

	// A cache written by another version is ignored.
	os.WriteFile(d2.cachePath(), []byte(`{"version":1,"repos":{"org/plugin-b":{"repo":"org/plugin-b","source":"github:org:org","latest":[{"name":"Bravo"}]}}}`), 0o644)
	d4 := newDiscoverer(h.cfg, &http.Client{})
	d4.loadCache()
	if !d4.Catalog().empty() {
		t.Fatal("cache of another version loaded")
	}
}

func TestConflicts(t *testing.T) {
	h := newHarness(t, func(c *Config) {
		c.Sources.GitHub.Orgs = []string{"org"}
		c.Sources.GitHub.Repos = []string{"https://github.com/someone/fork.git"}
	})
	h.gh.repos = []ghRepo{
		{FullName: "org/a1", Topics: []string{"decky-plugin"}},
		{FullName: "org/a2", Topics: []string{"decky-plugin"}},
		{FullName: "org/b", Topics: []string{"decky-plugin"}},
	}
	h.gh.manifests["org/a1"] = manifest("Same", "1.0.0", "org/a1", "")
	h.gh.manifests["org/a2"] = manifest("Same", "1.0.0", "org/a2", "")
	h.gh.manifests["org/b"] = manifest("Forked", "1.0.0", "org/b", "")
	h.gh.manifests["someone/fork"] = manifest("Forked", "9.0.0", "someone/fork", "")
	cat := h.disc.Refresh(context.Background())

	if cat.lookup("Same") != nil {
		t.Error("ambiguous name not dropped")
	}
	if e := cat.lookup("Forked"); e == nil || e.Repo != "someone/fork" {
		t.Errorf("explicit repo should win: %+v", e)
	}
}

func TestConfig(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "c.toml")

	os.WriteFile(p, []byte(""), 0o644)
	c, err := loadConfig(p)
	if err != nil || c.Port != 8737 || c.ListenAddress != "127.0.0.1" || c.hasSources() || c.Upstream != defaultUpstream ||
		fmt.Sprint(c.Sources.GitHub.Topics) != "[decky-plugin]" {
		t.Fatalf("empty config: %+v %v", c, err)
	}

	tok := filepath.Join(dir, "token")
	os.WriteFile(tok, []byte("secret\n"), 0o600)
	os.WriteFile(p, []byte(`
port = 9000
listenAddress = "::1"
refreshInterval = "1h"

[sources.github]
orgs = ["my-org"]
users = ["some-user"]
repos = ["https://github.com/a/b/", "c/d.e"]
topics = ["decky-plugin", "my-store"]
tokenFile = "`+tok+`"
`), 0o644)
	c, err = loadConfig(p)
	gh := c.Sources.GitHub
	if err != nil || c.addr() != "[::1]:9000" || fmt.Sprint(gh.Repos) != "[a/b c/d.e]" || c.RefreshInterval.Hours() != 1 ||
		fmt.Sprint(gh.Users) != "[some-user]" || fmt.Sprint(gh.Topics) != "[decky-plugin my-store]" || c.githubToken != "secret" || !c.hasSources() {
		t.Fatalf("%+v %v", c, err)
	}

	for _, bad := range []string{
		`prot = 1`,
		`orgs = ["a"]`, // old top-level format is not accepted
		`extraRepos = ["a/b"]`,
		`githubTokenFile = "/x"`,
		"[sources.github]\nrepos = [\"nope\"]",
		"[sources.github]\norgs = [\"a/b\"]",
		"[sources.github]\nusers = [\"-bad\"]",
		"[sources.github]\ntopics = []",
		"[sources.github]\ntopics = [\"Upper\"]",
		"[sources.github]\nfoo = 1",
		"[sources.gitlab]\nrepos = [\"a/b\"]",
		`refreshInterval = "5s"`,
	} {
		os.WriteFile(p, []byte(bad), 0o644)
		if _, err := loadConfig(p); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
	if _, err := loadConfig(filepath.Join(dir, "missing.toml")); err == nil {
		t.Error("explicit missing config accepted")
	}
}

func TestUsersAndTopics(t *testing.T) {
	h := newHarness(t, func(c *Config) {
		c.Sources.GitHub.Users = []string{"alice"}
		c.Sources.GitHub.Repos = []string{"bob/untagged"}
		c.Sources.GitHub.Topics = []string{"decky-plugin", "my-store"}
	})
	h.gh.userRepos["alice"] = []ghRepo{
		{FullName: "alice/one", Topics: []string{"decky-plugin"}},
		{FullName: "alice/two", Topics: []string{"my-store", "other"}},
		{FullName: "alice/none", Topics: []string{"other"}},
		{FullName: "alice/old", Topics: []string{"decky-plugin"}, Archived: true},
	}
	h.gh.manifests["alice/one"] = manifest("One", "1.0.0", "alice/one", "")
	h.gh.manifests["alice/two"] = manifest("Two", "1.0.0", "alice/two", "")
	h.gh.manifests["alice/none"] = manifest("None", "1.0.0", "alice/none", "")
	h.gh.manifests["alice/old"] = manifest("Old", "1.0.0", "alice/old", "")
	h.gh.manifests["bob/untagged"] = manifest("Bob", "1.0.0", "bob/untagged", "")
	cat := h.disc.Refresh(context.Background())

	var got []string
	for _, e := range cat.Plugins {
		got = append(got, e.Latest.Name+"="+e.Source)
	}
	if fmt.Sprint(got) != "[Bob=github:repo One=github:user:alice Two=github:user:alice]" {
		t.Fatalf("got %v, errors %v", got, cat.Errors)
	}

	resp, body := h.do(t, "GET", "/_proxy/status", nil)
	var st struct {
		PassThrough bool `json:"passThrough"`
		Sources     struct {
			GitHub struct {
				Orgs, Users, Repos, Topics []string
				Authenticated              bool
			} `json:"github"`
		} `json:"sources"`
		Plugins []struct {
			Name, Version, Repo, Source string
			Versions                    []string
		} `json:"plugins"`
		Errors []string `json:"errors"`
	}
	if err := json.Unmarshal([]byte(body), &st); err != nil || resp.StatusCode != 200 {
		t.Fatalf("status: %v %s", err, body)
	}
	gh := st.Sources.GitHub
	if st.PassThrough || fmt.Sprint(gh.Users) != "[alice]" || gh.Orgs == nil || len(gh.Orgs) != 0 ||
		fmt.Sprint(gh.Repos) != "[bob/untagged]" || gh.Authenticated || len(st.Plugins) != 3 ||
		st.Plugins[1].Source != "github:user:alice" || fmt.Sprint(st.Plugins[1].Versions) != "[1.0.0]" || st.Errors == nil {
		t.Fatalf("status = %s", body)
	}
}

func TestManifestValidation(t *testing.T) {
	good := func() Manifest {
		return Manifest{Schema: 2, Plugins: []Plugin{
			{Name: "X", Version: "1.0.0-rc.1", SHA256: strings.Repeat("0", 64), Artifact: "https://github.com/o/r/releases/download/v1/x.zip"},
			{Name: "Y", Version: "2.0.0", SHA256: strings.Repeat("1", 64), Artifact: "https://github.com/o/r/releases/download/v1/y.zip"},
		}}
	}
	if m := good(); m.validate() != nil {
		t.Fatal(m.validate())
	}
	for _, mut := range []func(*Manifest){
		func(m *Manifest) { m.Schema = 1 },
		func(m *Manifest) { m.Plugins = nil },
		func(m *Manifest) { m.Plugins[1].Name = "X" },
		func(m *Manifest) { m.Plugins[1].Version = "v1.0.0" },
		func(m *Manifest) { m.Plugins[1].SHA256 = strings.Repeat("A", 64) },
		func(m *Manifest) { m.Plugins[1].Artifact = "http://github.com/o/r/releases/download/x" },
		func(m *Manifest) { m.Plugins[1].Artifact = "https://evil.example/o/r/releases/download/x" },
		func(m *Manifest) { m.Plugins[1].Name = "a/b" },
		func(m *Manifest) { m.Plugins[1].ImageURL = "http://x" },
	} {
		m := good()
		mut(&m)
		if m.validate() == nil {
			t.Errorf("accepted %+v", m)
		}
	}
}

// Regression: Decky labels an update "downgrade" if the installed version is
// missing from the store's versions list, so all releases must be listed.
func TestReleaseHistory(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.Sources.GitHub.Repos = []string{"o/p"} })
	m010 := manifest("P", "0.1.0", "o/p", "2026-01-01T00:00:00Z")
	m011 := manifest("P", "0.1.1", "o/p", "2026-02-01T00:00:00Z")
	m012 := manifest("P", "0.1.2", "o/p", "2026-03-01T00:00:00Z")
	h.gh.manifests["o/p"] = m011
	h.gh.releases["o/p"] = []fakeRelease{
		{tag: "v0.2.0-rc.1", prerelease: true, manifest: manifest("P", "0.2.0-rc.1", "o/p", "")},
		{tag: "v0.1.1", manifest: m011},
		{tag: "v0.1.0", manifest: m010},
		{tag: "v0.0.9"}, // pre-workflow release without manifest
	}
	versionsOf := func() string {
		_, body := h.do(t, "GET", "/plugins?sort_by=name", steam)
		var list []map[string]any
		json.Unmarshal([]byte(body), &list)
		for _, p := range list {
			if p["name"] == "P" {
				var vs []string
				for _, v := range p["versions"].([]any) {
					vs = append(vs, v.(map[string]any)["name"].(string))
				}
				return fmt.Sprintf("%v created=%v updated=%v", vs, p["created"], p["updated"])
			}
		}
		return "missing"
	}

	h.disc.Refresh(context.Background())
	if got := versionsOf(); got != "[0.1.1 0.1.0] created=2026-01-01T00:00:00Z updated=2026-02-01T00:00:00Z" {
		t.Fatalf("got %s", got)
	}
	if h.gh.relCalls != 1 {
		t.Fatalf("release API calls = %d", h.gh.relCalls)
	}

	// Unchanged latest release: no API call.
	h.disc.Refresh(context.Background())
	if h.gh.relCalls != 1 {
		t.Fatalf("history refetched although nothing changed (%d calls)", h.gh.relCalls)
	}

	// New release while the API is rate limited: degrade, then recover.
	h.gh.mu.Lock()
	h.gh.manifests["o/p"] = m012
	h.gh.releases["o/p"] = append([]fakeRelease{{tag: "v0.1.2", manifest: m012}}, h.gh.releases["o/p"]...)
	h.gh.apiDown = true
	h.gh.mu.Unlock()
	cat := h.disc.Refresh(context.Background())
	if e := cat.lookup("P"); e == nil || e.HistoryComplete || len(e.History) != 3 {
		t.Fatalf("fallback history: %+v", e)
	}
	h.gh.mu.Lock()
	h.gh.apiDown = false
	h.gh.mu.Unlock()
	h.disc.Refresh(context.Background())
	if got := versionsOf(); !strings.HasPrefix(got, "[0.1.2 0.1.1 0.1.0]") {
		t.Fatalf("got %s", got)
	}
	if e := h.disc.Catalog().lookup("P"); !e.HistoryComplete {
		t.Fatal("history not completed after recovery")
	}

	// /versions only carries the latest.
	_, body := h.do(t, "GET", "/plugins/versions", steam)
	if !strings.Contains(body, `"P":["0.1.2"`) {
		t.Fatalf("versions = %s", body)
	}
}

func TestCompareSemver(t *testing.T) {
	ordered := []string{"0.9.9", "0.10.0", "1.0.0-alpha", "1.0.0-alpha.1", "1.0.0-alpha.beta", "1.0.0-beta", "1.0.0-beta.2", "1.0.0-beta.11", "1.0.0-rc.1", "1.0.0", "1.0.1", "10.0.0"}
	for i := range ordered {
		for j := range ordered {
			got := compareSemver(ordered[i], ordered[j])
			want := cmp.Compare(i, j)
			if (got > 0) != (want > 0) || (got < 0) != (want < 0) {
				t.Errorf("compare(%s, %s) = %d, want sign %d", ordered[i], ordered[j], got, want)
			}
		}
	}
}

// A project releasing several packages at once: tags don't match plugin
// versions, plugin versions repeat across releases, and a release can carry
// several plugins.
func TestMultiPluginProject(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.Sources.GitHub.Repos = []string{"o/project"} })
	const r = "o/project"
	rel := func(tag string, plugins ...Plugin) fakeRelease {
		return fakeRelease{tag: tag, manifest: manifestOf(plugins...)}
	}
	a1 := plugin("A", "1.0.0", r, "2026-01-01T00:00:00Z")
	a1rebuilt := a1
	a1rebuilt.SHA256, a1rebuilt.Created = strings.Repeat("cd", 32), a1.Created.AddDate(0, 1, 0)
	a2 := plugin("A", "1.1.0", r, "2026-03-01T00:00:00Z")
	b1 := plugin("B", "0.1.0", r, "2026-03-01T00:00:00Z")
	h.gh.releases[r] = []fakeRelease{
		rel("project-3.0.0", a2, b1),
		rel("project-2.0.0", a1rebuilt),
		{tag: "project-1.5.0"}, // a release without the plugins
		rel("project-1.0.0", a1),
	}
	h.gh.manifests[r] = h.gh.releases[r][0].manifest

	cat := h.disc.Refresh(context.Background())
	a, b := cat.lookup("A"), cat.lookup("B")
	if a == nil || b == nil || len(cat.Errors) != 0 {
		t.Fatalf("plugins = %+v, errors = %v", cat.Plugins, cat.Errors)
	}
	var vs []string
	for _, v := range a.History {
		vs = append(vs, v.Version+"/"+v.SHA256[:2]+"/"+v.Created.Format("01"))
	}
	// 1.0.0 is in two releases: the newer copy, with the older creation time.
	if fmt.Sprint(vs) != "[1.1.0/ab/03 1.0.0/cd/01]" || !a.HistoryComplete {
		t.Fatalf("A history = %v", vs)
	}
	if len(b.History) != 1 || a.id() == b.id() {
		t.Fatalf("B = %+v", b)
	}

	// Next project release rebuilds the same plugin versions (new hashes):
	// no API request, the new copies are served.
	a2r, b1r := a2, b1
	a2r.SHA256, b1r.SHA256 = strings.Repeat("ef", 32), strings.Repeat("ef", 32)
	h.gh.mu.Lock()
	h.gh.releases[r] = append([]fakeRelease{rel("project-3.0.1", a2r, b1r)}, h.gh.releases[r]...)
	h.gh.manifests[r] = h.gh.releases[r][0].manifest
	calls := h.gh.relCalls
	h.gh.mu.Unlock()
	cat = h.disc.Refresh(context.Background())
	if h.gh.relCalls != calls {
		t.Fatalf("release list fetched for a rebuild")
	}
	if e := cat.lookup("A"); e.Latest.SHA256 != a2r.SHA256 || e.History[0].SHA256 != a2r.SHA256 || len(e.History) != 2 {
		t.Fatalf("A = %+v", e)
	}

	// A plugin dropped from the latest release disappears.
	h.gh.mu.Lock()
	h.gh.releases[r] = append([]fakeRelease{rel("project-4.0.0", a2r)}, h.gh.releases[r]...)
	h.gh.manifests[r] = h.gh.releases[r][0].manifest
	h.gh.mu.Unlock()
	if cat = h.disc.Refresh(context.Background()); cat.lookup("B") != nil || cat.lookup("A") == nil {
		t.Fatalf("plugins = %+v", cat.Plugins)
	}
}

// Long release lists are read completely once, then only the newest page.
func TestReleasePagination(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.Sources.GitHub.Repos = []string{"o/p"} })
	var rels []fakeRelease
	for i := 250; i >= 1; i-- {
		v := fmt.Sprintf("1.0.%d", i)
		rels = append(rels, fakeRelease{tag: "v" + v, manifest: manifest("P", v, "o/p", "")})
	}
	h.gh.releases["o/p"] = rels
	h.gh.manifests["o/p"] = rels[0].manifest

	cat := h.disc.Refresh(context.Background())
	if e := cat.lookup("P"); e == nil || len(e.History) != 250 || e.History[249].Version != "1.0.1" {
		t.Fatalf("history: %+v %v", e, cat.Errors)
	}
	if h.gh.relCalls != 3 || h.gh.downloads != 250 {
		t.Fatalf("cold start: %d pages, %d downloads", h.gh.relCalls, h.gh.downloads)
	}

	// A new version, and an old release (on page 1) deleted.
	h.gh.mu.Lock()
	rels = append([]fakeRelease{{tag: "v1.0.251", manifest: manifest("P", "1.0.251", "o/p", "")}}, rels...)
	rels = slices.Delete(rels, 3, 4) // v1.0.248
	h.gh.releases["o/p"] = rels
	h.gh.manifests["o/p"] = rels[0].manifest
	h.gh.mu.Unlock()
	cat = h.disc.Refresh(context.Background())
	e := cat.lookup("P")
	if h.gh.relCalls != 4 || h.gh.downloads != 251 {
		t.Fatalf("incremental: %d pages, %d downloads", h.gh.relCalls, h.gh.downloads)
	}
	has := func(v string) bool {
		return slices.ContainsFunc(e.History, func(p Plugin) bool { return p.Version == v })
	}
	if len(e.History) != 250 || !has("1.0.251") || has("1.0.248") || !has("1.0.1") || !e.HistoryComplete {
		t.Fatalf("history after update: %d versions", len(e.History))
	}

	// The release records survive a restart: no downloads again.
	d2 := newDiscoverer(h.cfg, &http.Client{})
	d2.loadCache()
	h.gh.mu.Lock()
	rels = append([]fakeRelease{{tag: "v1.0.252", manifest: manifest("P", "1.0.252", "o/p", "")}}, rels...)
	h.gh.releases["o/p"] = rels
	h.gh.manifests["o/p"] = rels[0].manifest
	h.gh.mu.Unlock()
	cat = d2.Refresh(context.Background())
	if e := cat.lookup("P"); h.gh.relCalls != 5 || h.gh.downloads != 252 || len(e.History) != 251 {
		t.Fatalf("after restart: %d pages, %d downloads, %d versions", h.gh.relCalls, h.gh.downloads, len(e.History))
	}
}

// TestManifestFile validates a decky-store.json written by the packaging
// action (CI sets DECKY_STORE_MANIFEST).
func TestManifestFile(t *testing.T) {
	p := os.Getenv("DECKY_STORE_MANIFEST")
	if p == "" {
		t.Skip("DECKY_STORE_MANIFEST not set")
	}
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	var m Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if err := m.validate(); err != nil {
		t.Fatal(err)
	}
}
