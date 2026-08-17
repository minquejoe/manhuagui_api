package xrayproxy

import (
	"encoding/base64"
	"encoding/json"
	"testing"

	"github.com/Aoi-hosizora/manhuagui-api/internal/pkg/config"
)

func testProxyConfig() *config.ProxyConfig {
	return &config.ProxyConfig{
		Enabled:         true,
		XrayBin:         "./bin/xray",
		XrayConfig:      "./xray/config.json",
		SocksPort:       10808,
		HttpPort:        10809,
		ApiPort:         10810,
		ProbeInterval:   60,
		ProbeURL:        "https://www.manhuagui.com/",
		Subscriptions:   []string{"https://example.com/sub"},
		RefreshInterval: 3600,
	}
}

func TestParseVmess(t *testing.T) {
	payload := `{"v":"2","ps":"Test Node 😀","add":"127.0.0.1","port":"8443","id":"00000000-0000-0000-0000-000000000000","aid":"2","net":"ws","type":"none","host":"ws.example.com","path":"/ws","tls":"tls","sni":"ws.example.com","fp":"chrome","scy":"auto"}`
	line := "vmess://" + base64.StdEncoding.EncodeToString([]byte(payload))
	n, err := parseNode(line, 3)
	if err != nil {
		t.Fatalf("parseVmess failed: %v", err)
	}
	if n.Tag != "node-3" || n.Name != "Test Node 😀" {
		t.Fatalf("unexpected node: %+v", n)
	}
	ss := n.Outbound["streamSettings"].(map[string]any)
	if ss["network"] != "ws" || ss["security"] != "tls" {
		t.Fatalf("unexpected streamSettings: %v", ss)
	}
	vnext := n.Outbound["settings"].(map[string]any)["vnext"].([]any)
	user := vnext[0].(map[string]any)["users"].([]any)[0].(map[string]any)
	if user["alterId"] != 2.0 && user["alterId"] != 2 {
		t.Fatalf("unexpected alterId: %v", user["alterId"])
	}
}

func TestParseSS(t *testing.T) {
	// SIP002: base64(method:password)@host:port#name
	userinfo := base64.StdEncoding.EncodeToString([]byte("aes-256-gcm:secret"))
	line := "ss://" + userinfo + "@1.2.3.4:8388#%E6%B5%8B%E8%AF%95"
	n, err := parseNode(line, 0)
	if err != nil {
		t.Fatalf("parseSS sip002 failed: %v", err)
	}
	if n.Name != "测试" {
		t.Fatalf("unexpected name: %q", n.Name)
	}
	srv := n.Outbound["settings"].(map[string]any)["servers"].([]any)[0].(map[string]any)
	if srv["address"] != "1.2.3.4" || srv["port"] != 8388 || srv["method"] != "aes-256-gcm" || srv["password"] != "secret" {
		t.Fatalf("unexpected ss server: %v", srv)
	}

	// legacy: whole method:password@host:port base64-encoded, no literal @
	legacy := base64.StdEncoding.EncodeToString([]byte("chacha20-ietf-poly1305:pw@5.6.7.8:443"))
	line2 := "ss://" + legacy
	n2, err := parseNode(line2, 1)
	if err != nil {
		t.Fatalf("parseSS legacy failed: %v", err)
	}
	srv2 := n2.Outbound["settings"].(map[string]any)["servers"].([]any)[0].(map[string]any)
	if srv2["address"] != "5.6.7.8" || srv2["port"] != 443 || srv2["method"] != "chacha20-ietf-poly1305" {
		t.Fatalf("unexpected legacy ss server: %v", srv2)
	}

	// plaintext userinfo
	n3, err := parseNode("ss://aes-128-gcm:plain@9.9.9.9:10086#x", 2)
	if err != nil {
		t.Fatalf("parseSS plaintext failed: %v", err)
	}
	if n3.Name != "x" {
		t.Fatalf("unexpected name: %q", n3.Name)
	}
}

func TestParseVlessTrojan(t *testing.T) {
	vl, err := parseNode("vless://00000000-0000-0000-0000-000000000000@example.com:443?type=ws&security=tls&sni=example.com&path=%2Fws&host=example.com&fp=chrome&alpn=h2#VL-1", 0)
	if err != nil {
		t.Fatalf("parseVless failed: %v", err)
	}
	if vl.Name != "VL-1" || vl.Outbound["protocol"] != "vless" {
		t.Fatalf("unexpected vless node: %+v", vl)
	}
	ss := vl.Outbound["streamSettings"].(map[string]any)
	if ss["network"] != "ws" || ss["security"] != "tls" {
		t.Fatalf("unexpected vless stream: %v", ss)
	}

	tr, err := parseNode("trojan://password@1.2.3.4:443?sni=cdn.example.com&allowInsecure=1#TJ-1", 1)
	if err != nil {
		t.Fatalf("parseTrojan failed: %v", err)
	}
	srv := tr.Outbound["settings"].(map[string]any)["servers"].([]any)[0].(map[string]any)
	if srv["password"] != "password" || srv["address"] != "1.2.3.4" {
		t.Fatalf("unexpected trojan server: %v", srv)
	}
	tlsSettings := tr.Outbound["streamSettings"].(map[string]any)["tlsSettings"].(map[string]any)
	if tlsSettings["serverName"] != "cdn.example.com" || tlsSettings["allowInsecure"] != true {
		t.Fatalf("unexpected trojan tls: %v", tlsSettings)
	}
}

