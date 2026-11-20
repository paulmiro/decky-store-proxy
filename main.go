// decky-store-proxy is a pull-through Decky Loader plugin store.
//
// It forwards everything to the official store and, if configured, injects
// plugins discovered from GitHub releases (see README.md).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

var version = "dev" // set via -ldflags

var userAgent = "decky-store-proxy/" + version

func main() {
	configPath := flag.String("config", "", "path to config.toml (default: $XDG_CONFIG_HOME/decky-store-proxy/config.toml, optional)")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println(version)
		return
	}
	if err := run(*configPath); err != nil {
		slog.Error(err.Error())
		os.Exit(1)
	}
}

func run(configPath string) error {
	cfg, err := loadConfig(configPath)
	if err != nil {
		return err
	}
	client := &http.Client{Timeout: 30 * time.Second}
	disc := newDiscoverer(cfg, client)
	srv, err := newServer(cfg, disc, client)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if cfg.hasSources() {
		disc.loadCache()
		go func() {
			disc.Refresh(ctx)
			t := time.NewTicker(cfg.RefreshInterval.Duration)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
					disc.Refresh(ctx)
				}
			}
		}()
	}

	hs := &http.Server{
		Addr:              cfg.addr(),
		Handler:           srv,
		ReadHeaderTimeout: 10 * time.Second,
	}
	errc := make(chan error, 1)
	go func() { errc <- hs.ListenAndServe() }()
	slog.Info("listening", "url", fmt.Sprintf("http://%s%s", cfg.addr(), basePath),
		"upstream", cfg.Upstream, "orgs", cfg.Orgs, "extraRepos", cfg.ExtraRepos,
		"passThrough", !cfg.hasSources(), "cacheDir", cfg.CacheDir)

	select {
	case err := <-errc:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("listening on %s: %w", cfg.addr(), err)
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return hs.Shutdown(shutdownCtx)
	}
}
