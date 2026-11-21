# decky-store-proxy

> [!CAUTION]
> Every org, user and repo you add as a source can put code on your machine.
> Anyone who can publish a release there can ship a plugin, or an update to
> one, that Decky runs, often as root. Only add sources you trust.

A plugin store for [Decky Loader](https://github.com/SteamDeckHomebrew/decky-loader)
that adds your own plugins, from GitHub releases, to the official store.

It runs on your Steam Deck or SteamOS / NixOS machine, sits between Decky and
the official store, and passes everything through. Plugins found in the
configured GitHub orgs, users and repos show up in the store next to the
official ones, with installs and update notifications working as usual.
Without any sources configured, Decky behaves exactly as with the official store.

## Installation

### NixOS (incl. Jovian-NixOS)

Add the flake input:

```nix
decky-store-proxy.url = "github:paulmiro/decky-store-proxy";
decky-store-proxy.inputs.nixpkgs.follows = "nixpkgs";
```

Enable the module:

```nix
imports = [ inputs.decky-store-proxy.nixosModules.default ];

services.decky-store-proxy = {
  enable = true;
  settings.sources.github = {
    orgs = [ "paulmiro-decky-store" ];
    # users = [ ];
    # repos = [ ];
  };
};
```

`settings` takes every key from [Configuration](#configuration).
`sources.github.tokenFile` may point to a root-only file. It is passed to the
service as a systemd credential.

### SteamOS and other distributions

Build the binary on any machine with Go:

```bash
CGO_ENABLED=0 go build -o decky-store-proxy .
```

Copy it to `~/.local/bin/` on the Deck. Write your configuration to
`~/.config/decky-store-proxy/config.toml`, then create
`~/.config/systemd/user/decky-store-proxy.service`:

```ini
[Unit]
Description=Pull-through Decky Loader plugin store

[Service]
ExecStart=%h/.local/bin/decky-store-proxy
Restart=on-failure

[Install]
WantedBy=default.target
```

Start it, and keep it running outside a desktop session:

```bash
systemctl --user enable --now decky-store-proxy
```

```bash
loginctl enable-linger deck
```

## Setting up Decky

In Decky, open Settings → General → Store Channel, choose **Custom** and enter
`http://127.0.0.1:8737/plugins`. Switch back to *Default* to bypass the proxy.

Requires Decky Loader 3.3.0 or newer.

## Configuration

The configuration is a TOML file. Its default location is
`~/.config/decky-store-proxy/config.toml`; another path can be passed with
`-config <path>`. Unknown keys are an error. See
[`config.example.toml`](config.example.toml).

```toml
[sources.github]
orgs = ["some-org-name"]
users = ["some-username"]
repos = ["other-user/myplugin", "other-org/theplugin"]
topics = ["decky-plugin"]
```

| key | default | |
|---|---|---|
| `listenAddress` | `127.0.0.1` | |
| `port` | `8737` | |
| `refreshInterval` | `15m` | how often sources are checked, at least `1m` |
| `upstream` | `https://plugins.deckbrew.xyz/plugins` | the official store; testing: `https://testing.deckbrew.xyz/plugins` |
| `cacheDir` | `~/.cache/decky-store-proxy` | under systemd: its cache directory |

`[sources.github]`:

| key | default | |
|---|---|---|
| `orgs` | `[]` | organizations whose repos are searched for plugins |
| `users` | `[]` | users whose own repos are searched for plugins |
| `repos` | `[]` | individual repos (`owner/repo` or GitHub URL), searched regardless of topics |
| `topics` | `["decky-plugin"]` | a repo of an org or user is searched if it has at least one of these topics; must not be empty |
| `tokenFile` | unset | file with a GitHub token; falls back to `$GITHUB_TOKEN` |

Without any `orgs`, `users` or `repos`, the proxy only passes requests through
to the official store.

If two sources offer a plugin with the same name, a plugin from `repos` wins
over one from an org or user, which wins over the official store. If two orgs
or users offer the same name, both are skipped.

A token is only needed if GitHub's limit of 60 unauthenticated API requests per
hour is reached. That can happen with many orgs and users or a short
`refreshInterval`. It needs no permissions.

## Troubleshooting

Show the configured sources, the plugins found with their versions, and the
errors of the last refresh:

```bash
curl -s http://127.0.0.1:8737/_proxy/status
```

Check the sources now instead of waiting for `refreshInterval`:

```bash
curl -s -X POST http://127.0.0.1:8737/_proxy/refresh
```

A new release can take about a minute to be picked up, even when refreshing
manually. If a repo or source is removed, its plugins disappear from the store;
copies that are already installed keep working but get no more updates.

## Publishing plugins

See [docs/plugin-development.md](docs/plugin-development.md) for how to
release plugins so the proxy finds them, and [docs/design.md](docs/design.md)
for how the proxy works.

## License

[The Unlicense](UNLICENSE): public domain.
