package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
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
	Orgs            []string `toml:"orgs"`
	ExtraRepos      []string `toml:"extraRepos"`
	Topic           string   `toml:"topic"`
	RefreshInterval Duration `toml:"refreshInterval"`
	GithubTokenFile string   `toml:"githubTokenFile"`
	CacheDir        string   `toml:"cacheDir"`

	// Not configurable from TOML; overridden in tests.
	githubAPI string
	githubWeb string
	// Resolved from GithubTokenFile / $GITHUB_TOKEN.
	githubToken string
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
	orgNameRe  = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,38})$`)
	repoNameRe = regexp.MustCompile(`^[A-Za-z0-9-]{1,39}/[A-Za-z0-9._-]{1,100}$`)
)

// hasSources reports whether anything besides the upstream store is configured.
// Without sources the proxy is a pure pass-through.
func (c *Config) hasSources() bool {
	return len(c.Orgs) > 0 || len(c.ExtraRepos) > 0
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
	if c.Topic == "" {
		c.Topic = defaultTopic
	}
	if c.RefreshInterval.Duration == 0 {
		c.RefreshInterval.Duration = defaultRefreshInterval
	}
	if c.RefreshInterval.Duration < time.Minute {
		return fmt.Errorf("refreshInterval must be at least 1m")
	}
	for _, o := range c.Orgs {
		if !orgNameRe.MatchString(o) {
			return fmt.Errorf("invalid org name %q", o)
		}
	}
	for i, r := range c.ExtraRepos {
		n, err := normalizeRepo(r)
		if err != nil {
			return err
		}
		c.ExtraRepos[i] = n
	}
	if c.CacheDir == "" {
		if d := os.Getenv("CACHE_DIRECTORY"); d != "" { // set by systemd's CacheDirectory=
			c.CacheDir = strings.Split(d, ":")[0]
		} else if d, err := os.UserCacheDir(); err == nil {
			c.CacheDir = filepath.Join(d, "decky-store-proxy")
		}
	}
	if c.GithubTokenFile != "" {
		b, err := os.ReadFile(c.GithubTokenFile)
		if err != nil {
			return fmt.Errorf("reading githubTokenFile: %w", err)
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
		return "", fmt.Errorf("invalid extraRepos entry %q (expected owner/repo or a github.com URL)", s)
	}
	return r, nil
}
