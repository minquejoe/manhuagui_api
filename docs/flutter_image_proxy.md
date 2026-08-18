# Flutter 端改造提示词：图片经后端代理下载

> 用途：本文件是给 **manhuagui_flutter**（或其他 Flutter 项目）的改造说明，
> 可直接把「完整提示词」一节粘贴给 AI/协作者实施。后端（manhuagui_api）
> 的图片代理端点已上线，无需再改后端。

---

## 背景

手机直连图床（`i.hamreus.com` / `cf.mhgui.com`）延迟高且间歇性超时（实测
1.7s+，白天曾连续 15s 超时）。后端已新增图片代理端点：服务端经 xray 最优
节点（每 1 小时按漫画站+图床综合时延重选）下载图片后流式转发，手机只需
访问内网/Tailscale 上的 API 服务器（~10ms），整条链路稳定且快（实测
章节图 0.4~1.9s、封面 0.9s）。

## 后端端点契约（已上线，勿改）

```
GET {BASE_API_PURE_URL}v1/image/proxy?url=<encodeURIComponent(完整图片URL)>
```

- `url`：图片绝对 URL，**必须整体 `Uri.encodeComponent`**（URL 自带 `?e=...&m=...` 防过期签名，编码后作为单个 query 参数，服务器原样透传，**不要拆参数重组**）。
- 白名单（SSRF 防护）：仅允许 `*.hamreus.com` 与 `*.mhgui.com`（后端可配）。其他域名返回 `400`。
- 响应：**透传原始状态码与 `Content-Type`/`Content-Length`/`ETag`**，流式传输图片字节（不转 JSON）；成功时带 `Cache-Control: public, max-age=86400`。
- 失败：后端到图床网络错误返回 `502`（JSON `{"code":502,...}`）；参数非法返回 `400`。
- 请求已自动带浏览器 UA 和 `Referer: https://www.manhuagui.com/`（图床防盗链校验需要）。

---

## 完整提示词（直接粘贴）

```
任务：在 Flutter 项目（manhuagui_flutter）中，把所有图片加载/下载改为
经由后端 API 的图片代理端点下载，以解决手机直连图床慢/超时的问题。

后端已提供端点（无需修改后端）：
  GET {API_BASE_PURE_URL}v1/image/proxy?url=<encodeURIComponent(原始图片URL)>
  白名单：*.hamreus.com 与 *.mhgui.com；响应为流式图片字节，透传
  Content-Type；非法域名 400，后端网络错误 502。

请按以下清单实施：

1. lib/config.dart 增加常量：
   const IMAGE_PROXY_PATH = 'v1/image/proxy';

2. 新建 lib/service/image_url.dart（或放 config.dart），实现统一改写函数：
   String proxyImageUrl(String url, {String? apiBase}) {
     final u = Uri.parse(url);
     if (u.host.isEmpty) return url;
     final host = u.host.toLowerCase();
     final isCdn = host == 'hamreus.com' || host.endsWith('.hamreus.com') ||
                   host == 'mhgui.com'  || host.endsWith('.mhgui.com');
     if (!isCdn) return url;               // 非图床域名不动
     final base = (apiBase ?? BASE_API_PURE_URL).replaceAll(RegExp(r'/+$'), '');
     return '$base/$IMAGE_PROXY_PATH?url=${Uri.encodeComponent(url)}';
   }
   注意：url 必须整体 Uri.encodeComponent；绝对不要拆开图片 URL 的
   query（?e=..&m=.. 是防过期签名，必须原样透传）。

3. 章节在线看图（lib/page/manga_viewer.dart）：
   找到生成每页图片 URL 的 futures（_urlFutures），对每个页面 URL 调用
   proxyImageUrl() 后再交给 MangaGalleryView。

4. 章节下载（lib/service/storage/download.dart）：
   下载队列里对图片 URL 调用 proxyImageUrl()；DOWNLOAD_IMAGE_TIMEOUT
   建议从 12000 放宽到 25000（下载经代理多一跳，且要兼容大图）。

5. 封面/头像（可选但推荐）：所有显示 cf.mhgui.com / cf.hamreus.com 封面的
   地方统一走 proxyImageUrl()（搜索 'cf.mhgui.com'、'cf.hamreus.com'、
   'hamreus' 等字面量逐一接入）。

6. 失败回退（推荐）：图片加载失败时回退直连原始 URL，保证可用性：
   try { 加载 proxyImageUrl(url) } catch { 加载原始 url }

7. 调试建议：临时在 proxyImageUrl 里 print 改写前后的 URL，确认
   encode 正确、未被拆分。

完成后验证：在线看图、章节下载、漫画详情封面、列表页封面均正常；
断网/后端 502 时能回退直连。所有改动保持现有代码风格。
```

---

## 实施要点（给开发者的补充说明）

| 项 | 说明 |
|---|---|
| 编码 | `Uri.encodeComponent(url)` 是**必须**的；图片 URL 含 `://`、`?`、`&`、`=`、中文路径，不编码会破坏 query 结构 |
| 签名透传 | `?e=1787928849&m=qto-...` 是图床的过期签名（防盗链），**原样保留**，服务器端不做任何重组 |
| 超时 | 在线看图 `GALLERY_IMAGE_TIMEOUT`（15s）可保持；下载 `DOWNLOAD_IMAGE_TIMEOUT`（12s）建议放宽到 25s |
| 重试 | 后端已在请求层对幂等请求做一次重试；APP 层失败回退直连即可，无需额外重试逻辑 |
| 缓存 | 后端返回 `Cache-Control: public, max-age=86400`，APP 图片缓存/磁盘缓存策略无需改动 |
| 白名单 | 后端配置 `proxy.image-hosts` 可增删图床域名；若以后漫画柜换 CDN 域名，只需后端加白名单，APP 无需发版（但 proxyImageUrl 的域名判断也要同步加） |
| 安全 | 该端点无鉴权，建议仅在内网/Tailscale 使用；不要暴露公网 |

## 测试清单

- [ ] 章节在线看图：翻页正常，图片加载 < 2s，WebP 正常显示
- [ ] 章节下载：下载完成，图片文件完整可读（webp/jpg）
- [ ] 漫画详情页：封面正常
- [ ] 列表页（首页/更新/排行）：封面缩略图正常
- [ ] 网络断开：APP 提示网络错误而非崩溃
- [ ] 后端 502 时：回退直连生效（如已实现）
- [ ] 日志检查：proxyImageUrl 输出 URL 未被拆分、编码正确
