// Package connect handles Hosted Link and durable connection recovery.
package connect

import (
	"context"
	"errors"
	"strings"
	"time"

	"obsidian-fin/internal/domain"
	"obsidian-fin/internal/errs"
	"obsidian-fin/internal/plaid"
	"obsidian-fin/internal/secrets"
)

type Provider interface {
	CreateLink(context.Context, plaid.LinkRequest) (plaid.Link, error)
	Completion(context.Context, string, bool) (plaid.Completion, error)
	Exchange(context.Context, string) (plaid.Exchange, error)
	Metadata(context.Context, string) (plaid.Metadata, error)
	Remove(context.Context, string) error
}
type Repository interface {
	Item(context.Context) (*domain.Item, error)
	SaveConnection(context.Context, domain.Item, []domain.Account) error
	UpdateAccounts(context.Context, string, []domain.Account) error
}
type State interface {
	UserID() (string, error)
	Recovery() (*domain.Recovery, error)
	SaveRecovery(domain.Recovery) error
	ClearRecovery() error
}
type Options struct {
	Environment, ClientID, Country string
	HistoryDays                    int
	Update                         bool
	Open                           func(context.Context, string) error
	PollInterval, PollTimeout      time.Duration
}
type Result struct {
	Existing, Updated bool
	Accounts          int
}

