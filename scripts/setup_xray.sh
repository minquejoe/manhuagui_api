#!/usr/bin/env bash
# setup_xray.sh - download the Xray-core binary used by manhuagui_api's proxy feature.
#
# Usage:
#   scripts/setup_xray.sh [version]     # e.g. scripts/setup_xray.sh v26.3.27
#   XRAY_VERSION=v26.3.27 scripts/setup_xray.sh
#
# The binary is installed to ./bin/xray (configurable via the first argument
# of the script: scripts/setup_xray.sh <version> <output-dir>).
set -euo pipefail

VERSION="${XRAY_VERSION:-${1:-}}"
OUT_DIR="${2:-./bin}"

# Map kernel/arch to the xray release asset suffix.
case "$(uname -s)-$(uname -m)" in
  Linux-x86_64)  SUFFIX="64" ;;
  Linux-i386|Linux-i686) SUFFIX="32" ;;
  Linux-aarch64) SUFFIX="arm64-v8a" ;;
  Linux-armv7l|Linux-armv6l) SUFFIX="arm32-v7a" ;;
  Linux-riscv64) SUFFIX="riscv64" ;;
  Linux-loong64) SUFFIX="loong64" ;;
  Darwin-x86_64) SUFFIX="macos-64" ;;
  Darwin-arm64)  SUFFIX="macos-arm64-v8a" ;;
  *)
    echo "error: unsupported platform $(uname -s)-$(uname -m)" >&2
    exit 1
    ;;
esac

if [ -z "$VERSION" ]; then
  echo "resolving latest xray version from GitHub API ..." >&2
  VERSION="$(curl -fsSL --max-time 30 https://api.github.com/repos/XTLS/Xray-core/releases/latest \
    | sed -n 's/.*"tag_name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' | head -n1)"
fi
if [ -z "$VERSION" ]; then
  echo "error: failed to resolve xray version" >&2
  exit 1
fi

URL="https://github.com/XTLS/Xray-core/releases/download/${VERSION}/Xray-linux-${SUFFIX}.zip"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

echo "downloading ${URL} ..." >&2
curl -fsSL --max-time 600 -o "$TMP/xray.zip" "$URL"

echo "extracting to ${OUT_DIR}/ ..." >&2
mkdir -p "$OUT_DIR"
unzip -o "$TMP/xray.zip" xray geoip.dat geosite.dat -d "$TMP/unpack" >/dev/null 2>&1 || {
  # older archives may lack geo files
  unzip -o "$TMP/xray.zip" xray -d "$TMP/unpack" >/dev/null
}
install -m 0755 "$TMP/unpack/xray" "$OUT_DIR/xray"
[ -f "$TMP/unpack/geoip.dat" ]   && install -m 0644 "$TMP/unpack/geoip.dat"   "$OUT_DIR/geoip.dat"   || true
[ -f "$TMP/unpack/geosite.dat" ] && install -m 0644 "$TMP/unpack/geosite.dat" "$OUT_DIR/geosite.dat" || true

echo "done: $( "$OUT_DIR/xray" version | head -n1 )" >&2
