# decky-store-proxy

A pull-through plugin store for [Decky Loader](https://github.com/SteamDeckHomebrew/decky-loader).
It sits between Decky and the official store and adds your own plugins,
discovered automatically from GitHub releases. With no sources configured
it is a pure pass-through: Decky behaves exactly as with the official store.

```
Decky (Steam UI) ──► decky-store-proxy (127.0.0.1:8737) ──► plugins.deckbrew.xyz
                              │
                              └─► GitHub: org/user repo lists + decky-store.json per repo
```

## How it works

* Requests go to `upstream` unchanged (path, query, body, status).
* Each `refreshInterval`, the proxy lists the public, non-archived repos of each
  configured org and user that carry at least one of the configured topics,
  adds every explicitly listed repo, and downloads
  `https://github.com/<repo>/releases/latest/download/decky-store.json`.
  That is a plain download, not an API call. The API is only used to list
  each org's and user's repos (with ETags), so 60 requests/hour
  unauthenticated is plenty for a few owners.
* When a repo's latest release changes, the proxy also lists its releases (one
  API call) to offer every stable version. Decky needs the installed version
  in that list to label an install as an update rather than a downgrade.
  Unchanged repos cost no API calls.
* Those plugins are merged into `GET /plugins` (respecting `sort_by` /
  `sort_direction`) and `GET /plugins/versions`, so installs **and update
  notifications** work. Install counter POSTs for them are answered locally.
* Decky downloads the zip straight from the GitHub release and checks its
  sha256 against the manifest.
* CORS: the Steam UI (`https://steamloopback.host`) needs CORS headers. The
  proxy answers preflights itself and sets the same policy as the upstream store.

Robustness:
* If GitHub fails, the previous data for that org, user or repo is kept.
* The last good catalog is saved to `cacheDir`, so the store works across restarts and while offline.
* If upstream is down, your own plugins are still listed.

Precedence for duplicate plugin names: explicit `repos` > org and user repos >
upstream. Two org/user repos claiming the same name are both skipped and logged.

Debug endpoints:

```bash
curl -s http://127.0.0.1:8737/_proxy/status
```

It shows the configured sources (and whether a GitHub token is in use), the
discovered plugins with their versions and the source that found them
(`github:org:<name>`, `github:user:<name>` or `github:repo`), and the errors
of the last refresh. `POST /_proxy/refresh` refreshes now and returns the same.

```bash
curl -s -X POST http://127.0.0.1:8737/_proxy/refresh
```

## Decky setup

Decky → Settings → General → Store Channel → **Custom**, URL
`http://127.0.0.1:8737/plugins`. Switch back to *Default* to bypass the proxy.
Requires Decky Loader ≥ 3.3.0, the upstream store's minimum.

## Configuration

TOML, see [`config.example.toml`](config.example.toml). Default path
`~/.config/decky-store-proxy/config.toml`, or pass `-config <path>`. Unknown keys are an error.

```toml
port = 8737

[sources.github]
orgs = ["some-org-name"]
users = ["some-username"]
repos = ["other-user/myplugin", "other-org/theplugin"]
topics = ["decky-plugin"]
```

Top level:

| key | default | |
|---|---|---|
| `listenAddress` | `127.0.0.1` | |
| `port` | `8737` | |
| `refreshInterval` | `15m` | Go duration, ≥ 1m |
| `upstream` | `https://plugins.deckbrew.xyz/plugins` | the official store |
| `cacheDir` | `$CACHE_DIRECTORY` or `~/.cache/decky-store-proxy` | |

`[sources.github]`:

| key | default | |
|---|---|---|
| `orgs` | `[]` | organizations to scan |
| `users` | `[]` | users to scan (only repos they own) |
| `repos` | `[]` | `owner/repo` or GitHub URLs, always included, no topic needed |
| `topics` | `["decky-plugin"]` | an org/user repo needs **at least one**; `[]` is an error |
| `tokenFile` | unset | GitHub token file; falls back to `$GITHUB_TOKEN` |

The proxy is a pure pass-through when `orgs`, `users` and `repos` are all empty.

