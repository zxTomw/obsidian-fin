package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDefaultConfigAndStateLocations(t *testing.T) {
	userHome, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	configPath, err := DefaultConfigPath()
	if err != nil {
		t.Fatal(err)
	}
	if configPath != filepath.Join(userHome, ".config", "obsidian-fin", "config.toml") {
		t.Fatalf("unexpected default config path: %s", configPath)
	}
	stateRoot, err := DefaultRoot()
	if err != nil {
		t.Fatal(err)
	}
	if stateRoot != filepath.Join(userHome, "Library", "Application Support", "obsidian-fin") {
		t.Fatalf("unexpected default state root: %s", stateRoot)
	}
	// Exercise default-file loading with private fixture paths, without changing HOME or reading user configuration.
	fixtureRoot := t.TempDir()
	fixtureConfig := filepath.Join(fixtureRoot, ".config", "obsidian-fin", "config.toml")
	if err := os.MkdirAll(filepath.Dir(fixtureConfig), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fixtureConfig, []byte("client_id='example-client'\nenvironment='sandbox'\ncountry='CA'\nhistory_days=90\n"), 0600); err != nil {
		t.Fatal(err)
	}
	fixtureState := filepath.Join(fixtureRoot, "Library", "Application Support", "obsidian-fin")
	cfg, err := loadFile(Flags{}, fixtureState, fixtureConfig)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ClientID != "example-client" || cfg.DataDir != fixtureState || cfg.StateDir() != filepath.Join(fixtureState, "sandbox") {
		t.Fatalf("configuration and state were not loaded independently: %+v", cfg)
	}
	if _, err := os.Stat(fixtureState); !os.IsNotExist(err) {
		t.Fatal("loading configuration created state")
	}
}
