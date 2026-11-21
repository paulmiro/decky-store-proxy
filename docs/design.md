# How decky-store-proxy works

```
Decky (Steam UI) ──► decky-store-proxy (127.0.0.1:8737) ──► plugins.deckbrew.xyz
                              │
                              └─► GitHub: org/user repo lists + decky-store.json per repo
```

## Decky's store protocol

The Decky store base URL (here `http://127.0.0.1:8737/plugins`) serves:

| request | made by | |
|---|---|---|
| `GET /plugins?sort_by=…&sort_direction=…` | Steam UI | plugin list |
| `GET /plugins/versions` | Steam UI | latest version per plugin, for update notifications |
| `POST /plugins/{name}/versions/{version}/increment` | Decky's backend | install counter |

Decky downloads a version's `artifact` URL and checks its `sha256`.

## Pass-through

Requests are forwarded to `upstream` with path, query, body and status
unchanged. If no custom plugins are known, every request is passed through.

The Steam UI fetches from `https://steamloopback.host`, so responses need CORS
headers. The proxy answers preflight requests itself and sets the same policy
as the official store (that origin only, with credentials). It also answers
Private Network Access preflights, which the current Steam client does not send.

## Discovery

Every `refreshInterval`, and on `POST /_proxy/refresh`:

1. The public, non-archived repos of each configured org (`/orgs/{org}/repos`)
   and user (`/users/{user}/repos?type=owner`) with at least one configured
   topic are listed through the GitHub API, using ETags. Repos from `repos`
   are added without the topic check.
2. For each repo, `https://github.com/<repo>/releases/latest/download/decky-store.json`
   is downloaded. This is a normal download, not an API call. Every plugin in
   it is offered. Missing or invalid manifests are logged and the repo is
   skipped.
3. Decky's store card looks up the installed version in the plugin's version
   list; if it isn't there, an update is labelled as a downgrade. So the
   proxy keeps a record of each repo's stable releases with a
   `decky-store.json`, and builds every plugin's version list from them,
   matched by plugin name. The release list (`/repos/{repo}/releases`) is
   fetched through the API only when the latest release has a plugin version
   that wasn't there before. A release that only rebuilt the same versions, or
   an unchanged repo, costs no API calls.
   - The first fetch reads all pages of the release list. Later fetches stop
     at the first page with an already known release, and keep the older
     records. Known releases on the fetched pages that are no longer listed
     are dropped.
   - Each release's manifest is downloaded once, and again only if its asset
     is replaced.
4. Name conflicts are resolved: `repos` beat org and user repos; two org/user
   repos with a plugin of the same name are both dropped.

Failures don't empty the store:

- If listing an org or user fails, its previously found plugins are kept.
- If a manifest can't be downloaded, the repo's previous plugins are kept.
- If the release list can't be fetched, the known versions are kept and the
  fetch is retried on the next refresh.
- The state of every repo, including its release records, is saved to
  `cacheDir/catalog.json` and loaded on start, so the store also works across
  restarts and while GitHub is unreachable, and no manifest is downloaded
  twice. Cached repos whose source is no longer configured are dropped.

## Merging

- `GET /plugins`: upstream plugins with the same name as a custom plugin are
  removed. Custom plugins are inserted into the upstream list according to
  `sort_by` (`name`, `date` or `downloads`) and `sort_direction`, without
  reordering upstream entries. They get stable negative ids, derived from the
  repo and the plugin name. The `query` and
  `tags` filters are applied to custom plugins too. If upstream is down, only
  custom plugins are listed; upstream 4xx responses (e.g. 426 for old Decky
  versions) are passed through.
- `GET /plugins/versions`: custom plugins are added as
  `[version, sha256, artifact]`.
- Install counter requests for custom plugins are answered locally, so they
  don't count installs for an unrelated upstream plugin of the same name.

## Development

```bash
go test -race ./...
```

```bash
nix flake check
```

`nix develop` provides Go and gopls. The flake's `checks.<system>.module`
builds the systemd unit from an example NixOS configuration.

The packaging action lives in `actions/package-plugin/`: `package.sh` builds
the zip and `decky-store.json`, `release.sh` publishes them. CI runs it on
fixture plugins and validates the result with the proxy's manifest checks.

`plugin/` is the *Store Proxy* Decky plugin. Its Python backend sends
`POST /_proxy/refresh` to the URL from its settings. To release it, set the
same version in `plugin/package.json` and `nix/package.nix`, commit, and push
the tag `v<version>`.
