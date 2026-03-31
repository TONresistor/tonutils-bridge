package main

import (
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/rs/zerolog/log"

	"github.com/TONresistor/tonutils-bridge/wsbridge"
)

// WebSocketConfig holds WebSocket transport settings.
type WebSocketConfig struct {
	WriteTimeout   string `json:"write_timeout"`
	PongDeadline   string `json:"pong_deadline"`
	MaxInflight    int    `json:"max_inflight"`
	MaxMessageSize int64  `json:"max_message_size"`
}

// NamespaceConfig is the base config for a namespace.
// Enabled is a pointer to distinguish "absent" (nil -> default true) from "explicitly false".
type NamespaceConfig struct {
	Enabled *bool  `json:"enabled,omitempty"`
	Timeout string `json:"timeout,omitempty"`
}

// LiteConfig extends NamespaceConfig for the lite namespace.
type LiteConfig struct {
	NamespaceConfig
	SendWaitTimeout string `json:"send_wait_timeout,omitempty"`
	WatchTimeout    string `json:"watch_timeout,omitempty"`
}

// SubscribeConfig extends NamespaceConfig for the subscribe namespace.
type SubscribeConfig struct {
	NamespaceConfig
	MaxSubscriptions int `json:"max_subscriptions"`
	MaxMultiAccounts int `json:"max_multi_accounts"`
	MaxConfigParams  int `json:"max_config_params"`
}

// ADNLConfig extends NamespaceConfig for the adnl namespace.
type ADNLConfig struct {
	NamespaceConfig
	MaxPeers        int    `json:"max_peers"`
	QueryMaxTimeout string `json:"query_max_timeout,omitempty"`
	SSRFProtection  bool   `json:"ssrf_protection"`
}

// OverlayConfig extends NamespaceConfig for the overlay namespace.
type OverlayConfig struct {
	NamespaceConfig
	MaxOverlays     int    `json:"max_overlays"`
	QueryMaxTimeout string `json:"query_max_timeout,omitempty"`
}

// DHTConfig extends NamespaceConfig for the dht namespace.
type DHTConfig struct {
	NamespaceConfig
	TunnelTimeout string `json:"tunnel_timeout,omitempty"`
	AllowWrite    bool   `json:"allow_write"`
}

// TraceConfig holds settings for subscribe.trace.
type TraceConfig struct {
	NamespaceConfig
	MaxDepth         int    `json:"max_depth"`
	DefaultDepth     int    `json:"default_depth"`
	MaxMsgTimeout    string `json:"max_msg_timeout,omitempty"`
	DefaultMsgTimeout string `json:"default_msg_timeout,omitempty"`
	MaxResolvers     int    `json:"max_resolvers"`
}

// NamespacesConfig groups all namespace configurations.
type NamespacesConfig struct {
	Lite           LiteConfig      `json:"lite"`
	Subscribe      SubscribeConfig `json:"subscribe"`
	ADNL           ADNLConfig      `json:"adnl"`
	Overlay        OverlayConfig   `json:"overlay"`
	DHT            DHTConfig       `json:"dht"`
	SubscribeTrace TraceConfig     `json:"subscribe_trace"`
	Jetton         NamespaceConfig `json:"jetton"`
	NFT            NamespaceConfig `json:"nft"`
	DNS            NamespaceConfig `json:"dns"`
	Wallet         NamespaceConfig `json:"wallet"`
	SBT            NamespaceConfig `json:"sbt"`
	Payment        NamespaceConfig `json:"payment"`
	Network        NamespaceConfig `json:"network"`
}

// Config holds all bridge configuration.
type Config struct {
	Version        int              `json:"version"`
	ADNLKey        []byte           `json:"adnl_key"`
	Listen         string           `json:"listen"`
	Verbosity      int              `json:"verbosity"`
	TunnelSections int              `json:"tunnel_sections"`
	TonConfig      string           `json:"ton_config,omitempty"`
	MaxClients     int              `json:"max_clients"`
	AllowedOrigins []string         `json:"allowed_origins"`
	APIKey         string           `json:"api_key,omitempty"`
	WebSocket      WebSocketConfig  `json:"websocket"`
	Namespaces     NamespacesConfig `json:"namespaces"`
}

func boolPtr(v bool) *bool { return &v }

