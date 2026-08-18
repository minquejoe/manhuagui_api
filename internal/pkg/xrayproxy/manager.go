package xrayproxy

import (
	"archive/zip"
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/Aoi-hosizora/manhuagui-api/internal/pkg/config"
	"github.com/Aoi-hosizora/manhuagui-api/internal/pkg/static"
	"github.com/sirupsen/logrus"
)

const (
	// defaultXrayVersion is the pinned Xray-core release tag used when neither
	// proxy.xray-version nor the GitHub latest API is available.
	defaultXrayVersion = "v26.3.27"
	xrayReleaseURL     = "https://github.com/XTLS/Xray-core/releases/download/%s/Xray-linux-%s.zip"
	xrayLatestAPI      = "https://api.github.com/repos/XTLS/Xray-core/releases/latest"
)

var (
	aliveRe = regexp.MustCompile(`the outbound (\S+) is alive:([0-9.]+)`)
	deadRe  = regexp.MustCompile(`the outbound (\S+) is dead`)
)

// Status is a snapshot of the proxy manager state, exposed via GET /v1/proxy/status.
type Status struct {
	Enabled         bool      `json:"enabled"`
	Ready           bool      `json:"ready"`
	NodeCount       int       `json:"node_count"`
	CurrentTag      string    `json:"current_tag,omitempty"`
	CurrentNode     string    `json:"current_node,omitempty"`
	CurrentLatency  int64     `json:"current_latency_ms,omitempty"`
	LastUpdated     time.Time `json:"last_updated,omitempty"`
	RefreshInterval uint64    `json:"refresh_interval_seconds"`
	ProbeInterval   uint64    `json:"probe_interval_seconds"`
	ProbeURL        string    `json:"probe_url"`
	Subscriptions   []string  `json:"subscriptions"`
}

// Manager owns the xray child process: it fetches the subscription, generates
// the xray config, (re)starts xray when the node list changes, and reports the
// currently selected lowest-latency node.
type Manager struct {
	cfg    *config.ProxyConfig
	logger *logrus.Logger

	mu          sync.RWMutex
	cmd         *exec.Cmd
	ready       bool
	configHash  string
	latency     map[string]int64  // tag -> latest measured latency in ms
	nodeNames   map[string]string // tag -> display name
	nodeCount   int
	lastUpdate  time.Time
	currentTag  string
	lastRefresh time.Time // last subscription fetch, for the self-heal check

	selectMu sync.Mutex // serializes selectBestNode rounds (initial + ticker)

	stopCh chan struct{}
	wg     sync.WaitGroup // loop goroutine
	scanWg sync.WaitGroup // log scanner goroutines
}

func NewManager(cfg *config.ProxyConfig, logger *logrus.Logger) *Manager {
	if cfg == nil {
		cfg = &config.ProxyConfig{}
	}
	return &Manager{
		cfg:       cfg,
		logger:    logger,
		latency:   make(map[string]int64),
		nodeNames: make(map[string]string),
	}
}

// Start launches the manager background loop. It never fails fatally: if the
// proxy cannot be prepared, the API keeps running with direct connections.
func (m *Manager) Start() {
	if !m.cfg.Enabled {
		m.logger.Warn("[xray] proxy is disabled in config (proxy.enabled)")
		return
	}
	if len(m.cfg.Subscriptions) == 0 {
		m.logger.Warn("[xray] proxy is enabled but no subscription configured (proxy.subscriptions)")
		return
	}
	m.stopCh = make(chan struct{})
	if err := m.ensureBinary(); err != nil {
		m.logger.Errorf("[xray] failed to prepare xray binary: %v", err)
		return
	}
	m.logger.Infof("[xray] manager started: %d subscription(s), refresh every %ds, probe every %ds",
		len(m.cfg.Subscriptions), m.cfg.RefreshInterval, m.cfg.ProbeInterval)
	m.wg.Add(1)
	go m.loop()
}

