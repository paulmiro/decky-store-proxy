package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
	repos     []ghRepo
	manifests map[string]string // repo -> body; missing = 404
	failing   map[string]bool   // repo -> 500
	apiCalls  int
	apiDown   bool
}

func newFakeGitHub(t *testing.T) *fakeGitHub {
	g := &fakeGitHub{manifests: map[string]string{}, failing: map[string]bool{}}
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

func manifest(name, version, repo, created string, tags ...string) string {
	m := Manifest{
		Schema: 1, Name: name, Version: version,
		Artifact: "https://github.com/" + repo + "/releases/download/v" + version + "/plugin.zip",
		SHA256:   strings.Repeat("ab", 32), Author: "me", Description: "desc " + name,
		Tags: tags, ImageURL: "https://example.com/i.png",
	}
	b, _ := json.Marshal(m)
	if created != "" {
		var raw map[string]any
		json.Unmarshal(b, &raw)
		raw["created"] = created
		b, _ = json.Marshal(raw)
	}
	return string(b)
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
	h.gh.manifests["org/broken"] = `{"schema":1,"name":"Broken","version":"v1.0.0"}`
}

func TestDiscoveryAndMerge(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.Orgs = []string{"org"} })
	setupOrg(h)
	cat := h.disc.Refresh(context.Background())

	var got []string
	for _, e := range cat.Plugins {
		got = append(got, e.Manifest.Name)
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
	h := newHarness(t, func(c *Config) { c.Orgs = []string{"org"} })
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
	h := newHarness(t, func(c *Config) { c.Orgs = []string{"org"} })
	setupOrg(h)
	h.disc.Refresh(context.Background())

	d2 := newDiscoverer(h.cfg, &http.Client{})
	d2.loadCache()
	if d2.Catalog().lookup("Bravo") == nil {
		t.Fatal("cache not restored")
	}

	// Removing the org from config drops its cached plugins.
	cfg := *h.cfg
	cfg.Orgs = nil
	cfg.ExtraRepos = []string{"someone/else"}
	d3 := newDiscoverer(&cfg, &http.Client{})
	d3.loadCache()
	if !d3.Catalog().empty() {
		t.Fatal("stale cached plugins kept")
	}
}

func TestConflicts(t *testing.T) {
	h := newHarness(t, func(c *Config) {
		c.Orgs = []string{"org"}
		c.ExtraRepos = []string{"https://github.com/someone/fork.git"}
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
		t.Errorf("extra repo should win: %+v", e)
	}
}

func TestConfig(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "c.toml")

	os.WriteFile(p, []byte(""), 0o644)
	c, err := loadConfig(p)
	if err != nil || c.Port != 8737 || c.ListenAddress != "127.0.0.1" || c.hasSources() || c.Upstream != defaultUpstream {
		t.Fatalf("empty config: %+v %v", c, err)
	}

	os.WriteFile(p, []byte(`
port = 9000
listenAddress = "::1"
orgs = ["my-org"]
extraRepos = ["https://github.com/a/b/", "c/d.e"]
refreshInterval = "1h"
`), 0o644)
	c, err = loadConfig(p)
	if err != nil || c.addr() != "[::1]:9000" || fmt.Sprint(c.ExtraRepos) != "[a/b c/d.e]" || c.RefreshInterval.Hours() != 1 {
		t.Fatalf("%+v %v", c, err)
	}

	for _, bad := range []string{`prot = 1`, `extraRepos = ["nope"]`, `orgs = ["a/b"]`, `refreshInterval = "5s"`} {
		os.WriteFile(p, []byte(bad), 0o644)
		if _, err := loadConfig(p); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
	if _, err := loadConfig(filepath.Join(dir, "missing.toml")); err == nil {
		t.Error("explicit missing config accepted")
	}
}

func TestManifestValidation(t *testing.T) {
	good := Manifest{Schema: 1, Name: "X", Version: "1.0.0-rc.1", SHA256: strings.Repeat("0", 64),
		Artifact: "https://github.com/o/r/releases/download/v1.0.0-rc.1/x.zip"}
	if err := good.validate(); err != nil {
		t.Fatal(err)
	}
	for _, mut := range []func(*Manifest){
		func(m *Manifest) { m.Schema = 2 },
		func(m *Manifest) { m.Version = "v1.0.0" },
		func(m *Manifest) { m.SHA256 = strings.Repeat("A", 64) },
		func(m *Manifest) { m.Artifact = "http://github.com/o/r/releases/download/x" },
		func(m *Manifest) { m.Artifact = "https://evil.example/o/r/releases/download/x" },
		func(m *Manifest) { m.Name = "a/b" },
		func(m *Manifest) { m.ImageURL = "http://x" },
	} {
		m := good
		mut(&m)
		if m.validate() == nil {
			t.Errorf("accepted %+v", m)
		}
	}
}
