package main

import (
	"crypto/ed25519"
	"encoding/json"
	"os"

	"github.com/rs/zerolog/log"
)

type Config struct {
	Version uint   `json:"version"`
	ADNLKey []byte `json:"adnl_key"` // ed25519 seed (32 bytes)
}

func LoadConfig(dir string) (*Config, error) {
	path := dir + "/config.json"
	if _, err := os.Stat(path); os.IsNotExist(err) {
		_, priv, err := ed25519.GenerateKey(nil)
		if err != nil {
			return nil, err
		}

		cfg := &Config{
			Version: 1,
			ADNLKey: priv.Seed(),
		}

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

	if cfg.ADNLKey == nil || len(cfg.ADNLKey) != ed25519.SeedSize {
		_, priv, err := ed25519.GenerateKey(nil)
		if err != nil {
			return nil, err
		}
		cfg.ADNLKey = priv.Seed()
		if err := SaveConfig(dir, &cfg); err != nil {
			log.Warn().Err(err).Msg("Failed to persist regenerated ADNL key")
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