// Stop shuts down the manager and its xray child process.
func (m *Manager) Stop() {
	if m.stopCh == nil {
		return
	}
	close(m.stopCh)
	m.wg.Wait() // loop goroutine fully done (no more scanWg.Add)
	m.stopXray()
	m.scanWg.Wait() // log scanners exit once the process is dead
	m.logger.Info("[xray] manager stopped")
}

func (m *Manager) loop() {
	defer m.wg.Done()
	m.refresh()
	refreshTicker := time.NewTicker(time.Duration(m.cfg.RefreshInterval) * time.Second)
	probeTicker := time.NewTicker(time.Duration(m.cfg.ProbeInterval) * time.Second)
	healTicker := time.NewTicker(2 * time.Minute)
	defer refreshTicker.Stop()
	defer probeTicker.Stop()
	defer healTicker.Stop()

	// first node selection shortly after the first observatory round
	time.AfterFunc(15*time.Second, func() {
		m.selectMu.Lock()
		defer m.selectMu.Unlock()
		m.selectBestNode()
	})

	for {
		select {
		case <-m.stopCh:
			return
		case <-refreshTicker.C:
			m.refresh()
		case <-probeTicker.C:
			m.report()
			m.selectMu.Lock()
			m.selectBestNode()
			m.selectMu.Unlock()
		case <-healTicker.C:
			m.healCheck()
		}
	}
}

// healCheck refreshes the subscription when every node is dead, but is
// rate-limited to avoid hammering the subscription server.
func (m *Manager) healCheck() {
	m.mu.RLock()
	nodeCount, lastRefresh, alive := m.nodeCount, m.lastRefresh, len(m.latency)
	m.mu.RUnlock()
	if nodeCount > 0 && alive == 0 && time.Since(lastRefresh) > 2*time.Minute {
		m.logger.Warnf("[xray] all %d nodes are dead, refreshing subscription now", nodeCount)
		m.refresh()
	}
}

// refresh re-fetches the subscription, regenerates the xray config, and
// restarts xray if the node list changed.
func (m *Manager) refresh() {
	lines := make([]string, 0)
	for _, sub := range m.cfg.Subscriptions {
		body, err := m.fetchSubscription(sub)
		if err != nil {
			m.logger.Errorf("[xray] failed to fetch subscription %s: %v", sub, err)
			continue
		}
		text, err := decodeSubscription(body)
		if err != nil {
			m.logger.Errorf("[xray] failed to decode subscription %s: %v", sub, err)
			continue
		}
		lines = append(lines, strings.Split(text, "\n")...)
	}
	nodes, warnings := parseNodes(lines)
	for _, w := range warnings {
		m.logger.Warnf("[xray] %s", w)
	}
	if len(nodes) == 0 {
		m.logger.Error("[xray] no usable node parsed from subscriptions")
		return
	}

	// resolve node server hostnames to IPs via reliable public DNS (the system
	// resolver is often flaky for these CDN relay hostnames); unresolvable
	// nodes are dropped here
	nodes, resWarnings := resolveNodeAddresses(nodes)
	for _, w := range resWarnings {
		m.logger.Warnf("[xray] %s", w)
	}
	if len(nodes) == 0 {
		m.logger.Error("[xray] no usable node left after DNS resolution")
		return
	}

	cfgMap := buildXrayConfig(m.cfg, nodes)
	bs, err := json.MarshalIndent(cfgMap, "", "  ")
	if err != nil {
		m.logger.Errorf("[xray] failed to marshal xray config: %v", err)
		return
	}
	hash := hashOf(bs)

	m.mu.RLock()
	unchanged := hash == m.configHash && m.cmd != nil && m.cmd.Process != nil
	m.mu.RUnlock()
	if unchanged {
		m.logger.Infof("[xray] subscription unchanged (%d nodes), skip restart", len(nodes))
		return
	}

	dir := filepath.Dir(m.cfg.XrayConfig)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		m.logger.Errorf("[xray] failed to create config dir: %v", err)
		return
	}
	if err := os.WriteFile(m.cfg.XrayConfig, bs, 0o644); err != nil {
		m.logger.Errorf("[xray] failed to write xray config: %v", err)
		return
	}
	if err := m.testConfig(); err != nil {
		m.logger.Errorf("[xray] xray config test failed, keep old config: %v", err)
		return
	}

	names := make(map[string]string, len(nodes))
	for _, n := range nodes {
		names[n.Tag] = n.Name
	}
	now := time.Now()
	m.mu.Lock()
	m.configHash = hash
	m.nodeNames = names
	m.nodeCount = len(nodes)
	m.lastUpdate = now
	m.lastRefresh = now
	m.latency = make(map[string]int64)
	m.currentTag = ""
	m.mu.Unlock()

	if err := m.restartXray(); err != nil {
		m.logger.Errorf("[xray] failed to start xray: %v", err)
		return
	}
	m.logger.Infof("[xray] xray started with %d nodes, probing %s every %ds", len(nodes), m.cfg.ProbeURL, m.cfg.ProbeInterval)
}

