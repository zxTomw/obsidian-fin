package connect_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"obsidian-fin/internal/connect"
	"obsidian-fin/internal/domain"
	"obsidian-fin/internal/errs"
	"obsidian-fin/internal/plaid"
	"obsidian-fin/internal/secrets"
	"obsidian-fin/internal/storage"
	"obsidian-fin/internal/testutil"
)

type provider struct {
	LinkRequest                                                plaid.LinkRequest
	CompletionValue                                            plaid.Completion
	CompletionError, ExchangeError, MetadataError, RemoveError error
	Expires                                                    time.Time
	Exchanges, Creates, Removes                                int
	Update                                                     bool
}

func (p *provider) CreateLink(_ context.Context, r plaid.LinkRequest) (plaid.Link, error) {
	p.Creates++
	p.LinkRequest = r
	return plaid.Link{Token: "link-sandbox-test", URL: "https://secure.plaid.com/hl/test", Expires: p.Expires}, nil
}
func (p *provider) Completion(ctx context.Context, _ string, update bool) (plaid.Completion, error) {
	p.Update = update
	if err := ctx.Err(); err != nil {
		return plaid.Completion{}, err
	}
	return p.CompletionValue, p.CompletionError
}
func (p *provider) Exchange(context.Context, string) (plaid.Exchange, error) {
	p.Exchanges++
	return plaid.Exchange{ItemID: "item-test", AccessToken: "access-sandbox-test"}, p.ExchangeError
}
func (p *provider) Metadata(context.Context, string) (plaid.Metadata, error) {
	return plaid.Metadata{ItemID: "item-test", InstitutionID: "ins-test", InstitutionName: "BMO Test", Accounts: []domain.Account{testutil.Account()}}, p.MetadataError
}
func (p *provider) Remove(context.Context, string) error { p.Removes++; return p.RemoveError }
func goodProvider() *provider {
	return &provider{CompletionValue: plaid.Completion{Success: true, Finished: true, PublicToken: "public-sandbox-test"}}
}
func options() connect.Options {
	return connect.Options{Environment: "sandbox", ClientID: "client-test", Country: "CA", HistoryDays: 90, Open: func(context.Context, string) error { return nil }, PollInterval: time.Millisecond, PollTimeout: 10 * time.Millisecond}
}
func emptyStore(t *testing.T) *storage.Store {
	t.Helper()
	s := testutil.Store(t)
	if _, err := s.DB.Exec("DELETE FROM accounts; DELETE FROM items"); err != nil {
		t.Fatal(err)
	}
	return s
}
func state(t *testing.T) connect.FileState { t.Helper(); return connect.FileState{Dir: t.TempDir()} }

