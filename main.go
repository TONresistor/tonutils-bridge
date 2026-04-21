package main

import (
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	tunnelConfig "github.com/ton-blockchain/adnl-tunnel/config"
	"github.com/ton-blockchain/adnl-tunnel/tunnel"
	"github.com/xssnick/tonutils-go/address"
	"github.com/xssnick/tonutils-go/adnl"
	"github.com/xssnick/tonutils-go/adnl/dht"
	"github.com/xssnick/tonutils-go/adnl/keys"
	"github.com/xssnick/tonutils-go/adnl/overlay"
	"github.com/xssnick/tonutils-go/liteclient"
	"github.com/xssnick/tonutils-go/tl"
	"github.com/xssnick/tonutils-go/ton"
	"github.com/xssnick/tonutils-go/ton/dns"

	"github.com/TONresistor/tonutils-bridge/wsbridge"
)

const defaultConfigURL = "https://ton-blockchain.github.io/global.config.json"

func main() {
	addr := flag.String("addr", "", "WebSocket bridge listen address (overrides config)")
	configPath := flag.String("config", "", "Path to TON global config JSON (overrides config)")
	dataDir := flag.String("data-dir", ".", "Directory for persistent data (config.json, ADNL key)")
	tunnelSections := flag.Int("tunnel", -1, "Number of tunnel sections (overrides config; 0=disabled, >=2 to enable)")
	verbosity := flag.Int("verbosity", -1, "Log verbosity (overrides config; 0=fatal, 1=error, 2=info, 3=debug)")
	flag.Parse()

	// 1. Load config
	cfg, err := LoadConfig(*dataDir)
	if err != nil {
		log.Fatal().Err(err).Msg("Failed to load config")
	}

	// 2. Apply CLI overrides (flags take precedence over config)
	if *addr != "" {
		cfg.Listen = *addr
	}
	if *configPath != "" {
		cfg.TonConfig = *configPath
	}
	if *tunnelSections >= 0 {
		cfg.TunnelSections = *tunnelSections
	}
	if *verbosity >= 0 {
		cfg.Verbosity = *verbosity
	}

	// Logging
	zerolog.SetGlobalLevel([]zerolog.Level{
		zerolog.FatalLevel, zerolog.ErrorLevel, zerolog.InfoLevel, zerolog.DebugLevel,
	}[max(0, min(cfg.Verbosity, 3))])
	log.Logger = log.Output(zerolog.ConsoleWriter{Out: os.Stderr, TimeFormat: time.Kitchen})

	if err := run(cfg); err != nil {
		log.Fatal().Err(err).Msg("Bridge stopped")
	}
}

func run(cfg *Config) error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// 1. Load persistent ADNL key
	privKey := ed25519.NewKeyFromSeed(cfg.ADNLKey)

	// 2. Load TON network config
	log.Info().Msg("Loading TON network config...")
	lsCfg, err := loadConfig(cfg.TonConfig)
	if err != nil {
		return fmt.Errorf("failed to load TON config: %w", err)
	}

	// 3. Init liteserver connection pool + DNS resolver
	log.Info().Msg("Initializing liteserver pool and DNS resolver...")
	connPool, dnsClient, err := initDNSResolver(ctx, lsCfg)
	if err != nil {
		return fmt.Errorf("failed to init DNS resolver: %w", err)
	}
	defer connPool.Stop()

	// 4. Network manager (tunnel or direct)
	var netMgr adnl.NetManager
	if cfg.TunnelSections >= 2 {
		log.Info().Int("sections", cfg.TunnelSections).Msg("Starting ADNL tunnel...")
		nm, err := startTunnel(ctx, lsCfg, cfg.TunnelSections)
		if err != nil {
			return fmt.Errorf("tunnel init failed: %w", err)
		}
		netMgr = nm
	} else {
		dl, err := adnl.DefaultListener(":")
		if err != nil {
			return fmt.Errorf("failed to create listener: %w", err)
		}
		netMgr = adnl.NewMultiNetReader(dl)
	}
	defer netMgr.Close()

	// 5. DHT client (ephemeral key)
	log.Info().Msg("Initializing DHT client...")
	_, dhtKey, err := ed25519.GenerateKey(nil)
	if err != nil {
		return fmt.Errorf("failed to generate DHT key: %w", err)
	}
	dhtGate := adnl.NewGatewayWithNetManager(dhtKey, netMgr)
	if err = dhtGate.StartClient(); err != nil {
		return fmt.Errorf("failed to start DHT gateway: %w", err)
	}
	defer dhtGate.Close()

	dhtClient, err := dht.NewClientFromConfig(dhtGate, lsCfg)
	if err != nil {
		return fmt.Errorf("failed to init DHT client: %w", err)
	}
	defer dhtClient.Close()

	// 6. ADNL gateway for the WS bridge (persistent key)
	wsGate := adnl.NewGatewayWithNetManager(privKey, netMgr)
	if err = wsGate.StartClient(); err != nil {
		return fmt.Errorf("failed to start WS bridge gateway: %w", err)
	}
	defer wsGate.Close()

	// 7. Create and start bridge
	wsAPI := ton.NewAPIClient(connPool, ton.ProofCheckPolicyFast).WithRetry(2).WithTimeout(5 * time.Second)
	bridge := wsbridge.NewWSBridge(cfg.ToWSBridgeConfig(), dhtClient, wsAPI, dnsClient, wsGate, privKey)

	log.Info().Str("addr", cfg.Listen).Msg("Starting WebSocket bridge")
	return bridge.Start(ctx, cfg.Listen)
}

