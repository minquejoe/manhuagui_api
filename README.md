# manhuagui_api

+ An unofficial backend for manhuagui (https://www.manhuagui.com/) written in Go/Gin.

### Related repositories

+ [Aoi-hosizora/manhuagui_flutter](https://github.com/Aoi-hosizora/manhuagui_flutter): The corresponding manhuagui android client.
+ [Aoi-hosizora/ahlib](https://github.com/Aoi-hosizora/ahlib): A personal golang library series, which is used in this project.

### Documentation

+ Visit https://api-manhuagui.aoihosizora.top/v1/swagger/index.html or https://manhuaguibackend.docs.apiary.io/ for api documentation.

### Reference

+ [austinh115/lz-string-go](https://github.com/austinh115/lz-string-go)
+ [juju/ratelimit](https://github.com/juju/ratelimit)
+ [bluele/gcache](https://github.com/bluele/gcache)

## Xray subscription proxy

The backend can route its manhuagui requests through the node with the lowest
HTTP latency among all nodes of a v2ray-style VPN subscription:

1. A local [Xray-core](https://github.com/XTLS/Xray-core) instance is managed
   by this program (auto-downloaded from GitHub releases when missing, or
   installed manually via `scripts/setup_xray.sh`).
2. Every `proxy.refresh-interval` seconds the subscription is re-fetched and
   re-parsed (supports `vmess://`, `vless://`, `trojan://`, `ss://`, `socks://`
   lines, base64 or plaintext). Unsupported node types (hysteria2/tuic/ssr, ...)
   are skipped with a warning.
3. All nodes are written into `xray/config.json` as outbounds. Every
   `proxy.probe-interval` seconds the manager re-selects the proxy node: xray's
   `observatory` measures every node against `proxy.probe-url`
   (`https://www.manhuagui.com/`), the manager additionally times each alive
   node against every `proxy.probe-extra-url` (e.g. the image CDN
   `https://i.hamreus.com/`, by temporarily pinning the node via the balancer
   override API), and the node with the lowest combined latency is pinned via
   the balancer override. Node changes from the subscription trigger an
   automatic xray restart; when every node is dead the subscription is
   re-fetched immediately (self-healing, rate-limited).
4. Requests to `proxy.hosts` (manhuagui/hamreus domains by default) go through
   the local SOCKS proxy (`127.0.0.1:proxy.xray-socks-port`); everything else
   (e.g. GitHub API) stays direct. When xray is not ready, requests fall back
   to direct connections.
5. `GET /v1/proxy/status` reports the current state (ready flag, node count,
   selected node and latency, probe/refresh intervals).
6. `GET /v1/image/proxy?url=<encoded image url>` proxies whitelisted image CDN
   urls (`*.hamreus.com`, `*.mhgui.com`, configurable via `proxy.image-hosts`)
   through the pinned node and streams the response back, so mobile clients
   can fetch chapter images and covers through the fast server path instead of
   their own slow/flaky connection to the CDN. See
   `docs/flutter_image_proxy.md` for the Flutter-side integration prompt.

```yaml
proxy:
  enabled: true          # route manhuagui requests through the lowest-latency xray node
  auto-install: true     # download the xray binary automatically when missing
  xray-bin: ./bin/xray
  xray-config: ./xray/config.json
  xray-socks-port: 10808
  xray-http-port: 10809
  xray-api-port: 10810   # xray gRPC api (observatory + routing), 127.0.0.1 only
  subscriptions:         # v2ray-style subscription urls (base64 or plaintext)
    - https://example.com/sub?token=YOUR_TOKEN
  refresh-interval: 86400 # seconds: re-fetch the subscription and restart xray if nodes changed (24h)
  probe-interval: 3600    # seconds: re-measure every node's latency and re-select the best node (1h)
  probe-url: https://www.manhuagui.com/
  probe-extra-urls:       # extra latency targets measured per node each round (combined with probe-url)
    - https://i.hamreus.com/   # image CDN, so the pinned node is fast for images too
  hosts:                 # hosts routed through the proxy; "." prefix matches subdomains
    - manhuagui.com
    - .manhuagui.com
    - hamreus.com
    - .hamreus.com
```

Notes:

+ `xray-version` can pin a specific Xray release tag (e.g. `v26.3.27`);
  otherwise the latest GitHub release is used.
+ The generated config is validated with `xray run -test` before being applied;
  a broken node never breaks the running proxy.
+ If the subscription is unreachable or yields no usable node, the API keeps
  running with direct connections. Idempotent requests (GET/HEAD) are retried
  once when the currently selected node dies between probe cycles, and the
  xray balancer falls back to direct when every node is down.

## One-click deployment (systemd)

`scripts/deploy.sh` builds the binary, installs the xray binary, writes the
systemd unit and starts the service with a health check:

```bash
scripts/deploy.sh                          # system-wide service (needs sudo)
scripts/deploy.sh --user                   # user-level service (no sudo)
scripts/deploy.sh --prefix /opt/manhuagui_api   # copy the app there first
scripts/deploy.sh --goproxy https://goproxy.cn,direct   # override the Go module proxy
```

The script is idempotent (re-running it rebuilds and restarts) and:

+ auto-installs a Go toolchain to `~/.local/go` when none is found (downloads
  from go.dev with a fallback to the official CN mirror golang.google.cn);
+ downloads Go modules through a mirror because `proxy.golang.org` is usually
  unreachable in mainland China — default `GOPROXY=https://goproxy.cn,https://goproxy.io,direct`,
  override with `--goproxy <url>` or the `GO_PROXY` environment variable (an
  already exported `GOPROXY` is respected as well);
+ copies `config.example.yaml` to `config.yaml` when it is missing;
+ injects the shell's `HTTP(S)_PROXY`/`NO_PROXY` environment into the unit so
  subscription fetches keep working behind a proxy;
+ stops stale manually-started instances before starting the service.

The unit (`scripts/manhuagui.service`) runs the API with `Restart=on-failure`,
a 30s graceful-stop timeout, and `WorkingDirectory` set to the app dir so
relative paths (`bin/xray`, `xray/config.json`, `logs/`) resolve correctly.
The spawned xray child is killed automatically when the service stops.

Manage it with:

```bash
systemctl status manhuagui
journalctl -u manhuagui -f
curl -s http://127.0.0.1:10018/v1/proxy/status
```