// IsEnabled returns whether the namespace is enabled, defaulting to true if unset.
func (n NamespaceConfig) IsEnabled() bool {
	if n.Enabled == nil {
		return true
	}
	return *n.Enabled
}

// DefaultConfig returns a Config with all fields set to match the current
// hardcoded behaviour. This is the v2 baseline.
func DefaultConfig() *Config {
	return &Config{
		Version:        2,
		Listen:         "127.0.0.1:8081",
		Verbosity:      2,
		TunnelSections: 0,
		MaxClients:     100,
		AllowedOrigins: []string{"127.0.0.1", "localhost", "::1"},
		WebSocket: WebSocketConfig{
			WriteTimeout:   "10s",
			PongDeadline:   "60s",
			MaxInflight:    100,
			MaxMessageSize: 1 << 20,
		},
		Namespaces: NamespacesConfig{
			Lite: LiteConfig{
				NamespaceConfig: NamespaceConfig{Enabled: boolPtr(true), Timeout: "10s"},
				SendWaitTimeout: "60s",
				WatchTimeout:    "180s",
			},
			Subscribe: SubscribeConfig{
				NamespaceConfig:  NamespaceConfig{Enabled: boolPtr(true)},
				MaxSubscriptions: 50,
				MaxMultiAccounts: 100,
				MaxConfigParams:  50,
			},
			ADNL: ADNLConfig{
				NamespaceConfig: NamespaceConfig{Enabled: boolPtr(true), Timeout: "10s"},
				MaxPeers:        20,
				QueryMaxTimeout: "60s",
				SSRFProtection:  true,
			},
			Overlay: OverlayConfig{
				NamespaceConfig: NamespaceConfig{Enabled: boolPtr(true), Timeout: "10s"},
				MaxOverlays:     10,
				QueryMaxTimeout: "60s",
			},
			DHT: DHTConfig{
				NamespaceConfig: NamespaceConfig{Enabled: boolPtr(true), Timeout: "15s"},
				TunnelTimeout:   "30s",
				AllowWrite:      false,
			},
			SubscribeTrace: TraceConfig{
				NamespaceConfig:   NamespaceConfig{Enabled: boolPtr(true)},
				MaxDepth:          10,
				DefaultDepth:      3,
				MaxMsgTimeout:     "120s",
				DefaultMsgTimeout: "10s",
				MaxResolvers:      50,
			},
			Jetton:  NamespaceConfig{Enabled: boolPtr(true), Timeout: "10s"},
			NFT:     NamespaceConfig{Enabled: boolPtr(true), Timeout: "10s"},
			DNS:     NamespaceConfig{Enabled: boolPtr(true), Timeout: "10s"},
			Wallet:  NamespaceConfig{Enabled: boolPtr(true), Timeout: "10s"},
			SBT:     NamespaceConfig{Enabled: boolPtr(true), Timeout: "10s"},
			Payment: NamespaceConfig{Enabled: boolPtr(true), Timeout: "10s"},
			Network: NamespaceConfig{Enabled: boolPtr(true)},
		},
	}
}