func TestParseNodesUnsupported(t *testing.T) {
	lines := []string{
		"vmess://" + base64.StdEncoding.EncodeToString([]byte(`{"v":"2","ps":"x","add":"127.0.0.1","port":"443","id":"00000000-0000-0000-0000-000000000000","aid":"0","net":"tcp","type":"none","tls":""}`)),
		"hysteria2://pw@1.2.3.4:443/?sni=x.com#HY2", // unsupported by vanilla xray
		"ssr://AAAA#SSR", // unsupported
		"",
		"# comment",
	}
	nodes, warnings := parseNodes(lines)
	if len(nodes) != 1 {
		t.Fatalf("expected 1 node, got %d", len(nodes))
	}
	if len(warnings) != 2 {
		t.Fatalf("expected 2 warnings, got %d: %v", len(warnings), warnings)
	}
}

func TestDecodeSubscription(t *testing.T) {
	plain := "vmess://abc\nss://def"
	enc := base64.StdEncoding.EncodeToString([]byte(plain))
	got, err := decodeSubscription([]byte(enc))
	if err != nil || got != plain {
		t.Fatalf("decodeSubscription failed: %v %q", err, got)
	}
	got2, err := decodeSubscription([]byte(plain))
	if err != nil || got2 != plain {
		t.Fatalf("plain decodeSubscription failed: %v %q", err, got2)
	}
}

func TestBuildXrayConfig(t *testing.T) {
	nodes, _ := parseNodes([]string{
		"ss://" + base64.StdEncoding.EncodeToString([]byte("aes-256-gcm:pw")) + "@1.2.3.4:8388",
		"vless://00000000-0000-0000-0000-000000000000@example.com:443?type=ws&security=tls&sni=x.com",
	})
	cfgMap := buildXrayConfig(testProxyConfig(), nodes)
	bs, err := json.Marshal(cfgMap)
	if err != nil {
		t.Fatalf("marshal failed: %v", err)
	}
	if !json.Valid(bs) {
		t.Fatal("generated config is not valid json")
	}
	outbounds := cfgMap["outbounds"].([]any)
	if len(outbounds) != 4 { // 2 nodes + direct + block
		t.Fatalf("unexpected outbound count: %d", len(outbounds))
	}
	balancers := cfgMap["routing"].(map[string]any)["balancers"].([]any)
	strategy := balancers[0].(map[string]any)["strategy"].(map[string]any)
	if strategy["type"] != "leastPing" {
		t.Fatalf("unexpected strategy: %v", strategy)
	}
	obs := cfgMap["observatory"].(map[string]any)
	if obs["probeUrl"] != "https://www.manhuagui.com/" {
		t.Fatalf("unexpected probeUrl: %v", obs["probeUrl"])
	}
}

func TestNodeAddressHelpers(t *testing.T) {
	// vmess-style (vnext)
	n1, _ := parseNode("vmess://"+base64.StdEncoding.EncodeToString([]byte(
		`{"v":"2","ps":"x","add":"vm.example.com","port":"443","id":"00000000-0000-0000-0000-000000000000","aid":"0","net":"tcp","type":"none","tls":"tls","sni":"vm.example.com"}`)), 0)
	if got := nodeAddress(n1); got != "vm.example.com" {
		t.Fatalf("vnext address: %q", got)
	}
	setNodeAddress(n1, "1.2.3.4")
	if got := nodeAddress(n1); got != "1.2.3.4" {
		t.Fatalf("vnext address after set: %q", got)
	}
	if sn := n1.Outbound["streamSettings"].(map[string]any)["tlsSettings"].(map[string]any)["serverName"]; sn != "vm.example.com" {
		t.Fatalf("serverName should stay the hostname, got %v", sn)
	}

	// ss-style (servers)
	n2, _ := parseNode("ss://"+base64.StdEncoding.EncodeToString([]byte("aes-256-gcm:pw"))+"@ss.example.com:8388", 1)
	if got := nodeAddress(n2); got != "ss.example.com" {
		t.Fatalf("servers address: %q", got)
	}
	setNodeAddress(n2, "5.6.7.8")
	if got := nodeAddress(n2); got != "5.6.7.8" {
		t.Fatalf("servers address after set: %q", got)
	}

	// IP addresses are left untouched
	n3, _ := parseNode("ss://"+base64.StdEncoding.EncodeToString([]byte("aes-256-gcm:pw"))+"@9.9.9.9:443", 2)
	kept, _ := resolveNodeAddresses([]*Node{n1, n2, n3})
	if len(kept) != 3 {
		t.Fatalf("expected 3 kept nodes, got %d", len(kept))
	}
}