## NixOS (incl. Jovian-NixOS)

```nix
# flake.nix inputs
decky-store-proxy.url = "github:paulmiro/decky-store-proxy";
decky-store-proxy.inputs.nixpkgs.follows = "nixpkgs";
```

```nix
# configuration
imports = [ inputs.decky-store-proxy.nixosModules.default ];
services.decky-store-proxy = {
  enable = true;
  settings = {
    sources.github = {
      orgs = [ "paulmiro-decky-store" ];
      # users = [ ]; repos = [ ]; topics = [ "decky-plugin" ];
      # tokenFile = "/run/secrets/github-token";
    };
    # port = 8737; listenAddress = "127.0.0.1";
    # any other config key works too, e.g. refreshInterval = "30m";
  };
};
```

The service runs with `DynamicUser` and is sandboxed. A `sources.github.tokenFile`
may be root-only: it's handed over as a systemd credential.

Flake outputs:
* `packages.<system>.{default,decky-store-proxy}`
* `nixosModules.{default,decky-store-proxy}`
* `overlays.default`
* `checks.<system>.module`
* `devShells.<system>.default`

## SteamOS / other distros

Build a static binary: `CGO_ENABLED=0 go build -o decky-store-proxy .`.
Then copy it to `~/.local/bin/` on the Deck and create
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

```bash
systemctl --user enable --now decky-store-proxy
```

```bash
loginctl enable-linger deck
```

The second command keeps the service running outside a desktop session.

## Publishing plugins

Every plugin repo:

1. Is public, owned by a configured org or user, with a configured topic
   (default `decky-plugin`). Repos listed in `repos` don't need the topic.
2. Has `package.json` (`version`, strict SemVer) and `plugin.json` (`name`)
   at the root. The plugin template's layout works as-is.
3. Releases via the reusable workflow in this repo:

   ```yaml
   # .github/workflows/release.yml
   on:
     push:
       tags: ["v*"]
   permissions:
     contents: write
   jobs:
     release:
       uses: paulmiro/decky-store-proxy/.github/workflows/release-plugin.yml@<commit-sha>
   ```

To release: bump `version` in `package.json`, commit, then `git tag vX.Y.Z && git push --tags`.

The workflow:
* fails unless the tag equals `v` + `package.json` version;
* builds with `pnpm install --frozen-lockfile && pnpm run build` (the `build-command` input changes this);
* packages `<repo>.zip` in Decky's layout, reproducibly;
* writes `decky-store.json`;
* creates the release.

Prerelease tags (`v1.2.0-rc.1`) and backports never become "Latest", so
they never reach the store.

### Release contract (`decky-store.json`, schema 1)

```json
{
  "schema": 1,
  "name": "My Plugin",
  "version": "1.4.0",
  "artifact": "https://github.com/org/my-plugin/releases/download/v1.4.0/my-plugin.zip",
  "sha256": "<lowercase hex sha256 of the zip>",
  "author": "me",
  "description": "",
  "tags": [],
  "image_url": "",
  "created": "2026-01-01T00:00:00Z"
}
```

`name` must equal `plugin.json`'s name and never change. Decky matches
installed plugins to store entries by name.

Recommended org settings:
* enable **immutable releases**, so a replaced asset can't break the hash;
* add tag rulesets so only maintainers can push `v*` tags;
* keep membership small: anyone who can push a tag can ship code that Decky runs, possibly as root.

## Limitations

* Only repos with a published release containing `decky-store.json` are shown.
  Third-party plugins without one must be forked and released through the workflow.
* Version history covers the newest 100 releases. Releases made without the
  workflow (no `decky-store.json`) and prereleases are not offered.
* Plugin repos must be public, because Decky downloads the zip without credentials.
* New releases appear after at most `refreshInterval`, or immediately with `POST /_proxy/refresh`.
  GitHub's "latest release" download link can lag a release by about a minute.
* Removing a repo hides it from the store. Installed copies stay, without updates.

## License

[The Unlicense](UNLICENSE): public domain.
