#!/usr/bin/env bash
# Publishes the packaged files. See action.yml for the inputs.
set -euo pipefail
fail() { echo "::error::$*"; exit 1; }
cd "${PLUGIN_DIR:-.}"

case "$RELEASE" in
  create)
    [[ "$TAG" == "v$VERSION" ]] || fail "tag '$TAG' does not match package.json version (expected v$VERSION)"
    prerelease=false
    [[ "$VERSION" == *-* ]] && prerelease=true
    # Only the highest stable version becomes the latest release, so a
    # backport never replaces a newer version in the store.
    highest=$({ git ls-remote --tags --refs origin 'v*' | sed 's#.*refs/tags/v##'; echo "$VERSION"; } \
      | grep -E '^[0-9]+\.[0-9]+\.[0-9]+$' | sort -V | tail -n1 || true)
    latest=false
    [[ "$prerelease" == false && "$VERSION" == "$highest" ]] && latest=true
    args=(--verify-tag --title "$NAME $TAG" --generate-notes "--latest=$latest")
    [[ "$prerelease" == true ]] && args+=(--prerelease)
    gh release create "$TAG" "$ZIP" "$MANIFEST" "${args[@]}"
    ;;
  upload)
    gh release upload "$TAG" "$ZIP" "$MANIFEST" --clobber
    ;;
esac