// report logs the currently pinned/selected node and the probe latencies.
func (m *Manager) report() {
	m.mu.RLock()
	names := make(map[string]string, len(m.nodeNames))
	for k, v := range m.nodeNames {
		names[k] = v
	}
	latency := make(map[string]int64, len(m.latency))
	for k, v := range m.latency {
		latency[k] = v
	}
	m.mu.RUnlock()

	if len(latency) == 0 {
		m.logger.Debug("[xray] no probe result yet")
		return
	}

	type item struct {
		tag  string
		ms   int64
		name string
	}
	items := make([]item, 0, len(latency))
	for tag, ms := range latency {
		items = append(items, item{tag: tag, ms: ms, name: names[tag]})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].ms < items[j].ms })
	best := items[0]

	sel := ""
	if out, err := m.queryBalancer(); err == nil {
		sel = out
	}

	m.mu.Lock()
	if sel != "" {
		m.currentTag = sel
	} else {
		m.currentTag = best.tag
	}
	m.mu.Unlock()

	top := items
	if len(top) > 3 {
		top = top[:3]
	}
	parts := make([]string, 0, len(top))
	for _, it := range top {
		parts = append(parts, fmt.Sprintf("%s %dms", displayName(it.tag, it.name), it.ms))
	}
	if sel != "" {
		m.logger.Infof("[xray] balancer selected %s | top: %s", displayName(sel, names[sel]), strings.Join(parts, ", "))
	} else {
		m.logger.Infof("[xray] best node %s | top: %s", displayName(best.tag, best.name), strings.Join(parts, ", "))
	}
}

