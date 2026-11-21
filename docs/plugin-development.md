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
5. Work on NixOS as well as SteamOS. See [Working on NixOS](#working-on-nixos).

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
- fails if the zip contains anything that can't work on NixOS (see
  [Checked by the workflow](#checked-by-the-workflow));
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

## Working on NixOS

Plugins from the proxy must work on NixOS ([Jovian-NixOS](https://github.com/Jovian-Experiments/Jovian-NixOS))
as well as on SteamOS. They must work with Jovian's **default**
`jovian.decky-loader` settings: a plugin can't expect users to add packages or
change options for it. The frontend runs in the Steam client and behaves the
same on both systems. The differences all concern the Python backend and
anything it runs.

### The environment on Jovian-NixOS

| | SteamOS | Jovian-NixOS (defaults) |
|---|---|---|
| Decky | release binary (PyInstaller) | built from source as a NixOS service |
| Python | bundled with Decky (3.14 as of Decky 3.3) | nixpkgs' `python3` (3.14 on unstable, older on stable NixOS releases) |
| plugin directory | `/home/deck/homebrew/plugins` | `/var/lib/decky-loader/plugins` |
| backend user | `deck` (`root` with the `_root` flag) | `decky`, a system user (`root` with the `_root` flag) |
| `DECKY_USER` / `DECKY_USER_HOME` | `deck` / `/home/deck` | `decky` / `/var/lib/decky-loader` |
| `DECKY_HOME` | `/home/deck/homebrew` | `/var/lib/decky-loader` |
| `PATH` | the full SteamOS system | coreutils, findutils, grep, sed, systemd and psmisc **only** |
| FHS paths | `/usr/bin`, `/usr/lib`, `/lib64`, … | only `/bin/sh` and `/usr/bin/env` |

### Paths

- Never hardcode `/home/deck`, `~/homebrew` or any other path. Use the
  variables of the `decky` module: `DECKY_PLUGIN_DIR`,
  `DECKY_PLUGIN_SETTINGS_DIR`, `DECKY_PLUGIN_RUNTIME_DIR`,
  `DECKY_PLUGIN_LOG_DIR`.
- `DECKY_USER_HOME` is **not** the Steam user's home on Jovian. The Steam user
  is configured separately, and their home is only readable by them
  (mode 700). To find Steam's files, look up the user that runs Steam, for
  example from the `steam` process:

  ```python
  import os, pwd

  def steam_user_home() -> str | None:
      for pid in filter(str.isdigit, os.listdir("/proc")):
          try:
              with open(f"/proc/{pid}/comm") as f:
                  if f.read().strip() != "steam":
                      continue
              return pwd.getpwuid(os.stat(f"/proc/{pid}").st_uid).pw_dir
          except (OSError, KeyError):
              continue
      return None
  ```

  Reading files in that home requires the `_root` flag on Jovian.
- Don't write to `/etc`, `/usr` or other system locations. On NixOS they are
  generated from the system configuration and read-only (or overwritten on the
  next rebuild). Keep all state in the plugin's settings, runtime and log
  directories.
- Don't install systemd units or udev rules. Run helper processes from the
  backend instead, or, as root, as transient units with `systemd-run`.

### Python

- Only the standard library and Decky's own dependencies are guaranteed:
  `aiohttp`, `aiohttp-cors`, `aiohttp-jinja2`, `certifi`, `multidict`,
  `packaging`, `setproctitle`, `watchdog` and what they depend on.
- Ship everything else in `py_modules/`, which Decky adds to `sys.path`.
  Only use **pure-Python** packages there. Compiled extension modules (`.so`
  files) are built for one Python version and expect a regular Linux system;
  the Python version differs between SteamOS and NixOS, and also between
  NixOS releases.
- Don't use syntax or standard library features newer than the oldest Python
  you want to support. NixOS 25.11's `python3` is 3.13.

### Binaries and scripts

- Every executable you ship in `bin/` or download as a `remote_binary` must be
  **statically linked**. Dynamically linked programs fail on NixOS with
  "No such file or directory", because the dynamic loader
  (`/lib64/ld-linux-x86-64.so.2`) doesn't exist there. The same applies to
  AppImages.
  - Go: `CGO_ENABLED=0 go build`
  - Rust: `cargo build --release --target x86_64-unknown-linux-musl`
  - C/C++: build against musl with `-static` (e.g. `musl-gcc -static` or
    `zig cc -target x86_64-linux-musl`)
  - Check with `file <binary>` ("statically linked") or `ldd <binary>`
    ("not a dynamic executable").
- Shell scripts must start with `#!/bin/sh` (or `#!/usr/bin/env sh`) and be
  POSIX sh. `/bin/bash` doesn't exist, and neither `bash` nor `python3` is on
  Decky's `PATH`, so `#!/usr/bin/env bash` and `#!/usr/bin/env python3` fail
  too.
- Don't call system tools that aren't in the `PATH` above: no `curl`,
  `wget`, `unzip`, `tar`, `flatpak`, `pactl`, `steamos-*` helpers and so on.
  Prefer the standard library (`urllib`, `zipfile`, `tarfile`, `shutil`,
  `subprocess` with your own static binary) or `aiohttp`. If a tool is optional,
  check for it with `shutil.which()` and degrade with a clear message.
- Call executables by name (resolved through `PATH`) or by their path inside
  the plugin directory, never by an absolute system path such as `/usr/bin/…`.

### Checked by the workflow

The release workflow inspects every file in the zip and fails the release if
it finds:

- an ELF executable that requests a dynamic loader, or any ELF file (including
  compiled Python modules) that depends on shared libraries;
- a script whose interpreter is not `/bin/sh` or `sh` via `/usr/bin/env`.
  Files ending in `.py` are exempt, because Decky imports them rather than
  running them.

Everything else in this section can't be checked automatically.

### Testing

Test every release on a Jovian-NixOS machine with only
`jovian.decky-loader.enable = true`, in addition to SteamOS. Check the backend
log (`DECKY_PLUGIN_LOG_DIR`, or `journalctl -u decky-loader`) for errors.

## Securing an org

Anyone who can publish a release in a configured source can ship code that
Decky runs on users' machines, often as root. For an org that serves plugins:

- enable **immutable releases**, so a replaced asset can't break the hash or
  swap out a plugin;
- add tag rulesets so only maintainers can push `v*` tags;
- protect the default branch and require reviews;
- keep membership small.
