package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

const (
	defaultUpstream        = "https://plugins.deckbrew.xyz/plugins"
	defaultPort            = 8737
	defaultListenAddress   = "127.0.0.1"
	defaultTopic           = "decky-plugin"
	defaultRefreshInterval = 15 * time.Minute
)

// Config is the on-disk TOML configuration. Key names match the NixOS
// module's `services.decky-store-proxy.settings` 1:1.
type Config struct {
	ListenAddress   string   `toml:"listenAddress"`
	Port            int      `toml:"port"`
	Upstream        string   `toml:"upstream"`
	RefreshInterval Duration `toml:"refreshInterval"`
	CacheDir        string   `toml:"cacheDir"`
	Sources         Sources  `toml:"sources"`

	// Not configurable from TOML; overridden in tests.
	githubAPI string
	githubWeb string
	// Resolved from Sources.GitHub.TokenFile / $GITHUB_TOKEN.
	githubToken string
}

// Sources lists where custom plugins come from. Each kind of source gets its
// own table, so more kinds can be added later.
type Sources struct {
	GitHub GitHubSource `toml:"github"`
}

// GitHubSource is the [sources.github] table.
type GitHubSource struct {
	// Owners whose public, non-archived repos with one of Topics are scanned.
	Orgs  []string `toml:"orgs" json:"orgs"`
	Users []string `toml:"users" json:"users"`
	// Repos are always included (no topic required) and win name conflicts.
	Repos []string `toml:"repos" json:"repos"`
	// Topics filters org/user repos: a repo needs at least one of them.
	// Omitted means ["decky-plugin"]; an explicit empty list is an error.
	Topics    []string `toml:"topics" json:"topics"`
	TokenFile string   `toml:"tokenFile" json:"-"`
}

func (g *GitHubSource) configured() bool {
	return len(g.Orgs) > 0 || len(g.Users) > 0 || len(g.Repos) > 0
}

func (g *GitHubSource) hasTopic(topics []string) bool {
	return slices.ContainsFunc(topics, func(t string) bool { return slices.Contains(g.Topics, t) })
}

// Duration accepts Go duration strings such as "15m" or "1h".
type Duration struct{ time.Duration }

func (d *Duration) UnmarshalText(b []byte) error {
	v, err := time.ParseDuration(string(b))
	if err != nil {
		return err
	}
	d.Duration = v
	return nil
}

var (
	ownerNameRe = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,38})$`)
	topicRe     = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,49}$`)
	repoNameRe  = regexp.MustCompile(`^[A-Za-z0-9-]{1,39}/[A-Za-z0-9._-]{1,100}$`)
)

// hasSources reports whether anything besides the upstream store is configured.
// Without sources the proxy is a pure pass-through.
func (c *Config) hasSources() bool {
	return c.Sources.GitHub.configured()
}

func (c *Config) addr() string {
	host := c.ListenAddress
	if strings.Contains(host, ":") && !strings.HasPrefix(host, "[") {
		host = "[" + host + "]" // IPv6
	}
	return fmt.Sprintf("%s:%d", host, c.Port)
}

// loadConfig reads path. If path is empty, the default location is used and a
// missing file is not an error (it means: pure pass-through with defaults).
func loadConfig(path string) (*Config, error) {
	c := &Config{}
	explicit := path != ""
	if !explicit {
		if dir, err := os.UserConfigDir(); err == nil {
			path = filepath.Join(dir, "decky-store-proxy", "config.toml")
		}
	}
	if path != "" {
		md, err := toml.DecodeFile(path, c)
		switch {
		case err == nil:
			if und := md.Undecoded(); len(und) > 0 {
				keys := make([]string, len(und))
				for i, k := range und {
					keys[i] = k.String()
				}
				return nil, fmt.Errorf("%s: unknown keys: %s", path, strings.Join(keys, ", "))
			}
		case errors.Is(err, os.ErrNotExist) && !explicit:
			// fine: defaults only
		default:
			return nil, fmt.Errorf("reading config: %w", err)
		}
	}
	if err := c.finalize(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return c, nil
}

func (c *Config) finalize() error {
	if c.ListenAddress == "" {
		c.ListenAddress = defaultListenAddress
	}
	if c.Port == 0 {
		c.Port = defaultPort
	}
	if c.Port < 1 || c.Port > 65535 {
		return fmt.Errorf("port %d out of range", c.Port)
	}
	if c.Upstream == "" {
		c.Upstream = defaultUpstream
	}
	c.Upstream = strings.TrimRight(c.Upstream, "/")
	if !strings.HasPrefix(c.Upstream, "https://") && !strings.HasPrefix(c.Upstream, "http://") {
		return fmt.Errorf("upstream %q must be an http(s) URL", c.Upstream)
	}
	if c.RefreshInterval.Duration == 0 {
		c.RefreshInterval.Duration = defaultRefreshInterval
	}
	if c.RefreshInterval.Duration < time.Minute {
		return fmt.Errorf("refreshInterval must be at least 1m")
	}
	if err := c.Sources.GitHub.finalize(); err != nil {
		return fmt.Errorf("sources.github: %w", err)
	}
	if c.CacheDir == "" {
		if d := os.Getenv("CACHE_DIRECTORY"); d != "" { // set by systemd's CacheDirectory=
			c.CacheDir = strings.Split(d, ":")[0]
		} else if d, err := os.UserCacheDir(); err == nil {
			c.CacheDir = filepath.Join(d, "decky-store-proxy")
		}
	}
	if tf := c.Sources.GitHub.TokenFile; tf != "" {
		b, err := os.ReadFile(tf)
		if err != nil {
			return fmt.Errorf("reading sources.github.tokenFile: %w", err)
		}
		c.githubToken = strings.TrimSpace(string(b))
	} else {
		c.githubToken = os.Getenv("GITHUB_TOKEN")
	}
	if c.githubAPI == "" {
		c.githubAPI = "https://api.github.com"
	}
	if c.githubWeb == "" {
		c.githubWeb = "https://github.com"
	}
	return nil
}

func (g *GitHubSource) finalize() error {
	for _, list := range []struct {
		key   string
		names []string
	}{{"orgs", g.Orgs}, {"users", g.Users}} {
		for _, n := range list.names {
			if !ownerNameRe.MatchString(n) {
				return fmt.Errorf("invalid %s entry %q", list.key, n)
			}
		}
	}
	for i, r := range g.Repos {
		n, err := normalizeRepo(r)
		if err != nil {
			return err
		}
		g.Repos[i] = n
	}
	switch {
	case g.Topics == nil:
		g.Topics = []string{defaultTopic}
	case len(g.Topics) == 0:
		return fmt.Errorf("topics must not be empty (omit it to use [%q])", defaultTopic)
	}
	for _, t := range g.Topics {
		if !topicRe.MatchString(t) {
			return fmt.Errorf("invalid topic %q (lowercase letters, digits and hyphens)", t)
		}
	}
	return nil
}

// normalizeRepo accepts "owner/repo", "github.com/owner/repo" or
// "https://github.com/owner/repo(.git)" and returns "owner/repo".
func normalizeRepo(s string) (string, error) {
	r := strings.TrimSpace(s)
	r = strings.TrimPrefix(r, "https://")
	r = strings.TrimPrefix(r, "http://")
	r = strings.TrimPrefix(r, "github.com/")
	r = strings.TrimSuffix(r, "/")
	r = strings.TrimSuffix(r, ".git")
	if !repoNameRe.MatchString(r) {
		return "", fmt.Errorf("invalid repos entry %q (expected owner/repo or a github.com URL)", s)
	}
	return r, nil
}
