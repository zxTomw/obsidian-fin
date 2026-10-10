// Package testutil contains synthetic offline fixtures; no real banking data.
package testutil

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"obsidian-fin/internal/domain"
	"obsidian-fin/internal/secrets"
	"obsidian-fin/internal/storage"
)

func Ptr[T any](v T) *T { return &v }
func Account() domain.Account {
	return domain.Account{ID: "account-test", Name: "Test Chequing", Mask: Ptr("1234"), Type: "depository", Subtype: Ptr("checking")}
}
func Item() domain.Item {
	return domain.Item{ID: "item-test", Environment: "sandbox", InstitutionID: "ins-test", InstitutionName: "BMO Test", TokenRef: secrets.AccessRef("sandbox", "client-test", "item-test"), ClientUserID: "0123456789abcdef0123456789abcdef", ConnectedAt: time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)}
}
func Transaction(id string, pending bool) domain.Transaction {
	return domain.Transaction{ID: id, AccountID: Ptr("account-test"), Date: Ptr("2026-10-09"), Name: Ptr("Example purchase"), MerchantName: Ptr("Example merchant"), Amount: Ptr("42.50123"), Currency: Ptr("CAD"), Pending: Ptr(pending), CategoryPrimary: Ptr("FOOD_AND_DRINK"), CategoryDetail: Ptr("RESTAURANTS")}
}
func Store(t *testing.T) *storage.Store {
	t.Helper()
	s, err := storage.Open(context.Background(), filepath.Join(t.TempDir(), "obsidian-fin.db"), "sandbox")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	if err := s.SaveConnection(context.Background(), Item(), []domain.Account{Account()}); err != nil {
		t.Fatal(err)
	}
	return s
}
func Round(t *testing.T, s *storage.Store, pages ...domain.Page) {
	t.Helper()
	if err := s.CommitRound(context.Background(), Item().ID, pages, "cursor-test"); err != nil {
		t.Fatal(err)
	}
}
func Notes(t *testing.T, s *storage.Store) []domain.Note {
	t.Helper()
	n, err := s.Notes(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return n
}

type MemorySecrets struct {
	mu                              sync.Mutex
	Values                          map[string]string
	SetError, GetError, DeleteError error
}

func NewSecrets() *MemorySecrets { return &MemorySecrets{Values: map[string]string{}} }
func (m *MemorySecrets) Get(ctx context.Context, key string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if m.GetError != nil {
		return "", m.GetError
	}
	v, ok := m.Values[key]
	if !ok {
		return "", secrets.ErrNotFound
	}
	return v, nil
}
func (m *MemorySecrets) Set(ctx context.Context, key, value string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if m.SetError != nil {
		return m.SetError
	}
	m.Values[key] = value
	return nil
}
func (m *MemorySecrets) Delete(ctx context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if m.DeleteError != nil {
		return m.DeleteError
	}
	delete(m.Values, key)
	return nil
}