func Run(ctx context.Context, repo Repository, provider Provider, store secrets.Store, state State, opts Options) (Result, error) {
	item, err := repo.Item(ctx)
	if err != nil {
		return Result{}, errs.Wrap(1, "could not read connection state", err)
	}
	recovery, err := state.Recovery()
	if err != nil {
		return Result{}, errs.Wrap(1, "could not read connection recovery; another connection is blocked", err)
	}
	if recovery != nil {
		if item != nil && item.ID == recovery.ItemID && item.TokenRef == recovery.TokenRef {
			// SQLite committed before the recovery file was cleared. Keep the successful Item.
			if err := state.ClearRecovery(); err != nil {
				return Result{}, errs.Wrap(1, "connection is saved but recovery bookkeeping failed; rerun connect", err)
			}
		} else {
			return Result{}, recoverConnection(ctx, provider, store, state, *recovery, opts)
		}
	}
	if item != nil && !opts.Update {
		return Result{Existing: true}, nil
	}
	if item == nil && opts.Update {
		return Result{}, errs.New(1, "no connection exists; run obsidian-fin connect")
	}
	userID := ""
	access := ""
	if item != nil {
		if item.Environment != opts.Environment || !secrets.ValidateRef(item.TokenRef, opts.Environment, opts.ClientID) {
			return Result{}, errs.New(2, "connection environment/client mismatch")
		}
		userID = item.ClientUserID
		access, err = store.Get(ctx, item.TokenRef)
		if err != nil {
			return Result{}, errs.Wrap(1, "cannot access the connection token in Keychain", err)
		}
		if !secrets.ValidateToken(access, opts.Environment) {
			return Result{}, errs.New(2, "Keychain token environment mismatch")
		}
	} else {
		userID, err = state.UserID()
		if err != nil {
			return Result{}, errs.Wrap(1, "could not save a stable local user identifier", err)
		}
	}
	if opts.PollTimeout == 0 {
		opts.PollTimeout = 10 * time.Minute
	}
	if opts.PollInterval == 0 {
		opts.PollInterval = 5 * time.Second
	}
	pollCtx, cancel := context.WithTimeout(ctx, opts.PollTimeout)
	defer cancel()
	link, err := provider.CreateLink(pollCtx, plaid.LinkRequest{UserID: userID, Country: opts.Country, HistoryDays: opts.HistoryDays, AccessToken: access})
	if err != nil {
		return Result{}, err
	}
	if opts.Open == nil {
		return Result{}, errs.New(1, "browser opener is unavailable")
	}
	if err := opts.Open(pollCtx, link.URL); err != nil {
		return Result{}, err
	}
	var completion plaid.Completion
	for {
		if !link.Expires.IsZero() && !time.Now().Before(link.Expires) {
			return Result{}, errs.New(1, "Hosted Link expired; rerun connect")
		}
		completion, err = provider.Completion(pollCtx, link.Token, opts.Update)
		if err != nil {
			if ctx.Err() != nil {
				return Result{}, ctx.Err()
			}
			if pollCtx.Err() != nil {
				return Result{}, errs.New(1, "Hosted Link timed out after ten minutes; rerun connect")
			}
			return Result{}, err
		}
		if completion.Success {
			break
		}
		if completion.Finished {
			return Result{}, errs.New(130, "Hosted Link was canceled; rerun connect when ready")
		}
		timer := time.NewTimer(opts.PollInterval)
		select {
		case <-pollCtx.Done():
			timer.Stop()
			if ctx.Err() != nil {
				return Result{}, ctx.Err()
			}
			return Result{}, errs.New(1, "Hosted Link timed out; rerun connect")
		case <-timer.C:
		}
	}
	if opts.Update {
		metadata, err := provider.Metadata(ctx, access)
		if err != nil {
			return Result{}, err
		}
		if metadata.ItemID != item.ID {
			return Result{}, errs.New(1, "Plaid returned a different Item during update; connection was not changed")
		}
		if err := repo.UpdateAccounts(ctx, item.ID, metadata.Accounts); err != nil {
			return Result{}, errs.Wrap(1, "reauthentication completed but account metadata could not be saved; rerun connect --update", err)
		}
		return Result{Updated: true, Accounts: len(metadata.Accounts)}, nil
	}
	if completion.PublicToken == "" {
		return Result{}, errs.New(1, "Hosted Link did not return a public token in session results; rerun connect")
	}
	// Write a non-secret guard before exchange: an ambiguous exchange must never create a second Item on retry.
	ref := secrets.AccessRef(opts.Environment, opts.ClientID, userID)
	recovery = &domain.Recovery{TokenRef: ref, ClientUserID: userID}
	if err := state.SaveRecovery(*recovery); err != nil {
		return Result{}, errs.Wrap(1, "could not save connection recovery; token exchange was not attempted", err)
	}
	exchange, err := provider.Exchange(ctx, completion.PublicToken)
	if err != nil {
		var apiErr *errs.APIError
		if errors.As(err, &apiErr) && apiErr.Status >= 400 && apiErr.Status < 500 {
			if clearErr := state.ClearRecovery(); clearErr != nil {
				return Result{}, errs.Wrap(1, "token exchange failed and recovery bookkeeping could not be cleared", clearErr)
			}
			return Result{}, err
		}
		return Result{}, errs.Wrap(1, "token exchange result is uncertain; another connection is blocked; inspect Plaid Dashboard and connection-recovery.json before retrying", err)
	}
	recovery.ItemID = exchange.ItemID
	ref = secrets.AccessRef(opts.Environment, opts.ClientID, exchange.ItemID)
	recovery.TokenRef = ref
	if err := state.SaveRecovery(*recovery); err != nil {
		return Result{}, cleanup(ctx, provider, store, state, *recovery, exchange.AccessToken, err)
	}
	if !secrets.ValidateToken(exchange.AccessToken, opts.Environment) || exchange.ItemID == "" {
		return Result{}, cleanup(ctx, provider, store, state, *recovery, exchange.AccessToken, errs.New(2, "Plaid returned a token/environment mismatch"))
	}
	if err := store.Set(ctx, ref, exchange.AccessToken); err != nil {
		return Result{}, cleanup(ctx, provider, store, state, *recovery, exchange.AccessToken, err)
	}
	metadata, err := provider.Metadata(ctx, exchange.AccessToken)
	if err != nil {
		return Result{}, cleanup(ctx, provider, store, state, *recovery, exchange.AccessToken, err)
	}
	if metadata.ItemID != exchange.ItemID {
		return Result{}, cleanup(ctx, provider, store, state, *recovery, exchange.AccessToken, errors.New("Item metadata mismatch"))
	}
	newItem := domain.Item{ID: exchange.ItemID, Environment: opts.Environment, InstitutionID: metadata.InstitutionID, InstitutionName: metadata.InstitutionName, TokenRef: ref, ClientUserID: userID, ConnectedAt: time.Now().UTC()}
	if err := repo.SaveConnection(ctx, newItem, metadata.Accounts); err != nil {
		// A failed commit acknowledgement may nevertheless have committed. Verify before revoking.
		checkCtx, done := context.WithTimeout(context.WithoutCancel(ctx), 60*time.Second)
		defer done()
		saved, checkErr := repo.Item(checkCtx)
		if checkErr != nil {
			return Result{}, errs.Wrap(1, "connection durability is uncertain; token and recovery reference retained; rerun connect", err)
		}
		if saved == nil || saved.ID != newItem.ID {
			return Result{}, cleanup(ctx, provider, store, state, *recovery, exchange.AccessToken, err)
		}
	}
	if err := state.ClearRecovery(); err != nil {
		return Result{}, errs.Wrap(1, "connection is durable but recovery bookkeeping failed; rerun connect", err)
	}
	return Result{Accounts: len(metadata.Accounts)}, nil
}

