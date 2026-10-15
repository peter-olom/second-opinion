#!/bin/sh
# Install so from GitHub releases.
#
#   curl -fsSL https://raw.githubusercontent.com/peter-olom/second-opinion/main/install.sh | sh
#
# Environment:
#   SO_VERSION      release tag to install, e.g. v0.2.0 (default: latest)
#   SO_INSTALL_DIR  where to put the binary (default: ~/.local/bin)
#   SO_BASE_URL     download from this URL instead of GitHub releases (mirrors, testing)
set -eu

repo="peter-olom/second-opinion"
version="${SO_VERSION:-latest}"
dir="${SO_INSTALL_DIR:-$HOME/.local/bin}"

fail() {
	echo "so install: $*" >&2
	exit 1
}

os=$(uname -s | tr '[:upper:]' '[:lower:]')
case "$os" in
darwin | linux) ;;
*) fail "unsupported OS '$os'; download a release from https://github.com/$repo/releases" ;;
esac

case "$(uname -m)" in
x86_64 | amd64) arch=amd64 ;;
arm64 | aarch64) arch=arm64 ;;
*) fail "unsupported architecture '$(uname -m)'" ;;
esac

if [ -n "${SO_BASE_URL:-}" ]; then
	base="$SO_BASE_URL"
elif [ "$version" = latest ]; then
	base="https://github.com/$repo/releases/latest/download"
else
	base="https://github.com/$repo/releases/download/$version"
fi
asset="so_${os}_${arch}.tar.gz"

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

echo "downloading $asset ($version)…"
curl -fsSL -o "$tmp/$asset" "$base/$asset" || fail "download failed: $base/$asset"
curl -fsSL -o "$tmp/checksums.txt" "$base/checksums.txt" || fail "download failed: $base/checksums.txt"

expected=$(awk -v f="$asset" '$2 == f { print $1 }' "$tmp/checksums.txt")
[ -n "$expected" ] || fail "no checksum for $asset"
if command -v sha256sum >/dev/null 2>&1; then
	actual=$(sha256sum "$tmp/$asset" | awk '{ print $1 }')
else
	actual=$(shasum -a 256 "$tmp/$asset" | awk '{ print $1 }')
fi
[ "$actual" = "$expected" ] || fail "checksum mismatch for $asset"

tar -xzf "$tmp/$asset" -C "$tmp" so
mkdir -p "$dir"
install -m 0755 "$tmp/so" "$dir/so"
echo "installed $("$dir/so" version) to $dir/so"

case ":$PATH:" in
*":$dir:"*) ;;
*) echo "note: $dir is not on your PATH" ;;
esac
echo
echo "next:"
echo "  so skill install   # teach Claude Code, Codex, and pi to use so"
echo "  then ask your agent for a second opinion; on first use it sets up profiles with you"
