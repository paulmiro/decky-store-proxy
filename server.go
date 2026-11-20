package main

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"slices"
	"strings"
	"time"
)

// The Steam client UI (where Decky's store fetches run) uses this origin.
// The upstream store allows exactly this origin too.
const steamOrigin = "https://steamloopback.host"

const basePath = "/plugins"

type Server struct {
	cfg      *Config
	disc     *Discoverer
	client   *http.Client
	upstream *url.URL
	proxy    *httputil.ReverseProxy
}

func newServer(cfg *Config, disc *Discoverer, client *http.Client) (*Server, error) {
	up, err := url.Parse(cfg.Upstream)
	if err != nil {
		return nil, err
	}
	s := &Server{cfg: cfg, disc: disc, client: client, upstream: up}
	s.proxy = &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL = s.upstreamURL(pr.In.URL)
			pr.Out.Host = ""
		},
		Transport: client.Transport,
		ModifyResponse: func(resp *http.Response) error {
			stripCORS(resp.Header)
			setCORS(resp.Header, resp.Request)
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			slog.Warn("upstream request failed", "path", r.URL.Path, "err", err)
			setCORS(w.Header(), r)
			w.WriteHeader(http.StatusBadGateway)
		},
	}
	return s, nil
}

// upstreamURL maps /plugins<suffix>?<query> to <upstream><suffix>?<query>.
func (s *Server) upstreamURL(in *url.URL) *url.URL {
	out := *s.upstream
	out.Path = s.upstream.Path + strings.TrimPrefix(in.Path, basePath)
	// Keep the original escaping (e.g. %2F in a plugin name); ignored by
	// net/url if inconsistent with Path.
	out.RawPath = s.upstream.EscapedPath() + strings.TrimPrefix(in.EscapedPath(), basePath)
	out.RawQuery = in.RawQuery
	return &out
}

func stripCORS(h http.Header) {
	for k := range h {
		if strings.HasPrefix(k, "Access-Control-") {
			h.Del(k)
		}
	}
}

// setCORS mirrors the upstream store's CORS policy for the Steam UI origin.
// The request may be nil or lack an Origin (Decky's Python backend, curl):
// then nothing is added, exactly like upstream.
func setCORS(h http.Header, r *http.Request) {
	if r == nil || r.Header.Get("Origin") != steamOrigin {
		return
	}
	h.Set("Access-Control-Allow-Origin", steamOrigin)
	h.Set("Access-Control-Allow-Credentials", "true")
	h.Add("Vary", "Origin")
	if r.Method == http.MethodOptions {
		h.Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		if rh := r.Header.Get("Access-Control-Request-Headers"); rh != "" {
			h.Set("Access-Control-Allow-Headers", rh)
		}
		h.Set("Access-Control-Max-Age", "600")
		// Not sent by the current Steam client, but harmless if a future
		// Chromium starts doing Private Network Access preflights.
		if r.Header.Get("Access-Control-Request-Private-Network") == "true" {
			h.Set("Access-Control-Allow-Private-Network", "true")
		}
	} else {
		h.Set("Access-Control-Expose-Headers", "*")
	}
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	if r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
		setCORS(w.Header(), r)
		w.WriteHeader(http.StatusOK)
		return
	}
	switch path {
	case "/_proxy/status":
		s.handleStatus(w, r)
		return
	case "/_proxy/refresh":
		if r.Method != http.MethodPost {
			http.Error(w, "use POST", http.StatusMethodNotAllowed)
			return
		}
		s.disc.Refresh(r.Context())
		s.handleStatus(w, r)
		return
	}
	if path != basePath && !strings.HasPrefix(path, basePath+"/") {
		http.NotFound(w, r)
		return
	}

	cat := s.disc.Catalog()
	if cat.empty() {
		// Nothing to inject: pure pass-through.
		s.proxy.ServeHTTP(w, r)
		return
	}
	switch {
	case r.Method == http.MethodGet && path == basePath:
		s.handleList(w, r, cat)
	case r.Method == http.MethodGet && path == basePath+"/versions":
		s.handleVersions(w, r, cat)
	case r.Method == http.MethodPost && s.isCustomIncrement(r, cat):
		setCORS(w.Header(), r)
		w.WriteHeader(http.StatusOK)
	default:
		s.proxy.ServeHTTP(w, r)
	}
}

