package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"
)

// Config is everything mecraft keeps on disk, in one file (mecraft.json).
type Config struct {
	// HTTP is the address of the web UI.
	HTTP string `json:"http"`
	// OC is the address the OpenComputers client connects to.
	OC string `json:"oc"`
	// Token is the shared secret the OC client must send; empty: no check.
	Token string `json:"token"`
	// MaintainEvery is how often stock levels are checked, e.g. "5s".
	MaintainEvery string `json:"maintainEvery"`
	// Rules are the stock levels; edited from the web UI.
	Rules []Rule `json:"rules"`
}

func defaultConfig() Config {
	return Config{HTTP: ":8080", OC: ":7777", MaintainEvery: "5s", Rules: []Rule{}}
}

// Interval returns MaintainEvery as a duration.
func (c Config) Interval() (time.Duration, error) {
	d, err := time.ParseDuration(c.MaintainEvery)
	if err != nil {
		return 0, fmt.Errorf("maintainEvery: %w", err)
	}
	if d < 100*time.Millisecond {
		return 0, errors.New("maintainEvery: не меньше 100ms")
	}
	return d, nil
}

// LoadConfig reads path, filling missing fields with defaults. A missing
// file is created with the defaults.
func LoadConfig(path string) (Config, error) {
	cfg := defaultConfig()
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		if err := writeConfig(path, cfg); err != nil {
			return cfg, err
		}
		log.Printf("config: created %s with defaults", path)
		return cfg, nil
	}
	if err != nil {
		return cfg, err
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return cfg, fmt.Errorf("%s: %w", path, err)
	}
	if cfg.Rules == nil {
		cfg.Rules = []Rule{}
	}
	if _, err := cfg.Interval(); err != nil {
		return cfg, fmt.Errorf("%s: %w", path, err)
	}
	log.Printf("config: %s, %d stock levels", path, len(cfg.Rules))
	return cfg, nil
}

// writeConfig replaces path atomically.
func writeConfig(path string, cfg Config) error {
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	tmp, err := os.CreateTemp(filepath.Dir(path), ".mecraft-*.json")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return os.Rename(tmp.Name(), path)
}
