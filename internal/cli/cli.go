// Package cli defines only the explicitly scoped v1 commands.
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"obsidian-fin/internal/config"
	"obsidian-fin/internal/connect"
	"obsidian-fin/internal/errs"
	exporter "obsidian-fin/internal/export"
	"obsidian-fin/internal/fsutil"
	"obsidian-fin/internal/plaid"
	"obsidian-fin/internal/secrets"
	"obsidian-fin/internal/storage"
	syncer "obsidian-fin/internal/sync"
	"obsidian-fin/internal/terminal"
)

type Provider interface {
	connect.Provider
	syncer.Provider
}
type Dependencies struct {
	Secrets  secrets.Store
	Provider func(config.Config, string) (Provider, error)
	Prompt   func(context.Context, io.Writer) (string, error)
	Open     func(context.Context, string) error
}

// Execute translates Cobra's command-discovery failures without echoing arbitrary arguments.
func Execute(ctx context.Context, cmd *cobra.Command) error {
	err := cmd.ExecuteContext(ctx)
	if err != nil && strings.HasPrefix(err.Error(), "unknown command ") {
		return errs.New(2, "unknown command; run obsidian-fin --help")
	}
	return err
}

func New(version string, deps Dependencies) *cobra.Command {
	if deps.Secrets == nil {
		deps.Secrets = secrets.NewKeychain()
	}
	if deps.Provider == nil {
		deps.Provider = func(c config.Config, secret string) (Provider, error) {
			return plaid.New(c.Environment, c.ClientID, secret, c.Country)
		}
	}
	if deps.Prompt == nil {
		deps.Prompt = terminal.ReadSecret
	}
	if deps.Open == nil {
		deps.Open = terminal.OpenHosted
	}
	var flags config.Flags
	var cfg config.Config
	root := &cobra.Command{Use: "obsidian-fin", Short: "Manually sync BMO through Plaid into Obsidian", Version: version, SilenceUsage: true, SilenceErrors: true,
		CompletionOptions: cobra.CompletionOptions{DisableDefaultCmd: true},
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) > 0 {
				return errs.New(2, "unknown command; run obsidian-fin --help")
			}
			return cmd.Help()
		},
	}
	root.SetFlagErrorFunc(func(_ *cobra.Command, _ error) error {
		return errs.New(2, "invalid command-line flags; run obsidian-fin --help")
	})
	root.PersistentFlags().StringVar(&flags.Config, "config", "", "Path to non-secret TOML configuration")
	root.PersistentFlags().StringVar(&flags.DataDir, "data-dir", "", "Absolute path to the private state root")
	root.PersistentFlags().StringVar(&flags.Environment, "environment", "", "sandbox or production (default sandbox)")
	root.PersistentPreRunE = func(cmd *cobra.Command, args []string) error {
		for _, name := range []string{"config", "data-dir", "environment"} {
			flag := root.PersistentFlags().Lookup(name)
			if flag.Changed && flag.Value.String() == "" {
				return errs.New(2, "global path and environment flags cannot be empty")
			}
		}
		if flag := cmd.Flags().Lookup("vault"); flag != nil && flag.Changed && flags.Vault == "" {
			return errs.New(2, "vault flag cannot be empty")
		}
		var err error
		cfg, err = config.Load(flags)
		return err
	}
	noArgs := func(_ *cobra.Command, args []string) error {
		if len(args) > 0 {
			return errs.New(2, "unexpected positional arguments; run obsidian-fin --help")
		}
		return nil
	}
	var update bool
	connectCmd := &cobra.Command{Use: "connect", Short: "Connect BMO in the browser, or reauthenticate with --update", Args: noArgs}
	connectCmd.Flags().BoolVar(&update, "update", false, "Reauthenticate the existing Item without replacing it")
	connectCmd.RunE = func(cmd *cobra.Command, _ []string) error {
		return withStore(cmd.Context(), cfg, func(repo *storage.Store) error {
			// A repeated plain connect succeeds without Keychain/network access.
			item, err := repo.Item(cmd.Context())
			if err != nil {
				return errs.Wrap(1, "could not read connection", err)
			}
			state := connect.FileState{Dir: cfg.StateDir()}
			recovery, err := state.Recovery()
			if err != nil {
				return errs.Wrap(1, "could not read connection recovery", err)
			}
			if item != nil && recovery != nil && item.ID == recovery.ItemID && item.TokenRef == recovery.TokenRef {
				if err := state.ClearRecovery(); err != nil {
					return errs.Wrap(1, "connection is saved but recovery bookkeeping failed; rerun connect", err)
				}
				recovery = nil
			}
			if item == nil && update && recovery == nil {
				return errs.New(1, "no connection exists; run obsidian-fin connect")
			}
			if item != nil && !update && recovery == nil {
				if item.Environment != cfg.Environment || (cfg.ClientID != "" && !secrets.ValidateRef(item.TokenRef, cfg.Environment, cfg.ClientID)) {
					return errs.New(2, "connection environment/client mismatch")
				}
				fmt.Fprintf(cmd.OutOrStdout(), "Existing connection (%s); use connect --update to reauthenticate.\n", cfg.Environment)
				return nil
			}
			provider, err := getProvider(cmd, cfg, deps, true)
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.ErrOrStderr(), "Complete BMO authorization in the Plaid browser window; waiting up to 10 minutes.")
			result, err := connect.Run(cmd.Context(), repo, provider, deps.Secrets, state, connect.Options{Environment: cfg.Environment, ClientID: cfg.ClientID, Country: cfg.Country, HistoryDays: cfg.HistoryDays, Update: update, Open: deps.Open})
			if err != nil {
				return err
			}
			verb := "Connected"
			if result.Updated {
				verb = "Reauthenticated"
			}
			if result.Existing {
				verb = "Already connected"
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s (%s); %d authorized accounts.\n", verb, cfg.Environment, result.Accounts)
			return nil
		})
	}
	syncCmd := &cobra.Command{Use: "sync", Short: "Download available transaction changes into SQLite", Args: noArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		return withStore(cmd.Context(), cfg, func(repo *storage.Store) error {
			item, err := repo.Item(cmd.Context())
			if err != nil {
				return errs.Wrap(1, "could not read connection", err)
			}
			if item == nil {
				return errs.New(1, "no connection exists; run obsidian-fin connect")
			}
			if cfg.ClientID == "" {
				return errs.New(2, "set client_id in non-secret TOML configuration")
			}
			if !secrets.ValidateRef(item.TokenRef, cfg.Environment, cfg.ClientID) {
				return errs.New(2, "connection environment/client mismatch")
			}
			provider, err := getProvider(cmd, cfg, deps, false)
			if err != nil {
				return err
			}
			token, err := deps.Secrets.Get(cmd.Context(), item.TokenRef)
			if err != nil {
				return errs.Wrap(1, "could not read the connection token from Keychain", err)
			}
			if !secrets.ValidateToken(token, cfg.Environment) {
				return errs.New(2, "Keychain token environment mismatch")
			}
			fmt.Fprintln(cmd.ErrOrStderr(), "Downloading available Plaid changes.")
			counts, err := syncer.Run(cmd.Context(), repo, provider, *item, token)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Sync complete: %d added, %d modified, %d removed.\n", counts.Added, counts.Modified, counts.Removed)
			if counts.InitialEmpty {
				fmt.Fprintln(cmd.ErrOrStderr(), "Plaid may still be preparing initial history; run sync again later.")
			}
			return nil
		})
	}}
	exportCmd := &cobra.Command{Use: "export", Short: "Export committed data", RunE: func(*cobra.Command, []string) error { return errs.New(2, "specify export obsidian") }}
	obsidian := &cobra.Command{Use: "obsidian", Short: "Safely update managed notes in an existing Obsidian vault", Args: noArgs}
	obsidian.Flags().StringVar(&flags.Vault, "vault", "", "Absolute path to an existing Obsidian vault")
	obsidian.RunE = func(cmd *cobra.Command, _ []string) error {
		if cfg.Vault == "" {
			return errs.New(2, "set an absolute vault path with --vault or configuration")
		}
		return withStore(cmd.Context(), cfg, func(repo *storage.Store) error {
			item, err := repo.Item(cmd.Context())
			if err != nil {
				return errs.Wrap(1, "could not read connection", err)
			}
			if item == nil {
				return errs.New(1, "no connection exists; run obsidian-fin connect")
			}
			vault, err := exporter.OpenVault(cfg.Vault)
			if err != nil {
				code := 1
				if errors.Is(err, exporter.ErrConflict) {
					code = 3
				}
				return errs.Wrap(code, "cannot open or lock the managed vault subtree; check symlinks, permissions and overlapping exports", err)
			}
			defer vault.Close()
			counts, err := exporter.Run(cmd.Context(), repo, vault, cmd.ErrOrStderr())
			fmt.Fprintf(cmd.OutOrStdout(), "Export: %d created, %d updated, %d unchanged, %d conflicted, %d failed.\n", counts.Created, counts.Updated, counts.Unchanged, counts.Conflicted, counts.Failed)
			return err
		})
	}
	exportCmd.AddCommand(obsidian)
	root.AddCommand(connectCmd, syncCmd, exportCmd)
	return root
}

