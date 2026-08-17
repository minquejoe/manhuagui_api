#!/usr/bin/env bash
# deploy.sh - one-click deploy manhuagui_api as a systemd service.
#
# Usage:
#   scripts/deploy.sh                      # system-wide service (needs sudo)
#   scripts/deploy.sh --user               # user-level service (no sudo)
#   scripts/deploy.sh --prefix /opt/manhuagui_api   # copy the app there, then deploy
#   scripts/deploy.sh --goproxy https://goproxy.cn,direct   # override the Go module proxy
#   scripts/deploy.sh --help
#
# What it does:
#   1. ensures the xray binary (via scripts/setup_xray.sh)
#   2. ensures a Go toolchain (auto-downloads to ~/.local/go when missing)
#   3. builds build/manhuagui_api (static, native arch); Go modules are
#      downloaded via GOPROXY (defaults to the goproxy.cn mirror, since
#      proxy.golang.org is often unreachable in mainland China; override with
#      --goproxy or the GO_PROXY environment variable)
#   4. ensures config.yaml exists (copied from config.example.yaml when missing)
#   5. installs the systemd unit and starts the service
#   6. waits for the API to come up and prints status / log hints
#
# The service can be managed with:
#   systemctl status manhuagui        # system mode
#   journalctl -u manhuagui -f        # logs
#   curl -s http://127.0.0.1:10018/v1/proxy/status
set -euo pipefail

MODE=system   # system | user
PREFIX=""
GOPROXY_ARG=""  # from --goproxy, overrides the GO_PROXY env / default mirror

while [ $# -gt 0 ]; do
  case "$1" in
    --user) MODE=user; shift ;;
    --prefix) [ $# -ge 2 ] || { echo "error: --prefix requires a directory" >&2; exit 1; }; PREFIX="$2"; shift 2 ;;
    --prefix=*) PREFIX="${1#*=}"; shift ;;
    --goproxy) [ $# -ge 2 ] || { echo "error: --goproxy requires a url" >&2; exit 1; }; GOPROXY_ARG="$2"; shift 2 ;;
    --goproxy=*) GOPROXY_ARG="${1#*=}"; shift ;;
    -h|--help) sed -n '2,24p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
    *) echo "error: unknown argument: $1 (try --help)" >&2; exit 1 ;;
  esac
done

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"
UNIT_NAME="manhuagui"

if ! command -v systemctl >/dev/null 2>&1; then
  echo "error: systemctl not found (not a systemd system?)" >&2
  exit 1
fi

# ---------- target user / install dir ----------
if [ "$(id -u)" = 0 ]; then
  RUN_USER="${SUDO_USER:-root}"
else
  RUN_USER="$(id -un)"
fi
RUN_GROUP="$(id -gn "$RUN_USER" 2>/dev/null || echo "$RUN_USER")"

