#!/usr/bin/env bash
set -euo pipefail

[[ "$VERSION" =~ ^v0\.0\.0-atty\.[1-9][0-9]*$ ]]
release="$(gh api repos/atty303/devsy/releases/latest)"
jq -e --arg version "$VERSION" '.tag_name == $version and .draft == false and .prerelease == false' <<<"$release"

# Only this disposable verification bypasses mise's default 24-hour release delay.
export MISE_MINIMUM_RELEASE_AGE=0s
export MISE_DATA_DIR="$RUNNER_TEMP/fork-mise/data"
export MISE_CACHE_DIR="$RUNNER_TEMP/fork-mise/cache"
export MISE_CONFIG_DIR="$RUNNER_TEMP/fork-mise/config"
export MISE_GLOBAL_CONFIG_FILE="$RUNNER_TEMP/fork-mise/global.toml"
mkdir -p "$RUNNER_TEMP/fork-mise"
cd "$RUNNER_TEMP/fork-mise"
actual="$(mise latest github:atty303/devsy)"
test "${actual#v}" = "${VERSION#v}"
mise exec github:atty303/devsy@latest -- devsy --version | grep -F "$VERSION"
