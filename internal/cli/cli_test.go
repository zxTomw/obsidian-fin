package cli_test

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"obsidian-fin/internal/cli"
	"obsidian-fin/internal/config"
	"obsidian-fin/internal/domain"
	"obsidian-fin/internal/errs"
	"obsidian-fin/internal/fsutil"
	"obsidian-fin/internal/plaid"
	"obsidian-fin/internal/secrets"
	"obsidian-fin/internal/storage"
	"obsidian-fin/internal/testutil"
)

type provider struct {
	Creates, Exchanges int
	Environment        string
}

func (p *provider) CreateLink(_ context.Context, _ plaid.LinkRequest) (plaid.Link, error) {
	p.Creates++
	return plaid.Link{Token: "link-test", URL: "https://secure.plaid.com/hl/test"}, nil
}
func (*provider) Completion(context.Context, string, bool) (plaid.Completion, error) {
	return plaid.Completion{Success: true, Finished: true, PublicToken: "public-test"}, nil
}
func (p *provider) Exchange(context.Context, string) (plaid.Exchange, error) {
	p.Exchanges++
	return plaid.Exchange{ItemID: "item-test", AccessToken: "access-" + p.Environment + "-DO-NOT-LOG"}, nil
}
func (*provider) Metadata(context.Context, string) (plaid.Metadata, error) {
	return plaid.Metadata{ItemID: "item-test", InstitutionID: "ins-test", InstitutionName: "BMO Test", Accounts: []domain.Account{testutil.Account()}}, nil
}
func (*provider) Remove(context.Context, string) error { return nil }
func (*provider) Sync(context.Context, string, *string) (domain.Page, error) {
	return domain.Page{Added: []domain.Transaction{testutil.Transaction("transaction-private", false)}, NextCursor: "cursor-test"}, nil
}

func execute(t *testing.T, deps cli.Dependencies, args ...string) (string, string, error) {
	t.Helper()
	root := cli.New("test-version", deps)
	var out, diagnostics bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&diagnostics)
	root.SetArgs(args)
	err := cli.Execute(context.Background(), root)
	return out.String(), diagnostics.String(), err
}
func configuration(t *testing.T, dataDir string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("client_id='client-test'\ndata_dir='"+filepath.ToSlash(dataDir)+"'\n"), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestOfflineWorkflowAndExportWithoutKeychainOrNetwork(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "state")
	vault := t.TempDir()
	path := configuration(t, dataDir)
	memory := testutil.NewSecrets()
	p := &provider{Environment: "sandbox"}
	prompts := 0
	deps := cli.Dependencies{Secrets: memory, Provider: func(config.Config, string) (cli.Provider, error) { return p, nil }, Prompt: func(context.Context, io.Writer) (string, error) { prompts++; return "secret-DO-NOT-LOG", nil }, Open: func(context.Context, string) error { return nil }}
	for _, command := range [][]string{{"connect"}, {"connect"}, {"sync"}, {"export", "obsidian", "--vault", vault}, {"export", "obsidian", "--vault", vault}} {
		out, diagnostics, err := execute(t, deps, append([]string{"--config", path}, command...)...)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(out+diagnostics, "DO-NOT-LOG") || strings.Contains(out+diagnostics, "transaction-private") || strings.Contains(out+diagnostics, "1234") {
			t.Fatal("routine output leaked private contents")
		}
	}
	if p.Creates != 1 || p.Exchanges != 1 || prompts != 1 {
		t.Fatal("repeat connect repeated Link or prompt")
	}
	// Export must work even with secret access denied and a provider factory that fails the test if called.
	memory.GetError = secrets.ErrUnavailable
	deps.Provider = func(config.Config, string) (cli.Provider, error) { t.Fatal("export used the network"); return nil, nil }
	if out, _, err := execute(t, deps, "--config", path, "export", "obsidian", "--vault", vault); err != nil || !strings.Contains(out, "1 unchanged") {
		t.Fatalf("offline export: %s %v", out, err)
	}
	_, _, err := execute(t, deps, "--config", path, "--environment", "production", "export", "obsidian", "--vault", vault)
	if err == nil || !strings.Contains(err.Error(), "connect") {
		t.Fatal("production shared sandbox connection")
	}
	db := filepath.Join(dataDir, "sandbox", "obsidian-fin.db")
	content, err := os.ReadFile(db)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(content, []byte("DO-NOT-LOG")) {
		t.Fatal("SQLite contains a secret")
	}
	info, err := os.Stat(db)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("SQLite permissions not private")
	}
}