// applyDefaults fills zero-valued fields in cfg with defaults.
// Called after json.Unmarshal so only missing fields get defaults.
func applyDefaults(cfg *Config) {
	d := DefaultConfig()

	if cfg.Listen == "" {
		cfg.Listen = d.Listen
	}
	if cfg.MaxClients == 0 {
		cfg.MaxClients = d.MaxClients
	}
	if cfg.AllowedOrigins == nil {
		cfg.AllowedOrigins = d.AllowedOrigins
	}

	// WebSocket
	ws := &cfg.WebSocket
	if ws.WriteTimeout == "" {
		ws.WriteTimeout = d.WebSocket.WriteTimeout
	}
	if ws.PongDeadline == "" {
		ws.PongDeadline = d.WebSocket.PongDeadline
	}
	if ws.MaxInflight == 0 {
		ws.MaxInflight = d.WebSocket.MaxInflight
	}
	if ws.MaxMessageSize == 0 {
		ws.MaxMessageSize = d.WebSocket.MaxMessageSize
	}

	// Helper to default a NamespaceConfig
	nsDefault := func(ns *NamespaceConfig, def NamespaceConfig) {
		// Enabled defaults to the default value only when the entire
		// namespaces block was omitted (version 1 migration). When a v2
		// config explicitly sets enabled=false, json.Unmarshal produces
		// false and we must NOT overwrite it. We rely on applyDefaults
		// being called AFTER migration sets Version=2, so callers that
		// construct partial v2 configs must set Enabled explicitly.
		// For the v1 migration path, all namespace fields are zero and
		// we want them to match the defaults.
		if ns.Timeout == "" {
			ns.Timeout = def.Timeout
		}
	}

	// Lite
	nsDefault(&cfg.Namespaces.Lite.NamespaceConfig, d.Namespaces.Lite.NamespaceConfig)
	if cfg.Namespaces.Lite.SendWaitTimeout == "" {
		cfg.Namespaces.Lite.SendWaitTimeout = d.Namespaces.Lite.SendWaitTimeout
	}
	if cfg.Namespaces.Lite.WatchTimeout == "" {
		cfg.Namespaces.Lite.WatchTimeout = d.Namespaces.Lite.WatchTimeout
	}

	// Subscribe
	nsDefault(&cfg.Namespaces.Subscribe.NamespaceConfig, d.Namespaces.Subscribe.NamespaceConfig)
	if cfg.Namespaces.Subscribe.MaxSubscriptions == 0 {
		cfg.Namespaces.Subscribe.MaxSubscriptions = d.Namespaces.Subscribe.MaxSubscriptions
	}
	if cfg.Namespaces.Subscribe.MaxMultiAccounts == 0 {
		cfg.Namespaces.Subscribe.MaxMultiAccounts = d.Namespaces.Subscribe.MaxMultiAccounts
	}
	if cfg.Namespaces.Subscribe.MaxConfigParams == 0 {
		cfg.Namespaces.Subscribe.MaxConfigParams = d.Namespaces.Subscribe.MaxConfigParams
	}

	// ADNL
	nsDefault(&cfg.Namespaces.ADNL.NamespaceConfig, d.Namespaces.ADNL.NamespaceConfig)
	if cfg.Namespaces.ADNL.MaxPeers == 0 {
		cfg.Namespaces.ADNL.MaxPeers = d.Namespaces.ADNL.MaxPeers
	}
	if cfg.Namespaces.ADNL.QueryMaxTimeout == "" {
		cfg.Namespaces.ADNL.QueryMaxTimeout = d.Namespaces.ADNL.QueryMaxTimeout
	}

	// Overlay
	nsDefault(&cfg.Namespaces.Overlay.NamespaceConfig, d.Namespaces.Overlay.NamespaceConfig)
	if cfg.Namespaces.Overlay.MaxOverlays == 0 {
		cfg.Namespaces.Overlay.MaxOverlays = d.Namespaces.Overlay.MaxOverlays
	}
	if cfg.Namespaces.Overlay.QueryMaxTimeout == "" {
		cfg.Namespaces.Overlay.QueryMaxTimeout = d.Namespaces.Overlay.QueryMaxTimeout
	}

	// DHT
	nsDefault(&cfg.Namespaces.DHT.NamespaceConfig, d.Namespaces.DHT.NamespaceConfig)
	if cfg.Namespaces.DHT.TunnelTimeout == "" {
		cfg.Namespaces.DHT.TunnelTimeout = d.Namespaces.DHT.TunnelTimeout
	}

	// Trace
	nsDefault(&cfg.Namespaces.SubscribeTrace.NamespaceConfig, d.Namespaces.SubscribeTrace.NamespaceConfig)
	if cfg.Namespaces.SubscribeTrace.MaxDepth == 0 {
		cfg.Namespaces.SubscribeTrace.MaxDepth = d.Namespaces.SubscribeTrace.MaxDepth
	}
	if cfg.Namespaces.SubscribeTrace.DefaultDepth == 0 {
		cfg.Namespaces.SubscribeTrace.DefaultDepth = d.Namespaces.SubscribeTrace.DefaultDepth
	}
	if cfg.Namespaces.SubscribeTrace.MaxMsgTimeout == "" {
		cfg.Namespaces.SubscribeTrace.MaxMsgTimeout = d.Namespaces.SubscribeTrace.MaxMsgTimeout
	}
	if cfg.Namespaces.SubscribeTrace.DefaultMsgTimeout == "" {
		cfg.Namespaces.SubscribeTrace.DefaultMsgTimeout = d.Namespaces.SubscribeTrace.DefaultMsgTimeout
	}
	if cfg.Namespaces.SubscribeTrace.MaxResolvers == 0 {
		cfg.Namespaces.SubscribeTrace.MaxResolvers = d.Namespaces.SubscribeTrace.MaxResolvers
	}

	// Simple namespaces
	nsDefault(&cfg.Namespaces.Jetton, d.Namespaces.Jetton)
	nsDefault(&cfg.Namespaces.NFT, d.Namespaces.NFT)
	nsDefault(&cfg.Namespaces.DNS, d.Namespaces.DNS)
	nsDefault(&cfg.Namespaces.Wallet, d.Namespaces.Wallet)
	nsDefault(&cfg.Namespaces.SBT, d.Namespaces.SBT)
	nsDefault(&cfg.Namespaces.Payment, d.Namespaces.Payment)
}

