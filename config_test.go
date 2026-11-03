package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLoadConfigCreatesDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mecraft.json")
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.HTTP != ":8080" || cfg.OC != ":7777" || cfg.MaintainEvery != "5s" {
		t.Errorf("defaults: %+v", cfg)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("file not created: %v", err)
	}
	if !strings.Contains(string(data), `"maintainEvery": "5s"`) {
		t.Errorf("file: %s", data)
	}
}

func TestLoadConfigKeepsMissingDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mecraft.json")
	if err := os.WriteFile(path, []byte(`{"token":"abc"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Token != "abc" || cfg.HTTP != ":8080" || cfg.MaintainEvery != "5s" {
		t.Errorf("got %+v", cfg)
	}
}

func TestLoadConfigRejectsBadInterval(t *testing.T) {
	for _, v := range []string{`"5"`, `"1ms"`, `"soon"`} {
		path := filepath.Join(t.TempDir(), "mecraft.json")
		os.WriteFile(path, []byte(`{"maintainEvery":`+v+`}`), 0o644)
		if _, err := LoadConfig(path); err == nil {
			t.Errorf("maintainEvery %s: want error", v)
		}
	}
}

// Saving a rule must keep the other settings in the same file.
func TestRulesSaveKeepsSettings(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mecraft.json")
	os.WriteFile(path, []byte(`{"http":":9000","token":"secret","maintainEvery":"2s"}`), 0o644)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	rs := NewRules(path, cfg)
	rule := Rule{Key: "a|1|A", Name: "a", Damage: 1, Label: "A", Target: 10, Enabled: true}
	if err := rs.Put(rule); err != nil {
		t.Fatal(err)
	}
	again, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if again.HTTP != ":9000" || again.Token != "secret" || again.MaintainEvery != "2s" {
		t.Errorf("settings lost: %+v", again)
	}
	if len(again.Rules) != 1 || again.Rules[0] != rule {
		t.Errorf("rules: %+v", again.Rules)
	}
	if d, _ := again.Interval(); d != 2*time.Second {
		t.Errorf("interval %v", d)
	}
	if err := rs.Delete(rule.Key); err != nil {
		t.Fatal(err)
	}
	again, _ = LoadConfig(path)
	if len(again.Rules) != 0 || again.Token != "secret" {
		t.Errorf("after delete: %+v", again)
	}
}
