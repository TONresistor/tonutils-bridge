package wsbridge

import "time"

// BridgeConfig holds the runtime configuration for WSBridge.
// Populated by main from the top-level Config struct.
type BridgeConfig struct {
	MaxClients     int
	AllowedOrigins []string
	APIKey         string

	// Pre-parsed durations
	WriteTimeout time.Duration
	PongDeadline time.Duration
	PingPeriod   time.Duration // computed: (PongDeadline * 9) / 10

	MaxInflight    int
	MaxMessageSize int64

	Namespaces NamespacesRuntimeConfig
}

// NamespacesRuntimeConfig holds per-namespace settings with pre-parsed durations.
type NamespacesRuntimeConfig struct {
	Lite           LiteRuntimeConfig
	Subscribe      SubscribeRuntimeConfig
	ADNL           ADNLRuntimeConfig
	Overlay        OverlayRuntimeConfig
	DHT            DHTRuntimeConfig
	SubscribeTrace TraceRuntimeConfig
	Jetton         NSRuntimeConfig
	NFT            NSRuntimeConfig
	DNS            NSRuntimeConfig
	Wallet         NSRuntimeConfig
	SBT            NSRuntimeConfig
	Payment        NSRuntimeConfig
	Network        NSRuntimeConfig
}

// NSRuntimeConfig is the base namespace runtime config.
type NSRuntimeConfig struct {
	Enabled bool
	Timeout time.Duration
}

type LiteRuntimeConfig struct {
	NSRuntimeConfig
	SendWaitTimeout time.Duration
	WatchTimeout    time.Duration
}

type SubscribeRuntimeConfig struct {
	NSRuntimeConfig
	MaxSubscriptions int
	MaxMultiAccounts int
	MaxConfigParams  int
}

type ADNLRuntimeConfig struct {
	NSRuntimeConfig
	MaxPeers        int
	QueryMaxTimeout time.Duration
	SSRFProtection  bool
}

type OverlayRuntimeConfig struct {
	NSRuntimeConfig
	MaxOverlays     int
	QueryMaxTimeout time.Duration
}

type DHTRuntimeConfig struct {
	NSRuntimeConfig
	TunnelTimeout time.Duration
	AllowWrite    bool
}

type TraceRuntimeConfig struct {
	NSRuntimeConfig
	MaxDepth          int
	DefaultDepth      int
	MaxMsgTimeout     time.Duration
	DefaultMsgTimeout time.Duration
	MaxResolvers      int
}

// DefaultBridgeConfig returns a BridgeConfig matching the previous hardcoded behaviour.
func DefaultBridgeConfig() *BridgeConfig {
	return &BridgeConfig{
		MaxClients:     100,
		AllowedOrigins: []string{"127.0.0.1", "localhost", "::1"},
		WriteTimeout:   10 * time.Second,
		PongDeadline:   60 * time.Second,
		PingPeriod:     54 * time.Second,
		MaxInflight:    100,
		MaxMessageSize: 1 << 20,
		Namespaces: NamespacesRuntimeConfig{
			Lite: LiteRuntimeConfig{
				NSRuntimeConfig: NSRuntimeConfig{Enabled: true, Timeout: 10 * time.Second},
				SendWaitTimeout: 60 * time.Second,
				WatchTimeout:    180 * time.Second,
			},
			Subscribe: SubscribeRuntimeConfig{
				NSRuntimeConfig:  NSRuntimeConfig{Enabled: true},
				MaxSubscriptions: 50,
				MaxMultiAccounts: 100,
				MaxConfigParams:  50,
			},
			ADNL: ADNLRuntimeConfig{
				NSRuntimeConfig: NSRuntimeConfig{Enabled: true, Timeout: 10 * time.Second},
				MaxPeers:        20,
				QueryMaxTimeout: 60 * time.Second,
				SSRFProtection:  true,
			},
			Overlay: OverlayRuntimeConfig{
				NSRuntimeConfig: NSRuntimeConfig{Enabled: true, Timeout: 10 * time.Second},
				MaxOverlays:     10,
				QueryMaxTimeout: 60 * time.Second,
			},
			DHT: DHTRuntimeConfig{
				NSRuntimeConfig: NSRuntimeConfig{Enabled: true, Timeout: 15 * time.Second},
				TunnelTimeout:   30 * time.Second,
				AllowWrite:      false,
			},
			SubscribeTrace: TraceRuntimeConfig{
				NSRuntimeConfig:   NSRuntimeConfig{Enabled: true},
				MaxDepth:          10,
				DefaultDepth:      3,
				MaxMsgTimeout:     120 * time.Second,
				DefaultMsgTimeout: 10 * time.Second,
				MaxResolvers:      50,
			},
			Jetton:  NSRuntimeConfig{Enabled: true, Timeout: 10 * time.Second},
			NFT:     NSRuntimeConfig{Enabled: true, Timeout: 10 * time.Second},
			DNS:     NSRuntimeConfig{Enabled: true, Timeout: 10 * time.Second},
			Wallet:  NSRuntimeConfig{Enabled: true, Timeout: 10 * time.Second},
			SBT:     NSRuntimeConfig{Enabled: true, Timeout: 10 * time.Second},
			Payment: NSRuntimeConfig{Enabled: true, Timeout: 10 * time.Second},
			Network: NSRuntimeConfig{Enabled: true},
		},
	}
}

// namespaceEnabled returns whether the given JSON-RPC method's namespace is enabled.
func (c *BridgeConfig) namespaceEnabled(method string) bool {
	// Extract namespace prefix
	ns := method
	for i, ch := range method {
		if ch == '.' {
			ns = method[:i]
			break
		}
	}

	// Special case: subscribe.trace uses its own config but is also gated by subscribe
	if method == "subscribe.trace" {
		return c.Namespaces.Subscribe.Enabled && c.Namespaces.SubscribeTrace.Enabled
	}

	switch ns {
	case "lite":
		return c.Namespaces.Lite.Enabled
	case "subscribe":
		return c.Namespaces.Subscribe.Enabled
	case "adnl":
		return c.Namespaces.ADNL.Enabled
	case "overlay":
		return c.Namespaces.Overlay.Enabled
	case "dht":
		return c.Namespaces.DHT.Enabled
	case "jetton":
		return c.Namespaces.Jetton.Enabled
	case "nft":
		return c.Namespaces.NFT.Enabled
	case "dns":
		return c.Namespaces.DNS.Enabled
	case "wallet":
		return c.Namespaces.Wallet.Enabled
	case "sbt":
		return c.Namespaces.SBT.Enabled
	case "payment":
		return c.Namespaces.Payment.Enabled
	case "network":
		return c.Namespaces.Network.Enabled
	default:
		return true
	}
}
