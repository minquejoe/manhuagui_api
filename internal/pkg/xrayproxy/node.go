package xrayproxy

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// Node is a subscription node that can be converted into an xray outbound.
type Node struct {
	Tag      string         // xray outbound tag, e.g. node-0
	Name     string         // display name (ps / remark / host), may be empty
	Raw      string         // original subscription line
	Outbound map[string]any // xray outbound config
}

func (n *Node) String() string {
	if n.Name == "" {
		return n.Tag
	}
	return fmt.Sprintf("%s (%s)", n.Tag, n.Name)
}

// decodeSubscription decodes a subscription body which may be either plaintext
// lines (containing "://") or a base64-encoded v2ray subscription.
func decodeSubscription(body []byte) (string, error) {
	s := strings.TrimSpace(string(body))
	if strings.Contains(s, "://") {
		return s, nil
	}
	compact := strings.NewReplacer("\n", "", "\r", "", " ", "").Replace(s)
	for _, enc := range []*base64.Encoding{
		base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding,
	} {
		if bs, err := enc.DecodeString(compact); err == nil {
			dec := string(bs)
			if strings.Contains(dec, "://") {
				return dec, nil
			}
		}
	}
	return s, nil
}

// parseNodes parses subscription lines into xray outbound nodes.
// Unsupported lines are skipped and reported in the returned warnings.
func parseNodes(lines []string) (nodes []*Node, warnings []string) {
	nodes = make([]*Node, 0)
	idx := 0
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		n, err := parseNode(line, idx)
		if err != nil {
			preview := line
			if len(preview) > 60 {
				preview = preview[:60] + "..."
			}
			warnings = append(warnings, fmt.Sprintf("skip node #%d (%s): %v", idx, preview, err))
			continue
		}
		idx++
		nodes = append(nodes, n)
	}
	return nodes, warnings
}

// parseNode parses a single subscription line. Only protocols natively
// supported by the vanilla Xray-core binary are accepted (vmess, vless,
// trojan, shadowsocks, socks). Other schemes (hysteria2, tuic, ssr, ...) are
// rejected on purpose: an unknown outbound protocol would make xray reject the
// whole generated config.
func parseNode(line string, idx int) (*Node, error) {
	scheme, rest, ok := strings.Cut(line, "://")
	if !ok {
		return nil, fmt.Errorf("invalid node line")
	}
	tag := fmt.Sprintf("node-%d", idx)
	switch strings.ToLower(scheme) {
	case "vmess":
		return parseVmess(tag, line, rest)
	case "vless":
		return parseVless(tag, line)
	case "trojan":
		return parseTrojan(tag, line)
	case "ss":
		return parseSS(tag, line, rest)
	case "socks", "socks5":
		return parseSocks(tag, line)
	default:
		return nil, fmt.Errorf("unsupported scheme %q", scheme)
	}
}

func decodeBase64Any(s string) (string, error) {
	compact := strings.NewReplacer("\n", "", "\r", "", " ", "").Replace(s)
	for _, enc := range []*base64.Encoding{
		base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding,
	} {
		if bs, err := enc.DecodeString(compact); err == nil {
			return string(bs), nil
		}
	}
	return "", fmt.Errorf("invalid base64")
}

func str(m map[string]any, key string) string {
	v, ok := m[key]
	if !ok || v == nil {
		return ""
	}
	switch t := v.(type) {
	case string:
		return t
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case json.Number:
		return t.String()
	default:
		return fmt.Sprint(t)
	}
}

func intVal(m map[string]any, key string) int {
	v, ok := m[key]
	if !ok || v == nil {
		return 0
	}
	switch t := v.(type) {
	case float64:
		return int(t)
	case json.Number:
		if i, err := t.Int64(); err == nil {
			return int(i)
		}
	case string:
		if i, err := strconv.Atoi(strings.TrimSpace(t)); err == nil {
			return i
		}
	}
	return 0
}

