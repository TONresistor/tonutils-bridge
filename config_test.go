package main

import (
	"crypto/ed25519"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestDefaultConfig(t *testing.T) {
	cfg := DefaultConfig()

	if cfg.Version != 2 {
		t.Fatalf("expected version 2, got %d", cfg.Version)
	}
	if cfg.Listen != "127.0.0.1:8081" {
		t.Fatalf("expected listen 127.0.0.1:8081, got %s", cfg.Listen)
	}
	if cfg.MaxClients != 100 {
		t.Fatalf("expected max_clients 100, got %d", cfg.MaxClients)
	}
	if cfg.Namespaces.DHT.AllowWrite != false {
		t.Fatal("expected dht.allow_write = false")
	}
	if !cfg.Namespaces.Lite.IsEnabled() {
		t.Fatal("expected lite.enabled = true")
	}
	if cfg.Namespaces.ADNL.MaxPeers != 20 {
		t.Fatalf("expected adnl.max_peers 20, got %d", cfg.Namespaces.ADNL.MaxPeers)
	}
	if cfg.Namespaces.Subscribe.MaxSubscriptions != 50 {
		t.Fatalf("expected subscribe.max_subscriptions 50, got %d", cfg.Namespaces.Subscribe.MaxSubscriptions)
	}
	if cfg.WebSocket.MaxMessageSize != 1<<20 {
		t.Fatalf("expected max_message_size 1MB, got %d", cfg.WebSocket.MaxMessageSize)
	}
}

func TestLoadConfig_FirstLaunch(t *testing.T) {
	dir := t.TempDir()
	cfg, err := LoadConfig(dir)
	if err != nil {
		t.Fatal(err)
	}

	if cfg.Version != 2 {
		t.Fatalf("expected version 2, got %d", cfg.Version)
	}
	if len(cfg.ADNLKey) != ed25519.SeedSize {
		t.Fatalf("expected ADNL key of %d bytes, got %d", ed25519.SeedSize, len(cfg.ADNLKey))
	}
	if cfg.MaxClients != 100 {
		t.Fatalf("expected default max_clients, got %d", cfg.MaxClients)
	}

	// File should exist on disk
	if _, err := os.Stat(filepath.Join(dir, "config.json")); err != nil {
		t.Fatalf("config.json not created: %v", err)
	}
}

func TestLoadConfig_MigrateV1(t *testing.T) {
	dir := t.TempDir()

	// Write a v1 config (just version + adnl_key)
	_, priv, _ := ed25519.GenerateKey(nil)
	v1 := map[string]any{
		"version":  1,
		"adnl_key": priv.Seed(),
	}
	data, _ := json.MarshalIndent(v1, "", "\t")
	os.WriteFile(filepath.Join(dir, "config.json"), data, 0600)

	cfg, err := LoadConfig(dir)
	if err != nil {
		t.Fatal(err)
	}

	if cfg.Version != 2 {
		t.Fatalf("expected migrated to version 2, got %d", cfg.Version)
	}
	if cfg.Listen != "127.0.0.1:8081" {
		t.Fatalf("expected default listen after migration, got %s", cfg.Listen)
	}
	if cfg.MaxClients != 100 {
		t.Fatalf("expected default max_clients after migration, got %d", cfg.MaxClients)
	}
	if !cfg.Namespaces.Lite.IsEnabled() {
		t.Fatal("expected lite enabled after migration")
	}
	if cfg.Namespaces.DHT.AllowWrite {
		t.Fatal("expected dht.allow_write false after migration")
	}
}

func TestLoadConfig_PartialV2(t *testing.T) {
	dir := t.TempDir()

	_, priv, _ := ed25519.GenerateKey(nil)
	partial := map[string]any{
		"version":    2,
		"adnl_key":   priv.Seed(),
		"listen":     "0.0.0.0:9090",
		"max_clients": 50,
		// All other fields omitted — should get defaults
	}
	data, _ := json.MarshalIndent(partial, "", "\t")
	os.WriteFile(filepath.Join(dir, "config.json"), data, 0600)

	cfg, err := LoadConfig(dir)
	if err != nil {
		t.Fatal(err)
	}

	if cfg.Listen != "0.0.0.0:9090" {
		t.Fatalf("expected custom listen, got %s", cfg.Listen)
	}
	if cfg.MaxClients != 50 {
		t.Fatalf("expected custom max_clients 50, got %d", cfg.MaxClients)
	}
	// Defaults should be applied for missing fields
	if cfg.WebSocket.WriteTimeout != "10s" {
		t.Fatalf("expected default write_timeout, got %s", cfg.WebSocket.WriteTimeout)
	}
	if cfg.Namespaces.ADNL.MaxPeers != 20 {
		t.Fatalf("expected default max_peers, got %d", cfg.Namespaces.ADNL.MaxPeers)
	}
}

func TestConfig_Validate_InvalidMaxClients(t *testing.T) {
	cfg := DefaultConfig()
	cfg.MaxClients = -1
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected validation error for negative max_clients")
	}
}

func TestConfig_Validate_InvalidTimeout(t *testing.T) {
	cfg := DefaultConfig()
	cfg.WebSocket.WriteTimeout = "not-a-duration"
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected validation error for invalid timeout")
	}
}

func TestConfig_Validate_ValidConfig(t *testing.T) {
	cfg := DefaultConfig()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("default config should be valid: %v", err)
	}
}

func TestConfig_ToWSBridgeConfig(t *testing.T) {
	cfg := DefaultConfig()
	_, priv, _ := ed25519.GenerateKey(nil)
	cfg.ADNLKey = priv.Seed()

	bc := cfg.ToWSBridgeConfig()

	if bc.MaxClients != 100 {
		t.Fatalf("expected max_clients 100, got %d", bc.MaxClients)
	}
	if bc.WriteTimeout.Seconds() != 10 {
		t.Fatalf("expected write_timeout 10s, got %v", bc.WriteTimeout)
	}
	if bc.PongDeadline.Seconds() != 60 {
		t.Fatalf("expected pong_deadline 60s, got %v", bc.PongDeadline)
	}
	if bc.PingPeriod.Seconds() != 54 {
		t.Fatalf("expected ping_period 54s, got %v", bc.PingPeriod)
	}
	if !bc.Namespaces.ADNL.SSRFProtection {
		t.Fatal("expected SSRF protection enabled")
	}
	if bc.Namespaces.DHT.AllowWrite {
		t.Fatal("expected DHT allow_write false")
	}
}