// selectBestNode measures every alive node against probe-url (from the
// observatory results) plus each probe-extra-url (temporarily pinning the node
// via the balancer override API and timing a request through the socks proxy),
// then pins the node with the lowest combined latency as the proxy node.
func (m *Manager) selectBestNode() {
	if len(m.cfg.ProbeExtraURLs) == 0 {
		return
	}
	m.mu.RLock()
	names := make(map[string]string, len(m.nodeNames))
	for k, v := range m.nodeNames {
		names[k] = v
	}
	latency := make(map[string]int64, len(m.latency))
	for k, v := range m.latency {
		latency[k] = v
	}
	m.mu.RUnlock()

	tags := make([]string, 0, len(latency))
	for tag := range latency {
		tags = append(tags, tag)
	}
	if len(tags) == 0 {
		m.logger.Debug("[xray] node selection skipped: no probe result yet")
		return
	}

	type score struct {
		tag     string
		name    string
		primary int64 // probe-url latency (observatory)
		extra   int64 // combined extra-url latencies
		total   int64
	}
	scores := make([]score, 0, len(tags))
	extraCount := 0
	extraOK := 0
	for _, tag := range tags {
		s := score{tag: tag, name: names[tag], primary: latency[tag]}
		dead := false
		for _, u := range m.cfg.ProbeExtraURLs {
			ms, ok := m.measureViaNode(tag, u)
			extraCount++
			if !ok {
				dead = true
				break
			}
			s.extra += ms
			extraOK++
		}
		if dead {
			m.logger.Warnf("[xray] node %s unreachable for extra probe, excluded", displayName(tag, names[tag]))
			continue
		}
		s.total = s.primary + s.extra
		scores = append(scores, s)
	}
	if len(scores) == 0 {
		m.logger.Warn("[xray] no node passed extra probing, keeping current selection")
		return
	}
	sort.Slice(scores, func(i, j int) bool { return scores[i].total < scores[j].total })
	best := scores[0]
	if err := m.overrideBalancer(best.tag); err != nil {
		m.logger.Errorf("[xray] failed to pin node %s: %v", displayName(best.tag, best.name), err)
		return
	}
	m.mu.Lock()
	m.currentTag = best.tag
	m.mu.Unlock()

	top := scores
	if len(top) > 3 {
		top = top[:3]
	}
	parts := make([]string, 0, len(top))
	for _, s := range top {
		parts = append(parts, fmt.Sprintf("%s %dms(manhuagui)+%dms(extra)=%dms", displayName(s.tag, s.name), s.primary, s.extra, s.total))
	}
	m.logger.Infof("[xray] pinned node %s | %s", displayName(best.tag, best.name), strings.Join(parts, ", "))
}

// measureViaNode temporarily pins the balancer to the given node and times an
// HTTP GET to the target url through the local socks proxy. Returns the
// latency in ms and whether the request succeeded.
func (m *Manager) measureViaNode(tag, rawURL string) (int64, bool) {
	if err := m.overrideBalancer(tag); err != nil {
		return 0, false
	}
	proxyURL, _ := url.Parse("socks5://" + m.SocksAddr())
	client := &http.Client{
		Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)},
		Timeout:   10 * time.Second,
	}
	start := time.Now()
	resp, err := client.Get(rawURL)
	if err != nil {
		return 0, false
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return time.Since(start).Milliseconds(), true
}

// overrideBalancer pins the balancer selection to the given outbound tag via
// the xray RoutingService.
func (m *Manager) overrideBalancer(tag string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, m.cfg.XrayBin, "api", "bo",
		fmt.Sprintf("--server=127.0.0.1:%d", m.cfg.ApiPort), "-b", "balancer", tag)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func displayName(tag, name string) string {
	if name == "" {
		return tag
	}
	return fmt.Sprintf("%s (%s)", tag, name)
}

// ---------- xray process control ----------

func (m *Manager) restartXray() error {
	m.stopXray()
	cmd := exec.Command(m.cfg.XrayBin, "run", "-c", m.cfg.XrayConfig)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	m.mu.Lock()
	m.cmd = cmd
	m.mu.Unlock()

	m.scanWg.Add(2)
	go func() {
		defer m.scanWg.Done()
		m.scanLog(stdout)
	}()
	go func() {
		defer m.scanWg.Done()
		m.scanLog(stderr)
	}()
	m.waitPort()
	return nil
}

func (m *Manager) stopXray() {
	m.mu.Lock()
	cmd := m.cmd
	m.cmd = nil
	m.ready = false
	m.mu.Unlock()
	if cmd == nil || cmd.Process == nil {
		return
	}
	done := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(done)
	}()
	_ = cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		_ = cmd.Process.Kill()
		<-done
	}
	m.logger.Info("[xray] xray process stopped")
}

