#!/usr/bin/env bash
# One-line installer for whpaper (GitHub Releases, no root needed).
#
#   curl -fsSL https://raw.githubusercontent.com/Become-ILLUSORY/whpaper/main/scripts/install.sh | bash
#
# Env: REPO=owner/name  VERSION=v1.0.0  PREFIX=~/.local/bin
set -euo pipefail

REPO="${REPO:-Become-ILLUSORY/whpaper}"
VERSION="${VERSION:-latest}"
PREFIX="${PREFIX:-$HOME/.local/bin}"

os="$(uname -s | tr '[:upper:]' '[:lower:]')"
case "$os" in
  linux) os=linux ;;
  darwin) os=darwin ;;
  *) echo "unsupported OS: $os" >&2; exit 1 ;;
esac
arch="$(uname -m)"
case "$arch" in
  x86_64 | amd64) arch=amd64 ;;
  aarch64 | arm64) arch=arm64 ;;
  *) echo "unsupported arch: $arch" >&2; exit 1 ;;
esac

if [ "$VERSION" = "latest" ]; then
  VERSION="$(curl -fsSL "https://api.github.com/repos/$REPO/releases/latest" | sed -n 's/.*"tag_name": *"\([^"]*\)".*/\1/p' | head -1)"
  [ -n "$VERSION" ] || { echo "could not resolve the latest release tag" >&2; exit 1; }
fi

base="https://github.com/$REPO/releases/download/$VERSION"
asset="whpaper-$VERSION-$os-$arch.tar.gz"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

echo "downloading $asset"
curl -fsSL "$base/$asset" -o "$tmp/$asset"

if curl -fsSL "$base/checksums.txt" -o "$tmp/checksums.txt" 2>/dev/null; then
  want="$(grep " $asset\$" "$tmp/checksums.txt" | awk '{print $1}' || true)"
  if [ -n "$want" ]; then
    got="$(sha256sum "$tmp/$asset" | awk '{print $1}')"
    [ "$want" = "$got" ] || { echo "checksum mismatch: want $want got $got" >&2; exit 1; }
    echo "checksum ok"
  fi
fi

tar xzf "$tmp/$asset" -C "$tmp"
bin="$(find "$tmp" -maxdepth 1 -type f -name 'whpaper-*' | head -1)"
[ -n "$bin" ] || { echo "archive did not contain a binary" >&2; exit 1; }

mkdir -p "$PREFIX"
mv "$bin" "$PREFIX/whpaper"
chmod +x "$PREFIX/whpaper"
echo "installed $PREFIX/whpaper"
"$PREFIX/whpaper" -v

case ":$PATH:" in
  *":$PREFIX:"*) ;;
  *) echo; echo "note: $PREFIX is not in PATH — add this to your shell profile:"
     echo "  export PATH=\"$PREFIX:\$PATH\"" ;;
esac
