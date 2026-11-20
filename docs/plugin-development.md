# Publishing plugins for decky-store-proxy

The proxy offers a plugin if its GitHub repository publishes releases in the
format below. The reusable release workflow in this repository produces that
format, so normally all a plugin needs is a small workflow file.

## Requirements

A plugin repository must:

1. Be **public**. Decky downloads the plugin zip without credentials.
2. Be reachable through a source in the user's configuration:
   - owned by a configured org or user **and** tagged with one of the
     configured topics (by default `decky-plugin`), or
   - listed explicitly in `repos` (no topic needed).
3. Have `package.json` and `plugin.json` at the root, as in the
   [Decky plugin template](https://github.com/SteamDeckHomebrew/decky-plugin-template):
   - `package.json` `version` is the plugin version, in strict SemVer
     (`1.2.3`, no leading `v`).
   - `plugin.json` `name` is the plugin's identity. Decky matches installed
     plugins to store entries by this name, so it must never change.
4. Release through the reusable workflow (or produce the same assets).

## Releasing

Add `.github/workflows/release.yml`:

```yaml
name: Release

on:
  push:
    tags: ["v*"]

permissions:
  contents: write

jobs:
  release:
    uses: paulmiro/decky-store-proxy/.github/workflows/release-plugin.yml@<commit-sha>
```

Pinning a commit SHA is recommended; `@main` also works.

To release a version, bump `version` in `package.json`, commit, then tag and
push:

```bash
git tag v1.2.3 && git push origin main v1.2.3
```

The workflow:

- fails unless the tag equals `v` + `package.json` version;
- builds the frontend with `pnpm install --frozen-lockfile && pnpm run build`;
- packages `<repo>.zip` in Decky's install layout, reproducibly;
- writes `decky-store.json`;
- creates the GitHub release with both files.

Inputs:

| input | default | |
|---|---|---|
| `build-command` | `pnpm install --frozen-lockfile && pnpm run build` | must produce `dist/index.js` |
| `node-version` | `22` | |
| `extra-files` | `""` | space-separated extra files or directories to include |

The zip contains `dist/`, `package.json`, `plugin.json` and, if present,
`main.py`, `README*`, `LICENSE*`, `py_modules/`, the contents of `defaults/`, and
`backend/out/` as `bin/`.

Prerelease tags such as `v1.3.0-rc.1` become GitHub prereleases. A backport
released after a newer version (for example `v1.2.4` after `v1.3.0`) is not
marked as the latest release. Neither of them reaches the store as the current
version.

## Release format

Each release has two assets:

- `<repo>.zip`: the plugin, in Decky's install layout (a single top-level
  folder containing `plugin.json`).
- `decky-store.json`, schema 1:

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

| field | |
|---|---|
| `name` | equal to `plugin.json` `name` |
| `version` | strict SemVer, equal to `package.json` `version` |
| `artifact` | `https://github.com/.../releases/download/...` URL of the zip |
| `sha256` | checked by Decky before installing |
| `author` | from `plugin.json` `author` |
| `description`, `tags`, `image_url` | from `plugin.json` `publish.description`, `publish.tags`, `publish.image`; `image_url` must be `https` if set |
| `created` | release time, used when the store is sorted by date |

The store shows the latest release as the current version. All other stable
releases with a `decky-store.json` (from the newest 100 releases) are offered
as older versions; prereleases and releases without a valid manifest are
ignored. If the latest release has no valid manifest, the plugin is not offered
at all, and the proxy reports the problem in `/_proxy/status`.

Plugins from third parties that don't publish this format can be forked and
released through the workflow.

## Securing an org

Anyone who can publish a release in a configured source can ship code that
Decky runs on users' machines, often as root. For an org that serves plugins:

- enable **immutable releases**, so a replaced asset can't break the hash or
  swap out a plugin;
- add tag rulesets so only maintainers can push `v*` tags;
- protect the default branch and require reviews;
- keep membership small.