// isCustomIncrement matches POST /plugins/{name}/versions/{ver}/increment for a
// custom plugin. Those must not reach upstream: it would count the install for
// an unrelated upstream plugin of the same name (or just 404).
func (s *Server) isCustomIncrement(r *http.Request, cat *Catalog) bool {
	rest, ok := strings.CutPrefix(r.URL.EscapedPath(), basePath+"/")
	if !ok {
		return false
	}
	parts := strings.Split(rest, "/")
	if len(parts) != 4 || parts[1] != "versions" || parts[3] != "increment" {
		return false
	}
	name, err := url.PathUnescape(parts[0])
	return err == nil && cat.lookup(name) != nil
}

// fetchUpstream GETs the upstream equivalent of r. ok=false means the caller
// should serve custom plugins only (network error or 5xx). A non-nil resp with
// ok=false and a 4xx status is relayed as-is (e.g. 426 "upgrade Decky").
func (s *Server) fetchUpstream(r *http.Request) (body []byte, resp *http.Response, ok bool) {
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.upstreamURL(r.URL).String(), nil)
	if err != nil {
		return nil, nil, false
	}
	for _, h := range []string{"X-Decky-Version", "Accept-Language", "User-Agent"} {
		if v := r.Header.Get(h); v != "" {
			req.Header.Set(h, v)
		}
	}
	resp, err = s.client.Do(req)
	if err != nil {
		slog.Warn("upstream unreachable, serving custom plugins only", "err", err)
		return nil, nil, false
	}
	defer resp.Body.Close()
	body, err = io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		slog.Warn("reading upstream response", "err", err)
		return nil, nil, false
	}
	if resp.StatusCode >= 500 {
		slog.Warn("upstream error, serving custom plugins only", "status", resp.StatusCode)
		return nil, nil, false
	}
	return body, resp, resp.StatusCode == http.StatusOK
}

