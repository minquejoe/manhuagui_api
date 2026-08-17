package config

import (
	"github.com/Aoi-hosizora/ahlib-mx/xvalidator"
	"github.com/Aoi-hosizora/ahlib/xdefault"
	"github.com/Aoi-hosizora/ahlib/xstring"
	"gopkg.in/yaml.v2"
	"os"
	"strings"
)

type Config struct {
	Meta    *MetaConfig    `yaml:"meta"    validate:"required"`
	Server  *ServerConfig  `yaml:"server"  validate:"required"`
	Message *MessageConfig `yaml:"message" validate:"required"`
	Proxy   *ProxyConfig   `yaml:"proxy"   validate:"omitempty"`
}

type MetaConfig struct {
	Port    uint16 `yaml:"port"     validate:"required"`
	Host    string `yaml:"host"     default:"0.0.0.0" validate:"ip"`
	RunMode string `yaml:"run-mode" default:"debug"`
	LogName string `yaml:"log-name" default:"./logs/console"`
	Pprof   bool   `yaml:"pprof"    default:"false"`
	Swagger bool   `yaml:"swagger"  default:"false"`
	DocHost string `yaml:"doc-host"`
}

type ServerConfig struct {
	BucketPeriod   uint64 `yaml:"bucket-period"   default:"60"  validate:"gt=0"`
	BucketCap      uint64 `yaml:"bucket-cap"      default:"200" validate:"gt=0"`
	BucketQua      uint64 `yaml:"bucket-qua"      default:"50"  validate:"gt=0"`
	BucketCleanup  uint64 `yaml:"bucket-cleanup"  default:"120" validate:"gt=0"`
	BucketSurvived uint16 `yaml:"bucket-survived" default:"3"   validate:"gt=0"`

	ServerCache bool   `yaml:"server-cache" default:"false"`
	CacheSize   uint16 `yaml:"cache-size"   default:"100" validate:"gt=0"`
	CacheExpire uint64 `yaml:"cache-expire" default:"180" validate:"gt=0"`
	ClientCache bool   `yaml:"client-cache" default:"false"`

	DefLimit uint32 `yaml:"def-limit"  default:"20"  validate:"gt=0"`
	MaxLimit uint32 `yaml:"max-limit"  default:"50"  validate:"gt=0"`
}

type MessageConfig struct {
	GitHubToken string `yaml:"github-token" validate:"required"`
}

// ProxyConfig enables an optional xray-based outbound proxy for the backend API.
// When enabled, all nodes parsed from `subscriptions` are written into an xray
// config, xray measures the HTTP latency of every node against `probe-url`
// (xray observatory), and the balancer (leastPing) routes the API's proxied
// requests through the node with the lowest latency. The subscription is
// re-fetched every `refresh-interval` seconds; latencies are re-probed every
// `probe-interval` seconds.
type ProxyConfig struct {
	Enabled bool `yaml:"enabled" default:"false"`

	AutoInstall bool   `yaml:"auto-install" default:"true"` // download xray binary when missing
	XrayVersion string `yaml:"xray-version"`                // optional pinned xray release tag, e.g. v26.3.27
	XrayBin     string `yaml:"xray-bin"     default:"./bin/xray"`
	XrayConfig  string `yaml:"xray-config"  default:"./xray/config.json"`

	SocksPort uint16 `yaml:"xray-socks-port" default:"10808" validate:"omitempty,gt=0"`
	HttpPort  uint16 `yaml:"xray-http-port"  default:"10809" validate:"omitempty,gt=0"`
	ApiPort   uint16 `yaml:"xray-api-port"   default:"10810" validate:"omitempty,gt=0"`

	Subscriptions []string `yaml:"subscriptions"` // v2ray-style subscription urls

	RefreshInterval uint64 `yaml:"refresh-interval" default:"3600" validate:"omitempty,gt=0"` // seconds, re-fetch subscription
	ProbeInterval   uint64 `yaml:"probe-interval"   default:"60"   validate:"omitempty,gt=0"` // seconds, re-probe node latency
	ProbeURL        string `yaml:"probe-url"        default:"https://www.manhuagui.com/"`

	// Hosts whose requests are routed through the proxy. Entries may start
	// with "." to match any subdomain. Defaults to manhuagui/hamreus domains.
	Hosts []string `yaml:"hosts"`
}

var _debugMode = true

func IsDebugMode() bool {
	return _debugMode
}

func Load(path string) (*Config, error) {
	f, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	cfg := &Config{}
	f = xstring.FastStob(os.ExpandEnv(xstring.FastBtos(f)))
	if err = yaml.Unmarshal(f, cfg); err != nil {
		return nil, err
	}
	if _, err = xdefault.FillDefaultFields(cfg); err != nil {
		return nil, err
	}
	if err = validateConfig(cfg); err != nil {
		return nil, err
	}

	_debugMode = strings.ToLower(cfg.Meta.RunMode) != "release"
	return cfg, nil
}

func validateConfig(cfg *Config) error {
	val := xvalidator.NewMessagedValidator()
	val.SetValidateTagName("validate")
	val.SetMessageTagName("message")
	val.UseTagAsFieldName("yaml", "json")

	err := val.ValidateStruct(cfg)
	if err != nil {
		ut, _ := xvalidator.ApplyEnglishTranslator(val.ValidateEngine())
		translated := err.(*xvalidator.MultiFieldsError).Translate(ut, false)
		return xvalidator.MergeMapToError(translated)
	}
	return nil
}