// migrateV1 upgrades a v1 config (version + adnl_key only) to v2.
func migrateV1(cfg *Config) {
	cfg.Version = 2
	d := DefaultConfig()

	cfg.Listen = d.Listen
	cfg.Verbosity = d.Verbosity
	cfg.MaxClients = d.MaxClients
	cfg.AllowedOrigins = d.AllowedOrigins
	cfg.WebSocket = d.WebSocket
	cfg.Namespaces = d.Namespaces
}

// Validate checks that all config values are within valid bounds.
func (c *Config) Validate() error {
	if c.MaxClients < 1 {
		return fmt.Errorf("max_clients must be >= 1, got %d", c.MaxClients)
	}

	// Validate all duration strings
	durations := map[string]string{
		"websocket.write_timeout":                     c.WebSocket.WriteTimeout,
		"websocket.pong_deadline":                     c.WebSocket.PongDeadline,
		"namespaces.lite.timeout":                     c.Namespaces.Lite.Timeout,
		"namespaces.lite.send_wait_timeout":           c.Namespaces.Lite.SendWaitTimeout,
		"namespaces.lite.watch_timeout":               c.Namespaces.Lite.WatchTimeout,
		"namespaces.adnl.timeout":                     c.Namespaces.ADNL.Timeout,
		"namespaces.adnl.query_max_timeout":           c.Namespaces.ADNL.QueryMaxTimeout,
		"namespaces.overlay.timeout":                  c.Namespaces.Overlay.Timeout,
		"namespaces.overlay.query_max_timeout":        c.Namespaces.Overlay.QueryMaxTimeout,
		"namespaces.dht.timeout":                      c.Namespaces.DHT.Timeout,
		"namespaces.dht.tunnel_timeout":               c.Namespaces.DHT.TunnelTimeout,
		"namespaces.subscribe_trace.max_msg_timeout":  c.Namespaces.SubscribeTrace.MaxMsgTimeout,
		"namespaces.subscribe_trace.default_msg_timeout": c.Namespaces.SubscribeTrace.DefaultMsgTimeout,
		"namespaces.jetton.timeout":                   c.Namespaces.Jetton.Timeout,
		"namespaces.nft.timeout":                      c.Namespaces.NFT.Timeout,
		"namespaces.dns.timeout":                      c.Namespaces.DNS.Timeout,
		"namespaces.wallet.timeout":                   c.Namespaces.Wallet.Timeout,
		"namespaces.sbt.timeout":                      c.Namespaces.SBT.Timeout,
		"namespaces.payment.timeout":                  c.Namespaces.Payment.Timeout,
	}
	for field, val := range durations {
		if val == "" {
			continue
		}
		if _, err := time.ParseDuration(val); err != nil {
			return fmt.Errorf("%s: invalid duration %q: %w", field, val, err)
		}
	}

	if c.WebSocket.MaxInflight < 1 {
		return fmt.Errorf("websocket.max_inflight must be >= 1, got %d", c.WebSocket.MaxInflight)
	}
	if c.WebSocket.MaxMessageSize < 1 {
		return fmt.Errorf("websocket.max_message_size must be >= 1, got %d", c.WebSocket.MaxMessageSize)
	}
	if c.Namespaces.Subscribe.IsEnabled() && c.Namespaces.Subscribe.MaxSubscriptions < 1 {
		return fmt.Errorf("namespaces.subscribe.max_subscriptions must be >= 1, got %d", c.Namespaces.Subscribe.MaxSubscriptions)
	}
	if c.Namespaces.ADNL.IsEnabled() && c.Namespaces.ADNL.MaxPeers < 1 {
		return fmt.Errorf("namespaces.adnl.max_peers must be >= 1, got %d", c.Namespaces.ADNL.MaxPeers)
	}
	if c.Namespaces.Overlay.IsEnabled() && c.Namespaces.Overlay.MaxOverlays < 1 {
		return fmt.Errorf("namespaces.overlay.max_overlays must be >= 1, got %d", c.Namespaces.Overlay.MaxOverlays)
	}
	if c.Namespaces.SubscribeTrace.IsEnabled() && c.Namespaces.SubscribeTrace.MaxDepth < 1 {
		return fmt.Errorf("namespaces.subscribe_trace.max_depth must be >= 1, got %d", c.Namespaces.SubscribeTrace.MaxDepth)
	}
	if c.Namespaces.SubscribeTrace.IsEnabled() && c.Namespaces.SubscribeTrace.DefaultDepth < 1 {
		return fmt.Errorf("namespaces.subscribe_trace.default_depth must be >= 1, got %d", c.Namespaces.SubscribeTrace.DefaultDepth)
	}

	return nil
}

