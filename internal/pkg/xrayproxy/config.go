package xrayproxy

import (
	"fmt"

	"github.com/Aoi-hosizora/manhuagui-api/internal/pkg/config"
)

// buildXrayConfig generates the xray server config:
//   - a local SOCKS/HTTP inbound that the backend API uses as outbound proxy;
//   - all subscription nodes as outbounds;
//   - an observatory that periodically probes every node against `probe-url`
//     (www.manhuagui.com) and a leastPing balancer that routes all proxied
//     traffic through the node with the lowest measured HTTP latency;
//   - a gRPC API (ObservatoryService + RoutingService) listening on
//     127.0.0.1:api-port for status reporting.
func buildXrayConfig(cfg *config.ProxyConfig, nodes []*Node) map[string]any {
	tags := make([]string, 0, len(nodes))
	outbounds := make([]any, 0, len(nodes)+3)
	for _, n := range nodes {
		tags = append(tags, n.Tag)
		outbounds = append(outbounds, n.Outbound)
	}
	outbounds = append(outbounds,
		map[string]any{"tag": "direct", "protocol": "freedom", "settings": map[string]any{}},
		map[string]any{"tag": "block", "protocol": "blackhole", "settings": map[string]any{}},
	)

	return map[string]any{
		"log": map[string]any{"loglevel": "info"},
		// Resolve node server hostnames through xray's internal DNS with
		// several fallbacks (the system resolver may be flaky, e.g. when
		// many nodes are probed concurrently).
		"dns": map[string]any{
			"servers": []any{
				"223.5.5.5", "119.29.29.29", "114.114.114.114", "localhost",
			},
		},
		"inbounds": []any{
			map[string]any{
				"tag":      "socks-in",
				"listen":   "127.0.0.1",
				"port":     cfg.SocksPort,
				"protocol": "socks",
				"settings": map[string]any{"udp": true, "auth": "noauth"},
			},
			map[string]any{
				"tag":      "http-in",
				"listen":   "127.0.0.1",
				"port":     cfg.HttpPort,
				"protocol": "http",
				"settings": map[string]any{},
			},
		},
		"outbounds": outbounds,
		"routing": map[string]any{
			"domainStrategy": "AsIs",
			"rules": []any{
				map[string]any{
					"type":        "field",
					"inboundTag":  []string{"socks-in", "http-in"},
					"network":     "tcp,udp",
					"balancerTag": "balancer",
				},
			},
			"balancers": []any{
				map[string]any{
					"tag":         "balancer",
					"selector":    tags,
					"strategy":    map[string]any{"type": "leastPing"},
					"fallbackTag": "direct", // all nodes dead -> go direct instead of failing
				},
			},
		},
		"observatory": map[string]any{
			"subjectSelector":   tags,
			"probeUrl":          cfg.ProbeURL,
			"probeInterval":     fmt.Sprintf("%ds", cfg.ProbeInterval),
			"enableConcurrency": true,
		},
		"api": map[string]any{
			"tag":      "api",
			"listen":   fmt.Sprintf("127.0.0.1:%d", cfg.ApiPort),
			"services": []string{"ObservatoryService", "RoutingService"},
		},
	}
}