// startTunnel discovers relay nodes via DHT and starts an ADNL tunnel,
// returning a NetManager backed by the tunnel once it is ready.
func startTunnel(ctx context.Context, lsCfg *liteclient.GlobalConfig, sectionsNum int) (adnl.NetManager, error) {
	log.Info().Msg("Discovering tunnel relay nodes from DHT...")
	relays := discoverTunnelNodes(lsCfg)
	if len(relays) == 0 {
		return nil, fmt.Errorf("no tunnel relay nodes found via DHT")
	}
	log.Info().Int("count", len(relays)).Msg("Tunnel relay nodes discovered")

	tunCfg, err := tunnelConfig.GenerateClientConfig()
	if err != nil {
		return nil, fmt.Errorf("failed to generate tunnel config: %w", err)
	}
	tunCfg.TunnelSectionsNum = uint(sectionsNum)

	tunNodesCfg := tunnelConfig.SharedConfig{NodesPool: relays}

	tunnel.ChannelPacketsToPrepay = 30000
	tunnel.ChannelCapacityForNumPayments = 50

	events := make(chan any, 10)
	go tunnel.RunTunnel(ctx, tunCfg, &tunNodesCfg, nil, log.Logger, events)

	atm := &tunnel.AtomicSwitchableRegularTunnel{}
	initUpd := make(chan any, 1)

	go func() {
		for event := range events {
			switch e := event.(type) {
			case tunnel.StoppedEvent:
				return
			case tunnel.UpdatedEvent:
				log.Info().Msg("tunnel updated")
				atm.SwitchTo(e.Tunnel)
				select {
				case initUpd <- e:
				default:
				}
			case error:
				select {
				case initUpd <- e:
				default:
				}
			}
		}
	}()

	switch x := (<-initUpd).(type) {
	case tunnel.UpdatedEvent:
		log.Info().
			Str("ip", x.ExtIP.String()).
			Uint16("port", x.ExtPort).
			Msg("using tunnel")
	case error:
		return nil, fmt.Errorf("tunnel preparation failed: %w", x)
	}

	return adnl.NewMultiNetReader(atm), nil
}

// discoverTunnelNodes creates a temporary DHT client and queries for free
// tunnel relay nodes, returning them as TunnelRouteSection entries.
func discoverTunnelNodes(lsCfg *liteclient.GlobalConfig) []tunnelConfig.TunnelRouteSection {
	_, tmpKey, err := ed25519.GenerateKey(nil)
	if err != nil {
		log.Error().Err(err).Msg("Failed to generate temp DHT key")
		return nil
	}
	tmpGate := adnl.NewGateway(tmpKey)
	if err = tmpGate.StartClient(); err != nil {
		log.Error().Err(err).Msg("Failed to start temp DHT gateway")
		return nil
	}
	defer tmpGate.Close()

	tmpDHT, err := dht.NewClientFromConfig(tmpGate, lsCfg)
	if err != nil {
		log.Error().Err(err).Msg("Failed to create temp DHT client")
		return nil
	}
	defer tmpDHT.Close()

	// tl.Hash(OverlayKey{PaymentNode: [0...0]}) — free relay nodes
	overlayKey, err := tl.Hash(tunnel.OverlayKey{PaymentNode: make([]byte, 32)})
	if err != nil {
		log.Error().Err(err).Msg("Failed to compute tunnel overlay key")
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	var allNodes []overlay.Node
	var cont *dht.Continuation

	for i := 0; i < 3; i++ {
		nodesList, c, err := tmpDHT.FindOverlayNodes(ctx, overlayKey, cont)
		if err != nil {
			if i == 0 {
				log.Warn().Err(err).Msg("DHT tunnel relay discovery failed")
				return nil
			}
			break
		}
		if nodesList != nil {
			allNodes = append(allNodes, nodesList.List...)
		}
		if c == nil {
			break
		}
		cont = c
	}

	seen := make(map[string]bool)
	var sections []tunnelConfig.TunnelRouteSection
	for _, node := range allNodes {
		id, ok := node.ID.(keys.PublicKeyED25519)
		if !ok {
			continue
		}
		keyHex := hex.EncodeToString(id.Key)
		if seen[keyHex] {
			continue
		}
		seen[keyHex] = true
		sections = append(sections, tunnelConfig.TunnelRouteSection{Key: id.Key})
	}

	return sections
}

func loadConfig(path string) (*liteclient.GlobalConfig, error) {
	if path != "" {
		return liteclient.GetConfigFromFile(path)
	}
	// tonutils-go adds its own 10s timeout internally for URL fetches.
	return liteclient.GetConfigFromUrl(context.Background(), defaultConfigURL)
}

func initDNSResolver(ctx context.Context, cfg *liteclient.GlobalConfig) (*liteclient.ConnectionPool, *dns.Client, error) {
	pool := liteclient.NewConnectionPool()
	if err := pool.AddConnectionsFromConfig(ctx, cfg); err != nil {
		return nil, nil, err
	}

	api := ton.NewAPIClient(pool).WithTimeout(5 * time.Second).WithRetry()

	var root *address.Address
	var err error
	for i := 0; i < 5; i++ {
		root, err = dns.GetRootContractAddr(ctx, api)
		if err == nil {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if err != nil {
		return nil, nil, fmt.Errorf("failed to get DNS root contract: %w", err)
	}

	return pool, dns.NewDNSClient(api, root), nil
}
