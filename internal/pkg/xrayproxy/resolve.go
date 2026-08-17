package xrayproxy

import (
	"context"
	"fmt"
	"net"
	"sort"
	"sync"
	"time"
)

// dnsServers are used to resolve node server hostnames to IPs before writing
// the xray config. The system resolver (e.g. tailscale MagicDNS) is often
// unreliable for these CDN relay hostnames (intermittent malformed/NXDOMAIN
// responses), which made xray reset connections at runtime. Resolving here
// also keeps the xray child free of any DNS dependency.
var dnsServers = []string{"223.5.5.5", "119.29.29.29", "114.114.114.114"}

func lookupHost(ctx context.Context, host string) (string, error) {
	var lastErr error
	for _, server := range dnsServers {
		r := &net.Resolver{
			PreferGo: true,
			Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
				d := net.Dialer{Timeout: 2 * time.Second}
				return d.DialContext(ctx, "udp", server+":53")
			},
		}
		ips, err := r.LookupIP(ctx, "ip4", host)
		if err == nil && len(ips) > 0 {
			return ips[0].String(), nil
		}
		lastErr = err
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no A record")
	}
	return "", lastErr
}

// resolveNodeAddresses resolves every node's server hostname to an IP.
// Unresolvable nodes are dropped (they are dead anyway) and reported in the
// returned warnings. Order is preserved.
func resolveNodeAddresses(nodes []*Node) (kept []*Node, warnings []string) {
	kept = make([]*Node, 0, len(nodes))
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, 8) // limit concurrent lookups
	for _, n := range nodes {
		wg.Add(1)
		sem <- struct{}{}
		go func(n *Node) {
			defer wg.Done()
			defer func() { <-sem }()
			addr := nodeAddress(n)
			if addr == "" || net.ParseIP(addr) != nil {
				mu.Lock()
				kept = append(kept, n)
				mu.Unlock()
				return
			}
			ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
			defer cancel()
			ip, err := lookupHost(ctx, addr)
			if err != nil {
				mu.Lock()
				warnings = append(warnings, fmt.Sprintf("drop node %s (%s): resolve %s: %v", n.Tag, n.Name, addr, err))
				mu.Unlock()
				return
			}
			setNodeAddress(n, ip)
			mu.Lock()
			kept = append(kept, n)
			mu.Unlock()
		}(n)
	}
	wg.Wait()
	sort.Slice(kept, func(i, j int) bool { return kept[i].Tag < kept[j].Tag })
	return kept, warnings
}

// nodeAddress returns the dial address of a node's outbound config.
func nodeAddress(n *Node) string {
	settings, _ := n.Outbound["settings"].(map[string]any)
	if vnext, ok := settings["vnext"].([]any); ok && len(vnext) > 0 {
		if v0, ok := vnext[0].(map[string]any); ok {
			if s, ok := v0["address"].(string); ok {
				return s
			}
		}
	}
	if servers, ok := settings["servers"].([]any); ok && len(servers) > 0 {
		if s0, ok := servers[0].(map[string]any); ok {
			if s, ok := s0["address"].(string); ok {
				return s
			}
		}
	}
	return ""
}

// setNodeAddress replaces the node's dial address with an IP, keeping the
// original hostname for the TLS serverName (SNI) so servers still see the
// intended domain instead of the IP.
func setNodeAddress(n *Node, ip string) {
	orig := nodeAddress(n)
	settings, _ := n.Outbound["settings"].(map[string]any)
	if vnext, ok := settings["vnext"].([]any); ok && len(vnext) > 0 {
		if v0, ok := vnext[0].(map[string]any); ok {
			v0["address"] = ip
		}
	}
	if servers, ok := settings["servers"].([]any); ok && len(servers) > 0 {
		if s0, ok := servers[0].(map[string]any); ok {
			s0["address"] = ip
		}
	}
	if ss, ok := n.Outbound["streamSettings"].(map[string]any); ok {
		for _, key := range []string{"tlsSettings", "xtlsSettings", "realitySettings"} {
			if ts, ok := ss[key].(map[string]any); ok {
				if sn, _ := ts["serverName"].(string); sn == "" {
					ts["serverName"] = orig
				}
			}
		}
	}
}