// parseDur parses a duration string. Returns 0 for empty strings.
func parseDur(s string) time.Duration {
	if s == "" {
		return 0
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 10 * time.Second
	}
	return d
}

// ToWSBridgeConfig converts the Config to a wsbridge.BridgeConfig with pre-parsed durations.
func (c *Config) ToWSBridgeConfig() *wsbridge.BridgeConfig {
	pongDeadline := parseDur(c.WebSocket.PongDeadline)
	return &wsbridge.BridgeConfig{
		MaxClients:     c.MaxClients,
		AllowedOrigins: c.AllowedOrigins,
		APIKey:         c.APIKey,
		WriteTimeout:   parseDur(c.WebSocket.WriteTimeout),
		PongDeadline:   pongDeadline,
		PingPeriod:     (pongDeadline * 9) / 10,
		MaxInflight:    c.WebSocket.MaxInflight,
		MaxMessageSize: c.WebSocket.MaxMessageSize,
		Namespaces: wsbridge.NamespacesRuntimeConfig{
			Lite: wsbridge.LiteRuntimeConfig{
				NSRuntimeConfig: wsbridge.NSRuntimeConfig{Enabled: c.Namespaces.Lite.IsEnabled(), Timeout: parseDur(c.Namespaces.Lite.Timeout)},
				SendWaitTimeout: parseDur(c.Namespaces.Lite.SendWaitTimeout),
				WatchTimeout:    parseDur(c.Namespaces.Lite.WatchTimeout),
			},
			Subscribe: wsbridge.SubscribeRuntimeConfig{
				NSRuntimeConfig:  wsbridge.NSRuntimeConfig{Enabled: c.Namespaces.Subscribe.IsEnabled()},
				MaxSubscriptions: c.Namespaces.Subscribe.MaxSubscriptions,
				MaxMultiAccounts: c.Namespaces.Subscribe.MaxMultiAccounts,
				MaxConfigParams:  c.Namespaces.Subscribe.MaxConfigParams,
			},
			ADNL: wsbridge.ADNLRuntimeConfig{
				NSRuntimeConfig: wsbridge.NSRuntimeConfig{Enabled: c.Namespaces.ADNL.IsEnabled(), Timeout: parseDur(c.Namespaces.ADNL.Timeout)},
				MaxPeers:        c.Namespaces.ADNL.MaxPeers,
				QueryMaxTimeout: parseDur(c.Namespaces.ADNL.QueryMaxTimeout),
				SSRFProtection:  c.Namespaces.ADNL.SSRFProtection,
			},
			Overlay: wsbridge.OverlayRuntimeConfig{
				NSRuntimeConfig: wsbridge.NSRuntimeConfig{Enabled: c.Namespaces.Overlay.IsEnabled(), Timeout: parseDur(c.Namespaces.Overlay.Timeout)},
				MaxOverlays:     c.Namespaces.Overlay.MaxOverlays,
				QueryMaxTimeout: parseDur(c.Namespaces.Overlay.QueryMaxTimeout),
			},
			DHT: wsbridge.DHTRuntimeConfig{
				NSRuntimeConfig: wsbridge.NSRuntimeConfig{Enabled: c.Namespaces.DHT.IsEnabled(), Timeout: parseDur(c.Namespaces.DHT.Timeout)},
				TunnelTimeout:   parseDur(c.Namespaces.DHT.TunnelTimeout),
				AllowWrite:      c.Namespaces.DHT.AllowWrite,
			},
			SubscribeTrace: wsbridge.TraceRuntimeConfig{
				NSRuntimeConfig:   wsbridge.NSRuntimeConfig{Enabled: c.Namespaces.SubscribeTrace.IsEnabled()},
				MaxDepth:          c.Namespaces.SubscribeTrace.MaxDepth,
				DefaultDepth:      c.Namespaces.SubscribeTrace.DefaultDepth,
				MaxMsgTimeout:     parseDur(c.Namespaces.SubscribeTrace.MaxMsgTimeout),
				DefaultMsgTimeout: parseDur(c.Namespaces.SubscribeTrace.DefaultMsgTimeout),
				MaxResolvers:      c.Namespaces.SubscribeTrace.MaxResolvers,
			},
			Jetton:  wsbridge.NSRuntimeConfig{Enabled: c.Namespaces.Jetton.IsEnabled(), Timeout: parseDur(c.Namespaces.Jetton.Timeout)},
			NFT:     wsbridge.NSRuntimeConfig{Enabled: c.Namespaces.NFT.IsEnabled(), Timeout: parseDur(c.Namespaces.NFT.Timeout)},
			DNS:     wsbridge.NSRuntimeConfig{Enabled: c.Namespaces.DNS.IsEnabled(), Timeout: parseDur(c.Namespaces.DNS.Timeout)},
			Wallet:  wsbridge.NSRuntimeConfig{Enabled: c.Namespaces.Wallet.IsEnabled(), Timeout: parseDur(c.Namespaces.Wallet.Timeout)},
			SBT:     wsbridge.NSRuntimeConfig{Enabled: c.Namespaces.SBT.IsEnabled(), Timeout: parseDur(c.Namespaces.SBT.Timeout)},
			Payment: wsbridge.NSRuntimeConfig{Enabled: c.Namespaces.Payment.IsEnabled(), Timeout: parseDur(c.Namespaces.Payment.Timeout)},
			Network: wsbridge.NSRuntimeConfig{Enabled: c.Namespaces.Network.IsEnabled()},
		},
	}
}

