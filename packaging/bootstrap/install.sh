#!/usr/bin/env bash
# KARMAX one-line installer (Linux & macOS).
#
#   curl -fsSL https://github.com/MelloB1989/KARMAX/releases/latest/download/install.sh | bash
#
# Detects your OS/arch, downloads the matching release archive, and runs its
# installer (which sets up the background service). Override the source repo
# with KARMAX_REPO=owner/repo.
set -euo pipefail

REPO="${KARMAX_REPO:-MelloB1989/KARMAX}"
BASE="https://github.com/$REPO/releases/latest/download"

os="$(uname -s)"
arch="$(uname -m)"
case "$os" in
	Linux)  goos=linux ;;
	Darwin) goos=darwin ;;
	*) echo "unsupported OS: $os — on Windows use install.ps1" >&2; exit 1 ;;
esac
case "$arch" in
	x86_64|amd64)  goarch=amd64 ;;
	aarch64|arm64) goarch=arm64 ;;
	*) echo "unsupported architecture: $arch" >&2; exit 1 ;;
esac

if [ "$goos" = "darwin" ]; then
	asset="karmax_darwin_universal.tar.gz"   # one universal binary for Intel + Apple Silicon
else
	asset="karmax_${goos}_${goarch}.tar.gz"
fi

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

echo "==> downloading $asset"
curl -fSL --proto '=https' --tlsv1.2 "$BASE/$asset" -o "$tmp/karmax.tar.gz"
tar -C "$tmp" -xzf "$tmp/karmax.tar.gz"

echo "==> running installer"
bash "$tmp/karmax/install.sh"