APP_DIR="$REPO_DIR"
if [ -n "$PREFIX" ]; then
  case "$PREFIX" in
    "$REPO_DIR"|"$REPO_DIR"/*) echo "error: --prefix must be outside the repo directory" >&2; exit 1 ;;
  esac
  APP_DIR="${PREFIX%/}"
  echo "==> copying app to $APP_DIR"
  mkdir -p "$APP_DIR"
  tar -C "$REPO_DIR" --exclude=.git --exclude=logs --exclude=temp --exclude=build -cf - . | tar -C "$APP_DIR" -xf -
  [ "$(id -u)" = 0 ] && chown -R "$RUN_USER:$RUN_GROUP" "$APP_DIR"
fi
cd "$APP_DIR"

# ---------- 1. xray binary ----------
if [ ! -x "$APP_DIR/bin/xray" ]; then
  echo "==> installing xray binary"
  bash "$SCRIPT_DIR/setup_xray.sh" "" "$APP_DIR/bin"
else
  echo "==> xray binary present: $("$APP_DIR/bin/xray" version | head -n1)"
fi

# ---------- 2. go toolchain ----------
find_go() {
  if command -v go >/dev/null 2>&1; then
    go env GOROOT 2>/dev/null && return 0
  fi
  for d in "$HOME/go" /usr/local/go /usr/lib/go; do
    [ -x "$d/bin/go" ] && { echo "$d"; return 0; }
  done
  return 1
}
GO_DIR="$(find_go || true)"
if [ -z "$GO_DIR" ]; then
  echo "==> go not found, downloading to ~/.local/go"
  ver="1.22.12"
  case "$(uname -m)" in
    x86_64|amd64) arch="amd64" ;;
    aarch64|arm64) arch="arm64" ;;
    i386|i686) arch="386" ;;
    armv7l|armv6l) arch="armv6l" ;;
    *) echo "error: unsupported arch for go download" >&2; exit 1 ;;
  esac
  dest="$HOME/.local/go"
  rm -rf "$dest" "$HOME/.local/go.tgz"
  mkdir -p "$HOME/.local"
  # try go.dev first, fall back to the official CN mirror golang.google.cn
  dl_ok=""
  for base in "https://go.dev/dl" "https://golang.google.cn/dl"; do
    if curl -fsSL --max-time 600 -o "$HOME/.local/go.tgz" "${base}/go${ver}.linux-${arch}.tar.gz"; then
      dl_ok=1
      break
    fi
    echo "    download from ${base} failed, trying next mirror ..." >&2
  done
  [ -n "$dl_ok" ] || { echo "error: failed to download go toolchain" >&2; exit 1; }
  mkdir -p "$dest"
  tar -C "$dest" --strip-components=1 -xzf "$HOME/.local/go.tgz"
  rm -f "$HOME/.local/go.tgz"
  GO_DIR="$dest"
fi
export GOROOT="$GO_DIR" PATH="$GO_DIR/bin:$PATH"
export GOPATH="$HOME/.local/gopath" GOCACHE="$HOME/.cache/go-build" CGO_ENABLED=0
mkdir -p "$GOPATH"

# ---------- 2b. go module proxy ----------
# proxy.golang.org / sum.golang.org are often unreachable in mainland China,
# so download modules through a mirror by default. The shell's HTTP(S)_PROXY
# environment is still honored for the downloads (and later injected into the
# systemd unit). Precedence: --goproxy > $GO_PROXY > $GOPROXY > goproxy.cn.
if [ -n "$GOPROXY_ARG" ]; then
  export GOPROXY="$GOPROXY_ARG"
elif [ -z "${GO_PROXY:-}" ] && [ -z "${GOPROXY:-}" ]; then
  export GOPROXY="https://goproxy.cn,https://goproxy.io,direct"
  echo "==> using Go module proxy: $GOPROXY (override with --goproxy or GO_PROXY)"
elif [ -n "${GO_PROXY:-}" ]; then
  export GOPROXY="$GO_PROXY"
fi

# ---------- 3. build ----------
echo "==> building build/manhuagui_api"
mkdir -p "$APP_DIR/build"
"$GO_DIR/bin/go" build -trimpath -o "$APP_DIR/build/manhuagui_api" ./cmd/main.go

# ---------- 4. config ----------
if [ ! -f "$APP_DIR/config.yaml" ]; then
  echo "==> config.yaml missing, copying config.example.yaml"
  cp "$APP_DIR/config.example.yaml" "$APP_DIR/config.yaml"
  echo "    !!! edit $APP_DIR/config.yaml and set proxy.subscriptions before first use"
fi

# ---------- 5. systemd unit ----------
if [ "$MODE" = user ]; then
  UNIT_DIR="$HOME/.config/systemd/user"
  SYSCTL=(systemctl --user)
  SUDO=""
else
  UNIT_DIR="/etc/systemd/system"
  SUDO=""
  [ "$(id -u)" != 0 ] && SUDO="sudo"
  SYSCTL=($SUDO systemctl)
fi
mkdir -p "$UNIT_DIR"

TMP_UNIT="$(mktemp)"
ENV_FILE="$(mktemp)"
: > "$ENV_FILE"
for v in HTTP_PROXY HTTPS_PROXY ALL_PROXY NO_PROXY http_proxy https_proxy all_proxy no_proxy; do
  val="${!v:-}"
  [ -n "$val" ] && printf 'Environment="%s=%s"\n' "$v" "$val" >> "$ENV_FILE"
done

sed -e "s|__DIR__|$APP_DIR|g" -e "s|__USER__|$RUN_USER|g" -e "s|__GROUP__|$RUN_GROUP|g" \
  "$SCRIPT_DIR/manhuagui.service" > "$TMP_UNIT"
if [ "$MODE" = user ]; then
  sed -e '/^User=/d' -e '/^Group=/d' -e 's|WantedBy=multi-user.target|WantedBy=default.target|' "$TMP_UNIT" > "$TMP_UNIT.tmp"
  mv "$TMP_UNIT.tmp" "$TMP_UNIT"
fi
if [ -s "$ENV_FILE" ]; then
  sed -e "/^__ENV_LINES__$/r $ENV_FILE" -e "/^__ENV_LINES__$/d" "$TMP_UNIT" > "$TMP_UNIT.tmp"
else
  sed -e '/^__ENV_LINES__$/d' "$TMP_UNIT" > "$TMP_UNIT.tmp"
fi
mv "$TMP_UNIT.tmp" "$TMP_UNIT"
rm -f "$ENV_FILE"

$SUDO cp "$TMP_UNIT" "$UNIT_DIR/$UNIT_NAME.service"
rm -f "$TMP_UNIT"
echo "==> unit installed: $UNIT_DIR/$UNIT_NAME.service"

# ---------- 6. start ----------
# stop any stale manually-started instances (they would clash on the ports)
pkill -x manhuagui_api 2>/dev/null || true
pkill -x xray 2>/dev/null || true

"${SYSCTL[@]}" daemon-reload
"${SYSCTL[@]}" enable "$UNIT_NAME.service" >/dev/null 2>&1 || true
"${SYSCTL[@]}" restart "$UNIT_NAME.service"
if [ "$MODE" = user ]; then
  loginctl enable-linger "$RUN_USER" 2>/dev/null || true
fi

# ---------- 7. health check ----------
PORT="$(sed -n 's/^[[:space:]]*port:[[:space:]]*\([0-9][0-9]*\).*/\1/p' "$APP_DIR/config.yaml" | head -n1)"
PORT="${PORT:-10018}"
echo "==> waiting for API on 127.0.0.1:$PORT ..."
ok=""
for _ in $(seq 1 20); do
  if curl -fsS -m 2 "http://127.0.0.1:$PORT/ping" >/dev/null 2>&1; then ok=1; break; fi
  sleep 3
done

if [ -n "$ok" ]; then
  echo
  echo "==> deployed successfully =="
  echo "  api:      http://127.0.0.1:$PORT/ping"
  echo "  proxy:    curl -s http://127.0.0.1:$PORT/v1/proxy/status"
  echo "  service:  $([ "$MODE" = user ] && echo "systemctl --user" || echo "systemctl") status $UNIT_NAME"
  echo "  logs:     $([ "$MODE" = user ] && echo "journalctl --user" || echo "$SUDO journalctl") -u $UNIT_NAME -f"
else
  echo "error: API did not come up in time" >&2
  if [ "$MODE" = user ]; then
    journalctl --user -u "$UNIT_NAME" -n 30 --no-pager 2>&1 | tail -n 30 >&2 || true
  else
    $SUDO journalctl -u "$UNIT_NAME" -n 30 --no-pager 2>&1 | tail -n 30 >&2 || true
  fi
  exit 1
fi