func boolVal(m map[string]any, key string) bool {
	v, ok := m[key]
	if !ok || v == nil {
		return false
	}
	switch t := v.(type) {
	case bool:
		return t
	case string:
		s := strings.ToLower(strings.TrimSpace(t))
		return s == "1" || s == "true" || s == "yes" || s == "on"
	case float64:
		return t != 0
	}
	return false
}

// parseVmess parses `vmess://<base64-json>` (v2ray legacy format).
func parseVmess(tag, line, rest string) (*Node, error) {
	j, err := decodeBase64Any(rest)
	if err != nil {
		return nil, fmt.Errorf("vmess: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(j), &m); err != nil {
		return nil, fmt.Errorf("vmess: invalid json: %v", err)
	}
	addr := str(m, "add")
	port := intVal(m, "port")
	id := str(m, "id")
	if addr == "" || port == 0 || id == "" {
		return nil, fmt.Errorf("vmess: missing add/port/id")
	}
	aid := intVal(m, "aid")
	security := str(m, "scy")
	if security == "" {
		security = "auto"
	}
	network := str(m, "net")
	if network == "" {
		network = "tcp"
	}
	name := str(m, "ps")
	if name == "" {
		name = addr
	}

	settings := map[string]any{
		"vnext": []any{map[string]any{
			"address": addr,
			"port":    port,
			"users": []any{map[string]any{
				"id":       id,
				"alterId":  aid,
				"security": security,
				"level":    0,
			}},
		}},
	}
	out := map[string]any{
		"tag":      tag,
		"protocol": "vmess",
		"settings": settings,
		"streamSettings": buildStream(network, str(m, "tls"), str(m, "host"), str(m, "path"), str(m, "sni"),
			str(m, "fp"), str(m, "alpn"), boolVal(m, "allowInsecure"), str(m, "pbk"), str(m, "sid"), str(m, "spx"),
			str(m, "type"), "", ""),
	}
	return &Node{Tag: tag, Name: name, Raw: line, Outbound: out}, nil
}

// parseVless parses `vless://uuid@host:port?params#name`.
func parseVless(tag, line string) (*Node, error) {
	u, err := url.Parse(line)
	if err != nil {
		return nil, fmt.Errorf("vless: %v", err)
	}
	id := ""
	if u.User != nil {
		id = u.User.Username()
	}
	host := u.Hostname()
	port := defaultPort(u.Port(), 443)
	if id == "" || host == "" || port == 0 {
		return nil, fmt.Errorf("vless: missing id/host/port")
	}
	q := u.Query()
	network := q.Get("type")
	if network == "" {
		network = "tcp"
	}
	flow := q.Get("flow")
	users := map[string]any{"id": id, "encryption": "none", "level": 0}
	if flow != "" {
		users["flow"] = flow
	}
	name := u.Fragment
	if name == "" {
		name = host
	}

	settings := map[string]any{
		"vnext": []any{map[string]any{
			"address": host,
			"port":    port,
			"users":   []any{users},
		}},
	}
	out := map[string]any{
		"tag":      tag,
		"protocol": "vless",
		"settings": settings,
		"streamSettings": buildStream(network, q.Get("security"), q.Get("host"), q.Get("path"), q.Get("sni"),
			q.Get("fp"), q.Get("alpn"), q.Get("allowInsecure") == "1" || q.Get("allowInsecure") == "true",
			q.Get("pbk"), q.Get("sid"), q.Get("spx"), q.Get("headerType"), q.Get("serviceName"), q.Get("mode")),
	}
	return &Node{Tag: tag, Name: name, Raw: line, Outbound: out}, nil
}

// parseTrojan parses `trojan://password@host:port?params#name`.
func parseTrojan(tag, line string) (*Node, error) {
	u, err := url.Parse(line)
	if err != nil {
		return nil, fmt.Errorf("trojan: %v", err)
	}
	password := ""
	if u.User != nil {
		password = u.User.Username()
	}
	host := u.Hostname()
	port := defaultPort(u.Port(), 443)
	if password == "" || host == "" || port == 0 {
		return nil, fmt.Errorf("trojan: missing password/host/port")
	}
	q := u.Query()
	sni := q.Get("sni")
	if sni == "" {
		sni = q.Get("peer")
	}
	security := q.Get("security")
	if security == "" {
		security = "tls"
	}
	flow := q.Get("flow")
	server := map[string]any{"address": host, "port": port, "password": password, "level": 0}
	if flow != "" {
		server["flow"] = flow
	}
	name := u.Fragment
	if name == "" {
		name = host
	}

	settings := map[string]any{"servers": []any{server}}
	out := map[string]any{
		"tag":      tag,
		"protocol": "trojan",
		"settings": settings,
		"streamSettings": buildStream("tcp", security, "", "", sni, q.Get("fp"), q.Get("alpn"),
			q.Get("allowInsecure") == "1" || q.Get("allowInsecure") == "true",
			q.Get("pbk"), q.Get("sid"), q.Get("spx"), "", "", ""),
	}
	return &Node{Tag: tag, Name: name, Raw: line, Outbound: out}, nil
}

// parseSS parses `ss://userinfo@host:port#name`, where userinfo is either
// plaintext `method:password` (SIP002) or base64-encoded `method:password`.
// It also accepts the legacy format where the whole `method:password@host:port`
// is base64-encoded with no literal "@" outside the base64.
func parseSS(tag, line, rest string) (*Node, error) {
	name := ""
	if i := strings.Index(rest, "#"); i >= 0 {
		name, _ = url.QueryUnescape(rest[i+1:])
		rest = rest[:i]
	}
	query := ""
	if i := strings.Index(rest, "?"); i >= 0 {
		query = rest[i+1:]
		rest = rest[:i]
	}
	at := strings.LastIndex(rest, "@")
	if at < 0 {
		// legacy format: whole `method:password@host:port` is base64
		if dec, err := decodeBase64Any(rest); err == nil && strings.Contains(dec, "@") {
			rest = dec
			at = strings.LastIndex(rest, "@")
		}
	}
	if at < 0 {
		return nil, fmt.Errorf("ss: missing @")
	}
	userinfo, hostport := rest[:at], rest[at+1:]
	host, port, err := splitHostPort(hostport)
	if err != nil {
		return nil, fmt.Errorf("ss: %v", err)
	}
	method, password := "", ""
	if strings.Contains(userinfo, ":") {
		parts := strings.SplitN(userinfo, ":", 2)
		method, password = parts[0], parts[1]
	} else {
		dec, err := decodeBase64Any(userinfo)
		if err != nil {
			return nil, fmt.Errorf("ss: invalid userinfo: %v", err)
		}
		parts := strings.SplitN(dec, ":", 2)
		if len(parts) != 2 {
			return nil, fmt.Errorf("ss: invalid userinfo")
		}
		method, password = parts[0], parts[1]
	}
	if method == "" || password == "" {
		return nil, fmt.Errorf("ss: missing method/password")
	}
	if q, err := url.ParseQuery(query); err == nil && q.Get("plugin") != "" {
		return nil, fmt.Errorf("ss: node with obfs plugin unsupported")
	}
	if name == "" {
		name = host
	}

	settings := map[string]any{
		"servers": []any{map[string]any{
			"address":  host,
			"port":     port,
			"method":   method,
			"password": password,
			"level":    0,
		}},
	}
	out := map[string]any{"tag": tag, "protocol": "shadowsocks", "settings": settings}
	return &Node{Tag: tag, Name: name, Raw: line, Outbound: out}, nil
}

// parseSocks parses `socks://host:port#name` (rare but cheap to support).
func parseSocks(tag, line string) (*Node, error) {
	u, err := url.Parse(line)
	if err != nil {
		return nil, fmt.Errorf("socks: %v", err)
	}
	host := u.Hostname()
	port := defaultPort(u.Port(), 1080)
	if host == "" || port == 0 {
		return nil, fmt.Errorf("socks: missing host/port")
	}
	name := u.Fragment
	if name == "" {
		name = host
	}
	settings := map[string]any{
		"servers": []any{map[string]any{
			"address": host,
			"port":    port,
		}},
	}
	out := map[string]any{"tag": tag, "protocol": "socks", "settings": settings}
	return &Node{Tag: tag, Name: name, Raw: line, Outbound: out}, nil
}

func defaultPort(port string, def int) int {
	if port == "" {
		return def
	}
	p, err := strconv.Atoi(port)
	if err != nil || p <= 0 || p > 65535 {
		return 0
	}
	return p
}

func splitHostPort(s string) (string, int, error) {
	host, portStr := "", ""
	if strings.HasPrefix(s, "[") {
		end := strings.Index(s, "]")
		if end < 0 {
			return "", 0, fmt.Errorf("invalid host:port")
		}
		host = s[1:end]
		portStr = strings.TrimPrefix(s[end+1:], ":")
	} else {
		i := strings.LastIndex(s, ":")
		if i < 0 {
			return "", 0, fmt.Errorf("missing port")
		}
		host, portStr = s[:i], s[i+1:]
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port <= 0 || port > 65535 {
		return "", 0, fmt.Errorf("invalid port %q", portStr)
	}
	return host, port, nil
}

// buildStream builds the xray streamSettings object from parsed node fields.
func buildStream(network, security, host, path, sni, fp, alpn string, insecure bool, pbk, sid, spx, headerType, serviceName, mode string) map[string]any {
	s := map[string]any{"network": network}
	switch network {
	case "ws":
		s["wsSettings"] = map[string]any{"path": path, "headers": map[string]any{"Host": host}}
	case "grpc":
		g := map[string]any{}
		if serviceName != "" {
			g["serviceName"] = serviceName
		}
		if host != "" {
			g["authority"] = host
		}
		s["grpcSettings"] = g
	case "httpupgrade":
		h := map[string]any{"path": path}
		if host != "" {
			h["host"] = host
		}
		s["httpupgradeSettings"] = h
	case "xhttp":
		h := map[string]any{"path": path}
		if host != "" {
			h["host"] = host
		}
		s["xhttpSettings"] = h
	case "kcp":
		s["kcpSettings"] = map[string]any{}
	case "quic":
		s["quicSettings"] = map[string]any{}
	case "tcp":
		if headerType == "http" {
			// http header obfuscation is rare and complex; skip it
		}
	}

	sec := security
	if sec == "" {
		sec = "none"
	}
	if sec != "none" {
		s["security"] = sec
	}
	switch sec {
	case "tls":
		if ts := buildTLSSettings(sni, host, fp, alpn, insecure); len(ts) > 0 {
			s["tlsSettings"] = ts
		}
	case "xtls":
		if ts := buildTLSSettings(sni, host, fp, alpn, insecure); len(ts) > 0 {
			s["xtlsSettings"] = ts
		}
	case "reality":
		rs := map[string]any{}
		sn := sni
		if sn == "" {
			sn = host
		}
		if sn != "" {
			rs["serverName"] = sn
		}
		if fp != "" {
			rs["fingerprint"] = fp
		}
		if pbk != "" {
			rs["publicKey"] = pbk
		}
		if sid != "" {
			rs["shortId"] = sid
		}
		if spx != "" {
			rs["spiderX"] = spx
		}
		if len(rs) > 0 {
			s["realitySettings"] = rs
		}
	}
	return s
}

func buildTLSSettings(sni, host, fp, alpn string, insecure bool) map[string]any {
	ts := map[string]any{}
	sn := sni
	if sn == "" {
		sn = host
	}
	if sn != "" {
		ts["serverName"] = sn
	}
	if fp != "" {
		ts["fingerprint"] = fp
	}
	if alpn != "" {
		ts["alpn"] = strings.Split(alpn, ",")
	}
	if insecure {
		ts["allowInsecure"] = true
	}
	return ts
}