func TestConnectDurableRepeatAndUpdate(t *testing.T) {
	s := emptyStore(t)
	p := goodProvider()
	secretsStore := testutil.NewSecrets()
	local := state(t)
	result, err := connect.Run(context.Background(), s, p, secretsStore, local, options())
	if err != nil {
		t.Fatal(err)
	}
	if result.Accounts != 1 || p.Exchanges != 1 {
		t.Fatal("connection incomplete")
	}
	item, err := s.Item(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	token, err := secretsStore.Get(context.Background(), item.TokenRef)
	if err != nil || token != "access-sandbox-test" {
		t.Fatal("token not durable")
	}
	if recovery, err := local.Recovery(); err != nil || recovery != nil {
		t.Fatal("successful connection retained guard")
	}
	testutil.Round(t, s, domain.Page{Added: []domain.Transaction{testutil.Transaction("one", false)}})
	before, err := s.Item(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	noteID := testutil.Notes(t, s)[0].ID
	result, err = connect.Run(context.Background(), s, p, secretsStore, local, options())
	if err != nil || !result.Existing || p.Creates != 1 {
		t.Fatal("repeat created new Item")
	}
	opts := options()
	opts.Update = true
	p.CompletionValue.PublicToken = ""
	result, err = connect.Run(context.Background(), s, p, secretsStore, local, opts)
	if err != nil || !result.Updated || p.Exchanges != 1 || !p.Update || p.LinkRequest.AccessToken != token {
		t.Fatalf("update: %+v %v", result, err)
	}
	after, err := s.Item(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if *after.Cursor != *before.Cursor || after.ID != before.ID || testutil.Notes(t, s)[0].ID != noteID {
		t.Fatal("update reset identity or cursor")
	}
}

func TestCancellationExpiryAndPollingTimeout(t *testing.T) {
	for _, kind := range []string{"user exit", "expired", "timeout", "context canceled", "login required"} {
		t.Run(kind, func(t *testing.T) {
			s := emptyStore(t)
			p := goodProvider()
			ctx := context.Background()
			code := 1
			switch kind {
			case "user exit":
				p.CompletionValue = plaid.Completion{Finished: true}
				code = 130
			case "expired":
				p.Expires = time.Now().Add(-time.Minute)
			case "timeout":
				p.CompletionValue = plaid.Completion{}
			case "context canceled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
				code = 130
			case "login required":
				p.CompletionError = &errs.APIError{Code: "ITEM_LOGIN_REQUIRED", RequestID: "test-request"}
			}
			_, err := connect.Run(ctx, s, p, testutil.NewSecrets(), state(t), options())
			if err == nil || errs.ExitCode(err) != code || p.Exchanges != 0 {
				t.Fatalf("%s: %v exchanges=%d", kind, err, p.Exchanges)
			}
			if kind == "login required" && !strings.Contains(err.Error(), "connect --update") {
				t.Fatal("login remedy missing")
			}
		})
	}
}

type saveFailure struct {
	*storage.Store
	Error error
}

func (s saveFailure) SaveConnection(context.Context, domain.Item, []domain.Account) error {
	return s.Error
}

func TestKeychainAndPersistenceFailuresRevokeBeforeDelete(t *testing.T) {
	for _, kind := range []string{"keychain denied", "metadata failure", "SQLite failure"} {
		t.Run(kind, func(t *testing.T) {
			s := emptyStore(t)
			p := goodProvider()
			memory := testutil.NewSecrets()
			local := state(t)
			var repo connect.Repository = s
			switch kind {
			case "keychain denied":
				memory.SetError = secrets.ErrUnavailable
			case "metadata failure":
				p.MetadataError = errors.New("synthetic")
			case "SQLite failure":
				repo = saveFailure{Store: s, Error: errors.New("disk unavailable")}
			}
			_, err := connect.Run(context.Background(), repo, p, memory, local, options())
			if err == nil || p.Removes != 1 {
				t.Fatalf("missing cleanup: %v removes=%d", err, p.Removes)
			}
			if len(memory.Values) != 0 {
				t.Fatal("revoked token retained")
			}
			r, err := local.Recovery()
			if err != nil || r != nil {
				t.Fatal("recovery guard retained after cleanup")
			}
			item, err := s.Item(context.Background())
			if err != nil || item != nil {
				t.Fatal("partial connection reported durable")
			}
		})
	}
}
func TestFailedCleanupRetainsTokenAndBlocksNewItem(t *testing.T) {
	s := emptyStore(t)
	p := goodProvider()
	p.RemoveError = errors.New("offline")
	memory := testutil.NewSecrets()
	local := state(t)
	repo := saveFailure{Store: s, Error: errors.New("disk")}
	if _, err := connect.Run(context.Background(), repo, p, memory, local, options()); err == nil {
		t.Fatal("partial connection succeeded")
	}
	r, err := local.Recovery()
	if err != nil || r == nil || r.ItemID != "item-test" {
		t.Fatal("missing recovery reference")
	}
	if token, err := memory.Get(context.Background(), r.TokenRef); err != nil || token != "access-sandbox-test" {
		t.Fatal("cleanup failure deleted token")
	}
	if _, err := connect.Run(context.Background(), repo, p, memory, local, options()); err == nil || p.Creates != 1 {
		t.Fatal("cleanup failure allowed second Item")
	}
	p.RemoveError = nil
	if _, err := connect.Run(context.Background(), repo, p, memory, local, options()); err == nil || p.Creates != 1 {
		t.Fatal("recovery should clean and request rerun")
	}
	if r, err := local.Recovery(); err != nil || r != nil {
		t.Fatal("cleanup did not clear guard")
	}
}
func TestAmbiguousExchangeNeverRetriesAndBlocksReconnect(t *testing.T) {
	s := emptyStore(t)
	p := goodProvider()
	p.ExchangeError = errors.New("lost response")
	memory := testutil.NewSecrets()
	local := state(t)
	if _, err := connect.Run(context.Background(), s, p, memory, local, options()); err == nil {
		t.Fatal("expected uncertainty")
	}
	if _, err := connect.Run(context.Background(), s, p, memory, local, options()); err == nil || p.Exchanges != 1 || p.Creates != 1 {
		t.Fatal("ambiguous exchange was repeated")
	}
}
func TestCommittedConnectionGuardIsClearedWithoutRevocation(t *testing.T) {
	s := testutil.Store(t)
	item := testutil.Item()
	local := state(t)
	p := goodProvider()
	memory := testutil.NewSecrets()
	if err := local.SaveRecovery(domain.Recovery{ItemID: item.ID, TokenRef: item.TokenRef, ClientUserID: item.ClientUserID}); err != nil {
		t.Fatal(err)
	}
	result, err := connect.Run(context.Background(), s, p, memory, local, options())
	if err != nil || !result.Existing || p.Removes != 0 || p.Creates != 0 {
		t.Fatal("revoked a committed connection")
	}
}
func TestStableUserIDAndEnvironmentMismatch(t *testing.T) {
	local := state(t)
	a, err := local.UserID()
	if err != nil {
		t.Fatal(err)
	}
	b, err := local.UserID()
	if err != nil || a != b {
		t.Fatal("user ID changed")
	}
	s := testutil.Store(t)
	opts := options()
	opts.Update = true
	opts.Environment = "production"
	p := goodProvider()
	if _, err := connect.Run(context.Background(), s, p, testutil.NewSecrets(), local, opts); errs.ExitCode(err) != 2 || p.Creates != 0 {
		t.Fatal("mismatched token accepted")
	}
}

func TestRevocationCheckpointSurvivesKeychainDeleteFailure(t *testing.T) {
	s := emptyStore(t)
	p := goodProvider()
	memory := testutil.NewSecrets()
	memory.DeleteError = secrets.ErrUnavailable
	local := state(t)
	repo := saveFailure{Store: s, Error: errors.New("disk")}
	if _, err := connect.Run(context.Background(), repo, p, memory, local, options()); err == nil {
		t.Fatal("cleanup failure succeeded")
	}
	r, err := local.Recovery()
	if err != nil || r == nil || !r.Revoked || p.Removes != 1 {
		t.Fatal("revocation was not durably checkpointed")
	}
	memory.DeleteError = nil
	memory.GetError = secrets.ErrUnavailable
	if _, err := connect.Run(context.Background(), repo, p, memory, local, options()); err == nil {
		t.Fatal("cleanup should require a fresh connect after completion")
	}
	if p.Removes != 1 || p.Creates != 1 || len(memory.Values) != 0 {
		t.Fatal("already-revoked Item was accessed again")
	}
	if r, err := local.Recovery(); err != nil || r != nil {
		t.Fatal("checkpoint recovery did not clear guard")
	}
}