func (m *Manager) waitPort() {
	addr := fmt.Sprintf("127.0.0.1:%d", m.cfg.SocksPort)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 300*time.Millisecond)
		if err == nil {
			conn.Close()
			m.mu.Lock()
			m.ready = true
			m.mu.Unlock()
			m.logger.Infof("[xray] SOCKS proxy ready at %s", addr)
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	m.logger.Warnf("[xray] SOCKS proxy %s did not become ready within 5s", addr)
}

// scanLog parses xray's stdout/stderr for observatory probe results.
func (m *Manager) scanLog(r io.Reader) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if mm := aliveRe.FindStringSubmatch(line); mm != nil {
			sec, err := strconv.ParseFloat(mm[2], 64)
			if err != nil {
				continue
			}
			ms := int64(sec * 1000)
			m.mu.Lock()
			m.latency[mm[1]] = ms
			m.mu.Unlock()
			continue
		}
		if mm := deadRe.FindStringSubmatch(line); mm != nil {
			m.mu.Lock()
			delete(m.latency, mm[1])
			m.mu.Unlock()
		}
	}
}

// testConfig validates the generated config with `xray run -test` before
// applying it, so an unsupported node never breaks the running proxy.
func (m *Manager) testConfig() error {
	cmd := exec.Command(m.cfg.XrayBin, "run", "-test", "-c", m.cfg.XrayConfig)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// queryBalancer asks the running xray (RoutingService) which outbound the
// balancer currently selects.
func (m *Manager) queryBalancer() (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, m.cfg.XrayBin, "api", "bi", "-json",
		fmt.Sprintf("--server=127.0.0.1:%d", m.cfg.ApiPort), "balancer")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", err
	}
	var res struct {
		Balancer *struct {
			Override        *struct{ Target string } `json:"override"`
			PrincipleTarget *struct{ Tag []string }  `json:"principleTarget"`
		} `json:"balancer"`
	}
	if err := json.Unmarshal(out, &res); err != nil {
		return "", err
	}
	if res.Balancer == nil {
		return "", fmt.Errorf("unexpected balancer response")
	}
	if res.Balancer.Override != nil && res.Balancer.Override.Target != "" {
		return res.Balancer.Override.Target, nil
	}
	if res.Balancer.PrincipleTarget != nil && len(res.Balancer.PrincipleTarget.Tag) > 0 {
		return res.Balancer.PrincipleTarget.Tag[0], nil
	}
	return "", fmt.Errorf("balancer has no selection")
}

// ---------- subscription fetching ----------

func (m *Manager) fetchSubscription(rawURL string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", static.USER_AGENT)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status %d", resp.StatusCode)
	}
	return io.ReadAll(resp.Body)
}

// ---------- binary install ----------

func (m *Manager) ensureBinary() error {
	bin := m.cfg.XrayBin
	if fi, err := os.Stat(bin); err == nil && !fi.IsDir() && fi.Size() > 0 {
		return nil
	}
	if !m.cfg.AutoInstall {
		return fmt.Errorf("xray binary %s not found, run scripts/setup_xray.sh or set proxy.auto-install: true", bin)
	}
	m.logger.Infof("[xray] downloading xray binary to %s ...", bin)
	if err := m.installXray(); err != nil {
		return fmt.Errorf("failed to download xray: %w", err)
	}
	return nil
}

func (m *Manager) installXray() error {
	version := strings.TrimSpace(m.cfg.XrayVersion)
	if version == "" {
		version = defaultXrayVersion
	}
	assetURL := fmt.Sprintf(xrayReleaseURL, version, xrayArchSuffix())
	bs, err := m.download(assetURL, 5*time.Minute)
	if err != nil {
		// fall back to the latest release
		latest, lerr := m.latestXrayVersion()
		if lerr != nil {
			return fmt.Errorf("download %s failed: %v", assetURL, err)
		}
		m.logger.Infof("[xray] %s unavailable, retrying with latest release %s", version, latest)
		version = latest
		assetURL = fmt.Sprintf(xrayReleaseURL, version, xrayArchSuffix())
		bs, err = m.download(assetURL, 5*time.Minute)
		if err != nil {
			return fmt.Errorf("download %s failed: %v", assetURL, err)
		}
	}
	return m.extractXray(bs, version)
}