// LoadConfig reads or creates the bridge configuration.
func LoadConfig(dir string) (*Config, error) {
	path := dir + "/config.json"
	if _, err := os.Stat(path); os.IsNotExist(err) {
		// First launch: generate fresh config with new ADNL key
		_, priv, err := ed25519.GenerateKey(nil)
		if err != nil {
			return nil, err
		}

		cfg := DefaultConfig()
		cfg.ADNLKey = priv.Seed()

		if err = SaveConfig(dir, cfg); err != nil {
			return nil, err
		}

		return cfg, nil
	} else if err != nil {
		return nil, err
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var cfg Config
	if err = json.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}

	// Migrate v1 configs (version + adnl_key only)
	migrated := false
	if cfg.Version < 2 {
		migrateV1(&cfg)
		migrated = true
		log.Info().Msg("Migrated config.json from v1 to v2")
	}

	// Apply defaults for any missing fields (partial v2 configs)
	applyDefaults(&cfg)

	// Regenerate ADNL key if missing or invalid
	if cfg.ADNLKey == nil || len(cfg.ADNLKey) != ed25519.SeedSize {
		_, priv, err := ed25519.GenerateKey(nil)
		if err != nil {
			return nil, err
		}
		cfg.ADNLKey = priv.Seed()
		migrated = true
		log.Warn().Msg("Regenerated missing ADNL key")
	}

	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("config validation failed: %w", err)
	}

	if migrated {
		if err := SaveConfig(dir, &cfg); err != nil {
			log.Warn().Err(err).Msg("Failed to save migrated config")
		}
	}

	return &cfg, nil
}

func SaveConfig(dir string, cfg *Config) error {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}

	path := dir + "/config.json"

	data, err := json.MarshalIndent(cfg, "", "\t")
	if err != nil {
		return err
	}

	return os.WriteFile(path, data, 0600)
}
