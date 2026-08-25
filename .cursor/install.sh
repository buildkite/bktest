#!/usr/bin/env bash
#
# Idempotent per-project dependency install for the Buildkite test
# collectors monorepo. Language toolchains themselves (Ruby, Erlang/Elixir,
# .NET, Swift, the Android SDK, uv, Node, Rust, JDK 17) live in the base
# environment; this script only refreshes project dependencies after the
# source is checked out. Each project is guarded on its directory existing
# because collectors arrive on main incrementally and dry-run branches may
# contain only a subset.
set -euo pipefail

# Toolchain PATH / env. Sourced from the baked-in profile when present, with
# explicit fallbacks so the script is self-contained under any shell.
if [ -f /etc/profile.d/zz-cloud-dev-env.sh ]; then
  # shellcheck disable=SC1091
  source /etc/profile.d/zz-cloud-dev-env.sh
fi
export PATH="$HOME/.local/bin:$HOME/.local/share/mise/shims:$PATH"
export DOTNET_ROOT="${DOTNET_ROOT:-/opt/dotnet}"
export PATH="$PATH:/opt/dotnet"
export SWIFTLY_HOME_DIR="${SWIFTLY_HOME_DIR:-/opt/swiftly/share}"
export SWIFTLY_BIN_DIR="${SWIFTLY_BIN_DIR:-/opt/swiftly/bin}"
export PATH="/opt/swiftly/bin:$PATH"
export ANDROID_HOME="${ANDROID_HOME:-/opt/android-sdk}"
export ANDROID_SDK_ROOT="$ANDROID_HOME"
export JAVA_HOME="${JAVA_HOME:-/usr/lib/jvm/java-17-openjdk-amd64}"

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root"

run() { echo "+ ($1) ${*:2}"; ( cd "$1" && shift && "$@" ); }

# Rust
if [ -d test-collector-rust ]; then
  run test-collector-rust cargo fetch
fi

# JavaScript (browsers/system deps already provisioned in the base image)
if [ -d test-collector-javascript ]; then
  run test-collector-javascript npm ci
  run test-collector-javascript npx playwright install chromium
fi

# Python
if [ -d test-collector-python ]; then
  run test-collector-python uv sync --all-extras
fi

# Ruby
if [ -d test-collector-ruby ]; then
  run test-collector-ruby gem install bundler:2.3.25 --conservative --no-document
  run test-collector-ruby bundle install
fi

# Elixir
if [ -d test_collector_elixir ]; then
  run test_collector_elixir mix local.hex --force
  run test_collector_elixir mix local.rebar --force
  run test_collector_elixir mix deps.get
fi

# .NET
if [ -d test-collector-dotnet ]; then
  run test-collector-dotnet dotnet restore
fi

# Swift
if [ -d test-collector-swift ]; then
  run test-collector-swift swift package resolve
fi

# Android (JVM only; warms Gradle + resolves the core module's dependencies)
if [ -d test-collector-android ]; then
  run test-collector-android ./gradlew --no-daemon :collector:test-data-uploader:dependencies -q
fi

echo "install.sh completed successfully"
