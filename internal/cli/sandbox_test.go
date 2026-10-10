//go:build sandbox

package cli_test

import (
	"os"
	"testing"

	"obsidian-fin/internal/cli"
)

// TestSandboxHostedLinkSmoke is explicitly opted in and uses the native Keychain.
// It retains the Sandbox Item in the selected state directory for later inspection.
func TestSandboxHostedLinkSmoke(t *testing.T) {
	if os.Getenv("OBSIDIAN_FIN_SANDBOX_SMOKE") != "1" {
		t.Skip("set OBSIDIAN_FIN_SANDBOX_SMOKE=1 and OBSIDIAN_FIN_SMOKE_CONFIG to opt in")
	}
	path := os.Getenv("OBSIDIAN_FIN_SMOKE_CONFIG")
	if path == "" {
		t.Fatal("OBSIDIAN_FIN_SMOKE_CONFIG must name a non-secret TOML file with client_id and an existing test vault")
	}
	for _, args := range [][]string{{"connect"}, {"connect"}, {"sync"}, {"export", "obsidian"}, {"export", "obsidian"}, {"connect", "--update"}} {
		out, diagnostics, err := execute(t, cli.Dependencies{}, append([]string{"--config", path, "--environment", "sandbox"}, args...)...)
		if err != nil {
			t.Fatalf("%v failed: %v", args, err)
		}
		t.Log(out, diagnostics)
	}
}