func cleanup(ctx context.Context, provider Provider, store secrets.Store, state State, recovery domain.Recovery, token string, cause error) error {
	cleanCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 60*time.Second)
	defer cancel()
	if err := provider.Remove(cleanCtx, token); err != nil {
		// Try again to retain a token if the first Keychain write itself failed. Never write plaintext.
		var keyErr error
		if strings.HasPrefix(token, "access-sandbox-") && strings.Contains(recovery.TokenRef, "/sandbox/") || strings.HasPrefix(token, "access-production-") && strings.Contains(recovery.TokenRef, "/production/") {
			keyErr = store.Set(cleanCtx, recovery.TokenRef, token)
		} else {
			keyErr = errors.New("token environment mismatch")
		}
		stateErr := state.SaveRecovery(recovery)
		if keyErr != nil {
			return errs.Wrap(1, "connection was not saved, Plaid cleanup failed, and Keychain recovery storage is unavailable; another connection is blocked; revoke the Item in Plaid Dashboard using connection-recovery.json", errors.Join(cause, err, keyErr, stateErr))
		}
		return errs.Wrap(1, "connection was not saved and Plaid cleanup failed; another connection is blocked; retain the Keychain token and recovery reference, then rerun connect or revoke in Plaid Dashboard", errors.Join(cause, err, keyErr, stateErr))
	}
	recovery.Revoked = true
	if err := state.SaveRecovery(recovery); err != nil {
		return errs.Wrap(1, "new Item was revoked but its cleanup checkpoint could not be saved; token retained; inspect recovery before retrying", err)
	}
	if err := store.Delete(cleanCtx, recovery.TokenRef); err != nil {
		return errs.Wrap(1, "new Item was revoked but Keychain cleanup failed; rerun connect", err)
	}
	if err := state.ClearRecovery(); err != nil {
		return errs.Wrap(1, "new Item was revoked but recovery cleanup failed; rerun connect", err)
	}
	return errs.Wrap(1, "connection could not be saved; the new Plaid Item was revoked; rerun connect", cause)
}
func recoverConnection(ctx context.Context, provider Provider, store secrets.Store, state State, r domain.Recovery, opts Options) error {
	if r.ItemID == "" {
		return errs.New(1, "an earlier token exchange has an uncertain result; inspect Plaid Dashboard and connection-recovery.json; another connection is blocked")
	}
	if !secrets.ValidateRef(r.TokenRef, opts.Environment, opts.ClientID) {
		return errs.New(2, "connection recovery environment/client mismatch")
	}
	if !r.Revoked {
		token, err := store.Get(ctx, r.TokenRef)
		if err != nil {
			return errs.Wrap(1, "cleanup is blocked because its token is unavailable in Keychain; revoke the Item in Plaid Dashboard and follow README recovery steps", err)
		}
		if !secrets.ValidateToken(token, opts.Environment) {
			return errs.New(2, "recovery token environment mismatch")
		}
		if err := provider.Remove(ctx, token); err != nil {
			return errs.Wrap(1, "Plaid cleanup still failed; another connection is blocked; rerun connect", err)
		}
		r.Revoked = true
		if err := state.SaveRecovery(r); err != nil {
			return errs.Wrap(1, "Item revoked but cleanup checkpoint could not be saved; token retained", err)
		}
	}
	if err := store.Delete(ctx, r.TokenRef); err != nil {
		return errs.Wrap(1, "Item revoked but Keychain cleanup failed", err)
	}
	if err := state.ClearRecovery(); err != nil {
		return errs.Wrap(1, "Item revoked but recovery cleanup failed", err)
	}
	return errs.New(1, "the incomplete connection was cleaned up; rerun obsidian-fin connect")
}
