// Package config loads only non-secret TOML configuration, with explicit flag precedence.
package config

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/pelletier/go-toml/v2"
	"obsidian-fin/internal/errs"
)

type Config struct {
	ClientID    string `toml:"client_id"`
	Environment string `toml:"environment"`
	Country     string `toml:"country"`
	HistoryDays int    `toml:"history_days"`
	DataDir     string `toml:"data_dir"`
	Vault       string `toml:"vault"`
}
type Flags struct{ Config, DataDir, Environment, Vault string }

func DefaultRoot() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "Application Support", "obsidian-fin"), nil
}

func Load(flags Flags) (Config, error) {
	root, err := DefaultRoot()
	if err != nil {
		return Config{}, errs.Wrap(2, "cannot determine the default state directory", err)
	}
	c := Config{Environment: "sandbox", Country: "CA", HistoryDays: 90, DataDir: root}
	path := flags.Config
	if path == "" {
		path = filepath.Join(root, "config.toml")
	}
	content, err := os.ReadFile(path)
	if err != nil && (!errors.Is(err, os.ErrNotExist) || flags.Config != "") {
		return c, errs.Wrap(2, "cannot read configuration", err)
	}
	if err == nil {
		// Strict decoding rejects secrets and misspelled configuration keys without echoing their values.
		if err := toml.NewDecoder(bytes.NewReader(content)).DisallowUnknownFields().Decode(&c); err != nil {
			return c, errs.New(2, "invalid TOML configuration or unsupported keys; secrets belong in Keychain")
		}
	}
	if flags.DataDir != "" {
		c.DataDir = flags.DataDir
	}
	if flags.Environment != "" {
		c.Environment = flags.Environment
	}
	if flags.Vault != "" {
		c.Vault = flags.Vault
	}
	if c.Environment != "sandbox" && c.Environment != "production" {
		return c, errs.New(2, "environment must be sandbox or production")
	}
	if c.Country != "CA" && c.Country != "US" {
		return c, errs.New(2, "country must be CA or US for BMO")
	}
	if c.HistoryDays < 1 || c.HistoryDays > 730 {
		return c, errs.New(2, "history_days must be between 1 and 730")
	}
	if c.ClientID != "" && !regexp.MustCompile(`^[A-Za-z0-9_-]{1,100}$`).MatchString(c.ClientID) {
		return c, errs.New(2, "client_id has an invalid format")
	}
	for _, p := range []string{c.DataDir, c.Vault} {
		if strings.ContainsRune(p, 0) || (p != "" && !filepath.IsAbs(p)) {
			return c, errs.New(2, "data_dir and vault must be absolute paths")
		}
	}
	if c.DataDir == "" || filepath.Clean(c.DataDir) == string(filepath.Separator) {
		return c, errs.New(2, "data_dir must be a dedicated state directory")
	}
	c.DataDir = filepath.Clean(c.DataDir)
	return c, nil
}

func (c Config) StateDir() string { return filepath.Join(c.DataDir, c.Environment) }
