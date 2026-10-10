package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"obsidian-fin/internal/config"
	"obsidian-fin/internal/errs"
)

func TestFlagTOMLDefaultPrecedence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("client_id='example'\nenvironment='production'\ncountry='US'\nhistory_days=180\ndata_dir='/tmp/from-config'\nvault='/tmp/config-vault'\n"), 0600); err != nil {
		t.Fatal(err)
	}
	c, err := config.Load(config.Flags{Config: path, Environment: "sandbox", DataDir: "/tmp/from-flags", Vault: "/tmp/flag-vault"})
	if err != nil {
		t.Fatal(err)
	}
	if c.Environment != "sandbox" || c.DataDir != "/tmp/from-flags" || c.Vault != "/tmp/flag-vault" || c.Country != "US" || c.HistoryDays != 180 || c.ClientID != "example" {
		t.Fatalf("precedence: %+v", c)
	}
	if c.StateDir() != filepath.Join(c.DataDir, "sandbox") {
		t.Fatal("state not partitioned")
	}
}
func TestInvalidConfigurationDoesNotLeakValues(t *testing.T) {
	for _, body := range []string{"secret='DO-NOT-LOG'", "access_token='DO-NOT-LOG'", "client_id='DO-NOT-LOG\n'", "history_days=0", "environment='development'", "vault='relative'", "country='GB'", "vault=[invalid TOML"} {
		t.Run(body, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.toml")
			if err := os.WriteFile(path, []byte(body), 0600); err != nil {
				t.Fatal(err)
			}
			_, err := config.Load(config.Flags{Config: path})
			if errs.ExitCode(err) != 2 || strings.Contains(err.Error(), "DO-NOT-LOG") {
				t.Fatal("bad config accepted or leaked")
			}
		})
	}
	if _, err := config.Load(config.Flags{Config: filepath.Join(t.TempDir(), "missing")}); errs.ExitCode(err) != 2 {
		t.Fatal("explicit missing config ignored")
	}
}
