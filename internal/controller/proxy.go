package controller

import (
	"github.com/Aoi-hosizora/ahlib/xmodule"
	"github.com/Aoi-hosizora/goapidoc"
	"github.com/Aoi-hosizora/manhuagui-api/internal/pkg/module/sn"
	"github.com/Aoi-hosizora/manhuagui-api/internal/pkg/result"
	"github.com/Aoi-hosizora/manhuagui-api/internal/pkg/xrayproxy"
	"github.com/gin-gonic/gin"
)

func init() {
	goapidoc.AddDefinitions(
		goapidoc.NewDefinition("ProxyStatus", "xray proxy status response").
			Properties(
				goapidoc.NewProperty("enabled", "boolean", true, "proxy enabled in config"),
				goapidoc.NewProperty("ready", "boolean", true, "xray SOCKS proxy is ready"),
				goapidoc.NewProperty("node_count", "integer#int32", true, "parsed node count"),
				goapidoc.NewProperty("current_tag", "string", false, "selected outbound tag"),
				goapidoc.NewProperty("current_node", "string", false, "selected node name"),
				goapidoc.NewProperty("current_latency_ms", "integer#int64", false, "selected node latency"),
				goapidoc.NewProperty("last_updated", "string#date-time", false, "last subscription refresh time"),
				goapidoc.NewProperty("refresh_interval_seconds", "integer#int64", true, "subscription refresh interval"),
				goapidoc.NewProperty("probe_interval_seconds", "integer#int64", true, "latency probe interval"),
				goapidoc.NewProperty("probe_url", "string", true, "latency probe url"),
				goapidoc.NewProperty("subscriptions", "string[]", true, "configured subscription urls"),
			),
	)
	goapidoc.AddOperations(
		goapidoc.NewGetOperation("/v1/proxy/status", "Get xray proxy status").
			Tags("Proxy").
			Responses(goapidoc.NewResponse(200, "_Result<ProxyStatus>")),
	)
}

type ProxyController struct {
	manager *xrayproxy.Manager
}

func NewProxyController() *ProxyController {
	return &ProxyController{
		manager: xmodule.MustGetByName(sn.SProxyManager).(*xrayproxy.Manager),
	}
}

// GET /v1/proxy/status
func (p *ProxyController) GetStatus(c *gin.Context) *result.Result {
	return result.Ok().SetData(p.manager.Status())
}
