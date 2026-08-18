package controller

import (
	"github.com/Aoi-hosizora/ahlib/xmodule"
	"github.com/Aoi-hosizora/manhuagui-api/internal/pkg/config"
	"github.com/Aoi-hosizora/manhuagui-api/internal/pkg/module/sn"
	"github.com/Aoi-hosizora/manhuagui-api/internal/pkg/result"
	"github.com/Aoi-hosizora/manhuagui-api/internal/service"
	"github.com/gin-gonic/gin"
	"io"
	"net/http"
	"net/url"
	"strings"
)

var defaultImageHosts = []string{"hamreus.com", "mhgui.com"}

// ImageProxyController proxies whitelisted image CDN urls (hamreus/mhgui)
// through the xray proxy, so that slow or flaky client-to-CDN connections are
// replaced by the fast server path through the pinned lowest-latency node.
type ImageProxyController struct {
	httpService *service.HttpService
	hosts       []string // whitelist, entries may start with "." for subdomains
}

func NewImageProxyController() *ImageProxyController {
	hosts := defaultImageHosts
	if cfg := xmodule.MustGetByName(sn.SConfig).(*config.Config).Proxy; cfg != nil && len(cfg.ImageHosts) > 0 {
		hosts = cfg.ImageHosts
	}
	return &ImageProxyController{
		httpService: xmodule.MustGetByName(sn.SHttpService).(*service.HttpService),
		hosts:       hosts,
	}
}

// allowImageHost checks the host against the whitelist (SSRF protection).
func (c *ImageProxyController) allowImageHost(host string) bool {
	host = strings.ToLower(host)
	for _, h := range c.hosts {
		h = strings.ToLower(strings.TrimSpace(h))
		if h == "" {
			continue
		}
		if strings.HasPrefix(h, ".") {
			if host == strings.TrimPrefix(h, ".") || strings.HasSuffix(host, h) {
				return true
			}
		} else if host == h || strings.HasSuffix(host, "."+h) {
			return true
		}
	}
	return false
}

// GET /v1/image/proxy?url=<encoded image url>
func (c *ImageProxyController) ProxyImage(ctx *gin.Context) *result.Result {
	rawURL := ctx.Query("url")
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || !c.allowImageHost(u.Hostname()) {
		return result.Status(http.StatusBadRequest).SetMessage("invalid image url")
	}

	resp, err := c.httpService.HttpGetStream(rawURL, c.allowImageHost, nil)
	if err != nil {
		return result.Status(http.StatusBadGateway).SetMessage("image proxy failed").SetError(err, ctx)
	}
	defer resp.Body.Close()

	// pass through essential headers (signed urls keep their query string, so
	// no recomposition happens here), then stream the body
	for _, k := range []string{"Content-Type", "Content-Length", "ETag", "Last-Modified"} {
		if v := resp.Header.Get(k); v != "" {
			ctx.Header(k, v)
		}
	}
	ctx.Header("Cache-Control", "public, max-age=86400")
	ctx.Status(resp.StatusCode)
	_, _ = io.Copy(ctx.Writer, resp.Body)
	return nil // streaming response, do not wrap in the json result
}
