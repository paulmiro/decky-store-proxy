#!/usr/bin/env bash
# Packages a built Decky plugin. See action.yml for the inputs.
set -euo pipefail
shopt -s nullglob
fail() { echo "::error::$*"; exit 1; }

case "${RELEASE:-none}" in none | create | upload) ;; *) fail "release must be none, create or upload, got '$RELEASE'" ;; esac

cd "${PLUGIN_DIR:-.}"
[[ -f package.json ]] || fail "package.json missing in '${PLUGIN_DIR:-.}'"
[[ -f plugin.json ]] || fail "plugin.json missing in '${PLUGIN_DIR:-.}'"
version=$(jq -er .version package.json) || fail "package.json has no version"
name=$(jq -er .name plugin.json) || fail "plugin.json has no name"
semver='^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z.-]+)?(\+[0-9A-Za-z.-]+)?$'
[[ "$version" =~ $semver ]] || fail "package.json version '$version' is not SemVer"
[[ -n "${name// /}" && "$name" != */* && "$name" != *\\* ]] || fail "plugin.json name '$name' is invalid"
[[ -f dist/index.js ]] || fail "dist/index.js missing; build the plugin before packaging it"

tag="${TAG:-}"
if [[ -z "$tag" ]]; then
  [[ "${GITHUB_REF_TYPE:-}" == tag ]] || fail "not running on a tag push; set the tag input"
  tag="$GITHUB_REF_NAME"
fi

folder="${FOLDER_NAME:-}"
if [[ -z "$folder" ]]; then
  folder=$(printf '%s' "$name" | tr '[:upper:]' '[:lower:]' | sed -E 's/[^a-z0-9]+/-/g; s/^-+//; s/-+$//')
fi
[[ "$folder" =~ ^[A-Za-z0-9._-]+$ && "$folder" != . && "$folder" != .. ]] || fail "invalid folder name '$folder'; set the folder-name input"

zip_name="${ZIP_NAME:-${GITHUB_REPOSITORY#*/}-decky-plugin.zip}"
[[ "$zip_name" =~ ^[A-Za-z0-9._-]+\.zip$ ]] || fail "invalid zip name '$zip_name'"
artifact="$GITHUB_SERVER_URL/$GITHUB_REPOSITORY/releases/download/$tag/$zip_name"

out="${OUTPUT_DIR:-$RUNNER_TEMP/decky-store}"
mkdir -p "$out"
out=$(cd "$out" && pwd)
manifest="$out/decky-store.json"
[[ -e "$out/$zip_name" ]] && fail "$out/$zip_name already exists; give each plugin its own zip-name"
if [[ -f "$manifest" ]]; then
  jq -e --arg name "$name" 'any(.plugins[]; .name == $name) | not' "$manifest" > /dev/null \
    || fail "$manifest already lists a plugin named '$name'"
  jq -e --arg prefix "${artifact%/*}/" 'all(.plugins[]; .artifact | startswith($prefix))' "$manifest" > /dev/null \
    || fail "$manifest lists plugins of another release than '$tag'"
fi

# Stage in Decky's install layout (as in the Decky plugin template / CLI).
stage=$(mktemp -d)
trap 'rm -rf "$stage"' EXIT
dest="$stage/$folder"
mkdir -p "$dest"
cp -r dist package.json plugin.json "$dest/"
for f in main.py README* LICENSE* LICENCE*; do
  if [[ -e "$f" ]]; then cp -r "$f" "$dest/"; fi
done
if [[ -d py_modules && -n "$(ls -A py_modules)" ]]; then cp -r py_modules "$dest/"; fi
if [[ -d defaults ]]; then cp -r defaults/. "$dest/"; fi
if [[ -d backend/out ]]; then mkdir -p "$dest/bin" && cp -r backend/out/. "$dest/bin/"; fi
for f in ${EXTRA_FILES:-}; do cp -r "$f" "$dest/"; done

# Plugins must also work on NixOS, which has no dynamic loader at /lib64 and
# no bash or python3 on Decky's PATH. See docs/plugin-development.md.
failed=0
problem() { echo "::error title=NixOS compatibility::$1: $2"; failed=1; }
while IFS= read -r -d '' f; do
  rel="${f#"$stage"/}"
  if [[ "$(head -c 4 "$f" | od -An -tx1 | tr -d ' \n')" == 7f454c46 ]]; then
    # ELF: must not need a dynamic loader or shared libraries.
    if readelf -lW "$f" 2>/dev/null | grep -q 'Requesting program interpreter'; then
      problem "$rel" "dynamically linked executable; build it statically"
    elif readelf -dW "$f" 2>/dev/null | grep -q '(NEEDED)'; then
      problem "$rel" "depends on shared libraries ($(readelf -dW "$f" | sed -n 's/.*Shared library: \[\(.*\)\]/\1/p' | paste -sd, -)); build it statically, and don't ship compiled Python modules"
    fi
  elif [[ "$(head -c 2 "$f")" == '#!' && "$f" != *.py ]]; then
    # Scripts: only POSIX sh is guaranteed to exist.
    read -r -a cmd <<< "$(head -n 1 "$f" | cut -c3-)"
    interp="${cmd[0]:-}"
    if [[ "$interp" == /usr/bin/env ]]; then
      interp="${cmd[1]:-}"
      [[ "$interp" == -S ]] && interp="${cmd[2]:-}"
    fi
    if [[ "$interp" != /bin/sh && "$interp" != sh ]]; then
      problem "$rel" "uses interpreter '$(head -n 1 "$f" | cut -c3-)'; use #!/bin/sh (POSIX sh)"
    fi
  fi
done < <(find "$dest" -type f -print0)
if [[ "$failed" == 1 ]]; then
  fail "'$name' would not work on NixOS. See https://github.com/paulmiro/decky-store-proxy/blob/main/docs/plugin-development.md#working-on-nixos"
fi

# Reproducible zip: sorted entries, mtimes set to the time of the current commit.
epoch=$(git log -1 --format=%ct)
zip="$out/$zip_name"
(cd "$stage" && find "$folder" -exec touch -d "@$epoch" {} + \
  && find "$folder" -print | LC_ALL=C sort | zip -qX "$zip" -@)
unzip -l "$zip"
sha256=$(sha256sum "$zip" | cut -d' ' -f1)

entry=$(jq -n \
  --arg name "$name" \
  --arg version "$version" \
  --arg artifact "$artifact" \
  --arg sha256 "$sha256" \
  --arg created "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
  --slurpfile plugin plugin.json \
  '$plugin[0] as $p | {
    name: $name,
    version: $version,
    artifact: $artifact,
    sha256: $sha256,
    author: ($p.author // ""),
    description: ($p.publish.description // ""),
    tags: ($p.publish.tags // []),
    image_url: ($p.publish.image // ""),
    created: $created
  }')
if [[ -f "$manifest" ]]; then
  jq --argjson e "$entry" '.plugins += [$e]' "$manifest" > "$manifest.tmp"
else
  jq -n --argjson e "$entry" '{schema: 2, plugins: [$e]}' > "$manifest.tmp"
fi
mv "$manifest.tmp" "$manifest"
cat "$manifest"

{
  echo "name=$name"
  echo "version=$version"
  echo "tag=$tag"
  echo "zip=$zip"
  echo "manifest=$manifest"
  echo "output-dir=$out"
} >> "$GITHUB_OUTPUT"
echo "Packaged '$name' $version as $zip_name for release $tag"