func TestUsageAndExitCodes(t *testing.T) {
	path := configuration(t, filepath.Join(t.TempDir(), "state"))
	for _, args := range [][]string{{"--unknown-secret", "DO-NOT-LOG"}, {"unknown-command"}, {"sync", "extra"}, {"export"}, {"export", "obsidian"}, {"--environment", "development", "sync"}, {"connect", "--secret", "DO-NOT-LOG"}} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			_, _, err := execute(t, cli.Dependencies{}, append([]string{"--config", path}, args...)...)
			if errs.ExitCode(err) != 2 || strings.Contains(err.Error(), "DO-NOT-LOG") {
				t.Fatalf("args=%v err=%v code=%d", args, err, errs.ExitCode(err))
			}
		})
	}
	for _, args := range [][]string{{"--help"}, {"--version"}, {"connect", "--help"}} {
		out, _, err := execute(t, cli.Dependencies{}, args...)
		if err != nil || out == "" {
			t.Fatal("help/version not available")
		}
	}
}
func TestMissingConnectionStateInVaultAndLockFailures(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "state")
	path := configuration(t, dataDir)
	_, _, err := execute(t, cli.Dependencies{}, "--config", path, "sync")
	if errs.ExitCode(err) != 1 || !strings.Contains(err.Error(), "connect") {
		t.Fatal("missing connection remedy")
	}
	lock, err := fsutil.AcquireLock(filepath.Join(dataDir, ".process.lock"))
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = execute(t, cli.Dependencies{}, "--config", path, "sync")
	if errs.ExitCode(err) != 1 || !strings.Contains(err.Error(), "lock") {
		t.Fatal("state lock bypassed")
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	vault := t.TempDir()
	inside := filepath.Join(vault, "private-state")
	_, _, err = execute(t, cli.Dependencies{}, "--config", path, "--data-dir", inside, "export", "obsidian", "--vault", vault)
	if errs.ExitCode(err) != 2 {
		t.Fatal("state allowed inside vault")
	}
	if _, err := os.Stat(inside); !os.IsNotExist(err) {
		t.Fatal("invalid state path was created")
	}
	root := t.TempDir()
	environmentVault := filepath.Join(root, "sandbox")
	if err := os.Mkdir(environmentVault, 0700); err != nil {
		t.Fatal(err)
	}
	_, _, err = execute(t, cli.Dependencies{}, "--config", path, "--data-dir", root, "export", "obsidian", "--vault", environmentVault)
	if errs.ExitCode(err) != 2 {
		t.Fatal("environment state was allowed inside vault")
	}
	if _, err := os.Stat(filepath.Join(environmentVault, "obsidian-fin.db")); !os.IsNotExist(err) {
		t.Fatal("created database inside vault")
	}
}

func TestCopiedDatabaseEnvironmentMismatchUsesConfigurationExitCode(t *testing.T) {
	root := t.TempDir()
	path := configuration(t, root)
	dbPath := filepath.Join(root, "production", "obsidian-fin.db")
	s, err := storage.Open(context.Background(), dbPath, "sandbox")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SaveConnection(context.Background(), testutil.Item(), []domain.Account{testutil.Account()}); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	_, _, err = execute(t, cli.Dependencies{}, "--config", path, "--environment", "production", "sync")
	if errs.ExitCode(err) != 2 || !strings.Contains(err.Error(), "environment") {
		t.Fatal("database mismatch did not fail as invalid configuration")
	}
}