func relay(w http.ResponseWriter, r *http.Request, resp *http.Response, body []byte) {
	for k, vs := range resp.Header {
		if k == "Content-Length" || k == "Content-Encoding" || k == "Transfer-Encoding" || k == "Connection" {
			continue
		}
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	stripCORS(w.Header())
	setCORS(w.Header(), r)
	w.WriteHeader(resp.StatusCode)
	w.Write(body)
}

func writeJSON(w http.ResponseWriter, r *http.Request, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	setCORS(w.Header(), r)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write(b)
}

func (s *Server) handleVersions(w http.ResponseWriter, r *http.Request, cat *Catalog) {
	merged := map[string]any{}
	body, resp, ok := s.fetchUpstream(r)
	if resp != nil && !ok {
		relay(w, r, resp, body)
		return
	}
	if ok {
		var up map[string]json.RawMessage
		if err := json.Unmarshal(body, &up); err != nil {
			slog.Warn("upstream versions response is not an object; relaying unchanged", "err", err)
			relay(w, r, resp, body)
			return
		}
		for k, v := range up {
			merged[k] = v
		}
	}
	for _, e := range cat.Plugins {
		m := e.Manifest
		merged[m.Name] = []string{m.Version, m.SHA256, m.Artifact}
	}
	writeJSON(w, r, merged)
}

// storeEntry renders a custom plugin in the upstream store's list format.
func storeEntry(e *Entry) map[string]any {
	m := e.Manifest
	ts := func(t time.Time) any {
		if t.IsZero() {
			return nil
		}
		return t.UTC().Format(time.RFC3339)
	}
	versions := e.versions()
	// Like upstream: created = first release, updated = latest release.
	var first time.Time
	for _, v := range versions {
		if !v.Created.IsZero() && (first.IsZero() || v.Created.Before(first)) {
			first = v.Created
		}
	}
	created, updated := ts(first), ts(m.Created)
	var vs []map[string]any
	for _, v := range versions {
		vs = append(vs, map[string]any{
			"name":      v.Version,
			"hash":      v.SHA256,
			"artifact":  v.Artifact,
			"created":   ts(v.Created),
			"downloads": 0,
			"updates":   0,
		})
	}
	tags := m.Tags
	if tags == nil {
		tags = []string{}
	}
	return map[string]any{
		"id":          e.id(),
		"name":        m.Name,
		"author":      m.Author,
		"description": m.Description,
		"tags":        tags,
		"visible":     true,
		"image_url":   m.ImageURL,
		"downloads":   0,
		"updates":     0,
		"created":     created,
		"updated":     updated,
		"versions":    vs,
	}
}

// sortKey extracts what the upstream store sorts by.
type sortKey struct {
	name      string
	created   time.Time
	downloads int64
}

func keyOf(raw map[string]json.RawMessage) sortKey {
	var k sortKey
	json.Unmarshal(raw["name"], &k.name)
	var c string
	if json.Unmarshal(raw["created"], &c) == nil {
		k.created, _ = time.Parse(time.RFC3339Nano, c)
	}
	json.Unmarshal(raw["downloads"], &k.downloads)
	return k
}

func (s *Server) handleList(w http.ResponseWriter, r *http.Request, cat *Catalog) {
	q := r.URL.Query()
	var upstream []map[string]json.RawMessage
	body, resp, ok := s.fetchUpstream(r)
	if resp != nil && !ok {
		relay(w, r, resp, body)
		return
	}
	if ok {
		if err := json.Unmarshal(body, &upstream); err != nil {
			slog.Warn("upstream list is not an array; relaying unchanged", "err", err)
			relay(w, r, resp, body)
			return
		}
	}

	// Custom plugins replace upstream plugins of the same name.
	upstream = slices.DeleteFunc(upstream, func(p map[string]json.RawMessage) bool {
		return cat.lookup(keyOf(p).name) != nil
	})

	// Apply the same filters the upstream store supports.
	query := strings.ToLower(q.Get("query"))
	var tags []string
	for _, t := range q["tags"] {
		for _, x := range strings.Split(t, ",") {
			if x != "" {
				tags = append(tags, x)
			}
		}
	}
	var custom []map[string]json.RawMessage
	for i := range cat.Plugins {
		e := &cat.Plugins[i]
		if query != "" && !strings.Contains(strings.ToLower(e.Manifest.Name), query) {
			continue
		}
		if !hasAllTags(e.Manifest.Tags, tags) {
			continue
		}
		raw := map[string]json.RawMessage{}
		for k, v := range storeEntry(e) {
			raw[k], _ = json.Marshal(v)
		}
		custom = append(custom, raw)
	}

	merged := mergeSorted(upstream, custom, q.Get("sort_by"), q.Get("sort_direction"))
	writeJSON(w, r, merged)
}

func hasAllTags(have, want []string) bool {
	for _, t := range want {
		if !slices.Contains(have, t) {
			return false
		}
	}
	return true
}

// mergeSorted inserts custom entries into the already-sorted upstream list
// without reordering upstream entries.
func mergeSorted(upstream, custom []map[string]json.RawMessage, sortBy, direction string) []map[string]json.RawMessage {
	var compare func(a, b sortKey) int
	switch sortBy {
	case "name":
		compare = func(a, b sortKey) int { return cmp.Compare(strings.ToLower(a.name), strings.ToLower(b.name)) }
	case "date":
		compare = func(a, b sortKey) int { return a.created.Compare(b.created) }
	case "downloads":
		compare = func(a, b sortKey) int { return cmp.Compare(a.downloads, b.downloads) }
	default:
		// Upstream sorts by its internal id; custom plugins go last.
		return append(upstream, custom...)
	}
	if direction == "desc" {
		asc := compare
		compare = func(a, b sortKey) int { return asc(b, a) }
	}
	slices.SortStableFunc(custom, func(a, b map[string]json.RawMessage) int { return compare(keyOf(a), keyOf(b)) })

	out := make([]map[string]json.RawMessage, 0, len(upstream)+len(custom))
	i := 0
	for _, c := range custom {
		ck := keyOf(c)
		for i < len(upstream) && compare(keyOf(upstream[i]), ck) <= 0 {
			out = append(out, upstream[i])
			i++
		}
		out = append(out, c)
	}
	return append(out, upstream[i:]...)
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	cat := s.disc.Catalog()
	type plugin struct {
		Name, Version, Repo, Source, Artifact string
		Versions                              []string
		HistoryComplete                       bool
	}
	ps := []plugin{}
	for _, e := range cat.Plugins {
		var vs []string
		for _, v := range e.versions() {
			vs = append(vs, v.Version)
		}
		ps = append(ps, plugin{e.Manifest.Name, e.Manifest.Version, e.Repo, e.Source, e.Manifest.Artifact, vs, e.HistoryComplete})
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	enc.Encode(map[string]any{
		"upstream":    s.cfg.Upstream,
		"orgs":        s.cfg.Orgs,
		"extraRepos":  s.cfg.ExtraRepos,
		"passThrough": cat.empty(),
		"updated":     cat.Updated,
		"plugins":     ps,
		"errors":      cat.Errors,
	})
	w.Header().Set("Content-Type", "application/json")
	w.Write(buf.Bytes())
}