func getProvider(cmd *cobra.Command, cfg config.Config, deps Dependencies, allowPrompt bool) (Provider, error) {
	if cfg.ClientID == "" {
		return nil, errs.New(2, "set client_id in non-secret TOML configuration")
	}
	ref := secrets.ClientRef(cfg.Environment, cfg.ClientID)
	secret, err := deps.Secrets.Get(cmd.Context(), ref)
	if errors.Is(err, secrets.ErrNotFound) && allowPrompt {
		secret, err = deps.Prompt(cmd.Context(), cmd.ErrOrStderr())
		if err != nil {
			return nil, err
		}
		if err := deps.Secrets.Set(cmd.Context(), ref, secret); err != nil {
			return nil, errs.Wrap(1, "could not store the Plaid client secret in Keychain", err)
		}
	} else if err != nil {
		return nil, errs.Wrap(1, "could not read the Plaid client secret from Keychain; run connect interactively", err)
	}
	return deps.Provider(cfg, secret)
}
func resolved(path string) (string, error) {
	path = filepath.Clean(path)
	if p, err := filepath.EvalSymlinks(path); err == nil {
		return p, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	parent := filepath.Dir(path)
	if parent == path {
		return "", errors.New("cannot resolve state path")
	}
	p, err := resolved(parent)
	if err != nil {
		return "", err
	}
	return filepath.Join(p, filepath.Base(path)), nil
}
func withStore(ctx context.Context, cfg config.Config, fn func(*storage.Store) error) (err error) {
	if cfg.Vault != "" {
		vault, resolveErr := filepath.EvalSymlinks(cfg.Vault)
		if resolveErr != nil {
			return errs.New(2, "vault must be an existing directory")
		}
		info, statErr := os.Stat(vault)
		if statErr != nil || !info.IsDir() {
			return errs.New(2, "vault must be an existing directory")
		}
		for _, path := range []string{cfg.DataDir, cfg.StateDir()} {
			state, resolveErr := resolved(path)
			if resolveErr != nil {
				return errs.New(2, "cannot resolve state directory")
			}
			rel, resolveErr := filepath.Rel(vault, state)
			if resolveErr != nil || rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))) {
				return errs.New(2, "data directory and environment state must be outside the Obsidian vault")
			}
		}
	}
	if err := fsutil.PrivateDir(cfg.DataDir); err != nil {
		return errs.Wrap(1, "could not create private state root", err)
	}
	if err := fsutil.PrivateDir(cfg.StateDir()); err != nil {
		return errs.Wrap(1, "could not create environment state directory", err)
	}
	// Lock the whole root, so sandbox and production commands sharing it cannot overlap either.
	lock, err := fsutil.AcquireLock(filepath.Join(cfg.DataDir, ".process.lock"))
	if err != nil {
		return errs.Wrap(1, "cannot lock state directory; another obsidian-fin command may be running", err)
	}
	defer lock.Close()
	repo, err := storage.Open(ctx, filepath.Join(cfg.StateDir(), "obsidian-fin.db"), cfg.Environment)
	if err != nil {
		if errs.ExitCode(err) == 2 {
			return errs.Wrap(2, "database environment does not match the selected environment", err)
		}
		return errs.Wrap(1, "could not open the private state database", err)
	}
	defer func() {
		closeErr := repo.Close()
		if err == nil && closeErr != nil {
			err = errs.Wrap(1, "could not close state database", closeErr)
		}
	}()
	return fn(repo)
}