func (m *Manager) latestXrayVersion() (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, xrayLatestAPI, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", static.USER_AGENT)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var rel struct {
		TagName string `json:"tag_name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return "", err
	}
	if rel.TagName == "" {
		return "", fmt.Errorf("empty tag_name")
	}
	return rel.TagName, nil
}

func (m *Manager) download(rawURL string, timeout time.Duration) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", static.USER_AGENT)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status %d", resp.StatusCode)
	}
	return io.ReadAll(resp.Body)
}

func (m *Manager) extractXray(bs []byte, version string) error {
	zr, err := zip.NewReader(bytes.NewReader(bs), int64(len(bs)))
	if err != nil {
		return fmt.Errorf("invalid zip: %v", err)
	}
	dir := filepath.Dir(m.cfg.XrayBin)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	found := false
	for _, f := range zr.File {
		if f.Name != "xray" && f.Name != "geoip.dat" && f.Name != "geosite.dat" {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return err
		}
		dstPath := filepath.Join(dir, f.Name)
		dst, err := os.OpenFile(dstPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
		if err != nil {
			rc.Close()
			return err
		}
		_, err = io.Copy(dst, rc)
		rc.Close()
		dst.Close()
		if err != nil {
			return err
		}
		if f.Name == "xray" {
			found = true
		}
	}
	if !found {
		return fmt.Errorf("xray binary not found in archive")
	}
	m.logger.Infof("[xray] xray %s installed at %s", version, m.cfg.XrayBin)
	return nil
}

func xrayArchSuffix() string {
	switch runtime.GOARCH {
	case "amd64":
		return "64"
	case "386":
		return "32"
	case "arm64":
		return "arm64-v8a"
	case "arm":
		return "arm32-v7a"
	default:
		return runtime.GOARCH
	}
}

// ---------- misc ----------

func hashOf(bs []byte) string {
	h := sha256.Sum256(bs)
	return hex.EncodeToString(h[:])
}

// IsReady reports whether the local SOCKS proxy is currently usable.
func (m *Manager) IsReady() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.ready
}

func (m *Manager) SocksAddr() string {
	return fmt.Sprintf("127.0.0.1:%d", m.cfg.SocksPort)
}

// ProxyURL returns the socks5 proxy URL used by the API's http transport.
func (m *Manager) ProxyURL() *url.URL {
	return &url.URL{Scheme: "socks5", Host: m.SocksAddr()}
}

// ShouldProxy reports whether requests to the given host should be routed
// through the xray proxy. Empty hosts list means the default manhuagui /
// hamreus domains.
func (m *Manager) ShouldProxy(host string) bool {
	if !m.cfg.Enabled {
		return false
	}
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	hosts := m.cfg.Hosts
	if len(hosts) == 0 {
		hosts = []string{".manhuagui.com", ".hamreus.com"}
	}
	for _, h := range hosts {
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

// Status returns a snapshot of the current proxy state.
func (m *Manager) Status() *Status {
	m.mu.RLock()
	defer m.mu.RUnlock()
	st := &Status{
		Enabled:         m.cfg.Enabled,
		Ready:           m.ready,
		NodeCount:       m.nodeCount,
		CurrentTag:      m.currentTag,
		LastUpdated:     m.lastUpdate,
		RefreshInterval: m.cfg.RefreshInterval,
		ProbeInterval:   m.cfg.ProbeInterval,
		ProbeURL:        m.cfg.ProbeURL,
		Subscriptions:   m.cfg.Subscriptions,
	}
	if m.currentTag != "" {
		st.CurrentNode = m.nodeNames[m.currentTag]
		st.CurrentLatency = m.latency[m.currentTag]
	}
	return st
}
