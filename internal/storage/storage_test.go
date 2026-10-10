package storage_test

import (
	"context"
	"path/filepath"
	"testing"

	"obsidian-fin/internal/domain"
	"obsidian-fin/internal/storage"
	"obsidian-fin/internal/testutil"
)

func TestLifecycleRoundAndReplay(t *testing.T) {
	for _, order := range []string{"posted first", "removal first", "pending never seen"} {
		t.Run(order, func(t *testing.T) {
			s := testutil.Store(t)
			priorID := ""
			if order != "pending never seen" {
				testutil.Round(t, s, domain.Page{Added: []domain.Transaction{testutil.Transaction("pending", true)}})
				priorID = testutil.Notes(t, s)[0].ID
			}
			posted := testutil.Transaction("posted", false)
			posted.PendingID = testutil.Ptr("pending")
			add := domain.Page{Added: []domain.Transaction{posted}}
			remove := domain.Page{Removed: []string{"pending"}}
			pages := []domain.Page{add, remove}
			if order == "removal first" {
				pages = []domain.Page{remove, add}
			}
			testutil.Round(t, s, pages...)
			notes := testutil.Notes(t, s)
			if len(notes) != 1 || notes[0].Transaction.ID != "posted" || notes[0].Transaction.Removed {
				t.Fatalf("incorrect posted note: %+v", notes)
			}
			if priorID != "" && notes[0].ID != priorID {
				t.Fatal("pending note ID changed")
			}
			id := notes[0].ID
			testutil.Round(t, s, pages...)
			if again := testutil.Notes(t, s); len(again) != 1 || again[0].ID != id {
				t.Fatal("replay changed note identity")
			}
			testutil.Round(t, s, domain.Page{Removed: []string{"pending", "unknown", "unknown"}})
			if testutil.Notes(t, s)[0].Transaction.Removed {
				t.Fatal("superseded removal removed posted note")
			}
			testutil.Round(t, s, domain.Page{Removed: []string{"posted"}})
			if !testutil.Notes(t, s)[0].Transaction.Removed || testutil.Notes(t, s)[0].ID != id {
				t.Fatal("posted removal lost note")
			}
			var count int
			if err := s.DB.QueryRow("SELECT count(*) FROM transactions").Scan(&count); err != nil || count != 3 {
				t.Fatalf("rows=%d err=%v", count, err)
			}
		})
	}
}

func TestModifiedAndUnmatchedRemainDistinct(t *testing.T) {
	s := testutil.Store(t)
	p := testutil.Transaction("pending", true)
	a := testutil.Transaction("posted-a", false)
	b := testutil.Transaction("posted-b", false)
	testutil.Round(t, s, domain.Page{Added: []domain.Transaction{p, a, b}})
	if len(testutil.Notes(t, s)) != 3 {
		t.Fatal("fuzzy deduplication occurred")
	}
	id := ""
	for _, n := range testutil.Notes(t, s) {
		if n.Transaction.ID == a.ID {
			id = n.ID
		}
	}
	a.Amount = testutil.Ptr("-0.0001234567890123456789")
	a.Currency = nil
	a.UnofficialCurrency = nil
	a.MerchantName = nil
	a.AuthorizedDate = nil
	testutil.Round(t, s, domain.Page{Modified: []domain.Transaction{a}})
	for _, n := range testutil.Notes(t, s) {
		if n.Transaction.ID == a.ID {
			if n.ID != id || *n.Transaction.Amount != *a.Amount || n.Transaction.Currency != nil || n.Transaction.MerchantName != nil {
				t.Fatal("modified/null/decimal data lost")
			}
		}
	}
}

func TestPageOrderAndAtomicRollback(t *testing.T) {
	s := testutil.Store(t)
	transaction := testutil.Transaction("ordered", false)
	testutil.Round(t, s, domain.Page{Added: []domain.Transaction{transaction}, Removed: []string{transaction.ID}}, domain.Page{Modified: []domain.Transaction{transaction}})
	if testutil.Notes(t, s)[0].Transaction.Removed {
		t.Fatal("response-page order not preserved")
	}
	before, err := s.Item(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	bad := testutil.Transaction("bad", false)
	bad.AccountID = testutil.Ptr("missing-account")
	err = s.CommitRound(context.Background(), before.ID, []domain.Page{{Added: []domain.Transaction{testutil.Transaction("new", false), bad}}}, "must-not-commit")
	if err == nil {
		t.Fatal("expected foreign key failure")
	}
	after, err := s.Item(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if *after.Cursor != *before.Cursor || !after.LastSyncAt.Equal(*before.LastSyncAt) || len(testutil.Notes(t, s)) != 1 {
		t.Fatal("partial sync commit")
	}
}

func TestDeferredCommitFailureRollsBack(t *testing.T) {
	s := testutil.Store(t)
	// Trigger only in this fixture injects a deferred foreign-key violation at commit.
	_, err := s.DB.Exec(`CREATE TABLE commit_failure(x TEXT REFERENCES items(item_id) DEFERRABLE INITIALLY DEFERRED); CREATE TRIGGER fail_commit AFTER UPDATE OF sync_cursor ON items BEGIN INSERT INTO commit_failure VALUES ('missing-item'); END;`)
	if err != nil {
		t.Fatal(err)
	}
	err = s.CommitRound(context.Background(), testutil.Item().ID, []domain.Page{{Added: []domain.Transaction{testutil.Transaction("not-saved", false)}}}, "not-saved")
	if err == nil {
		t.Fatal("expected commit failure")
	}
	item, err := s.Item(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if item.Cursor != nil || len(testutil.Notes(t, s)) != 0 {
		t.Fatal("commit failure persisted rows or cursor")
	}
}

func TestOneItemAndEnvironmentIsolation(t *testing.T) {
	s := testutil.Store(t)
	item := testutil.Item()
	item.ID = "second"
	if err := s.SaveConnection(context.Background(), item, nil); err == nil {
		t.Fatal("accepted second Item")
	}
	path := filepath.Join(t.TempDir(), "db")
	other, err := storage.Open(context.Background(), path, "sandbox")
	if err != nil {
		t.Fatal(err)
	}
	if err := other.SaveConnection(context.Background(), testutil.Item(), nil); err != nil {
		t.Fatal(err)
	}
	if err := other.Close(); err != nil {
		t.Fatal(err)
	}
	if production, err := storage.Open(context.Background(), path, "production"); err == nil {
		production.Close()
		t.Fatal("accepted mismatched environment")
	}
}

func TestUnseenPredecessorReservesIdentityBeforeLateArrival(t *testing.T) {
	s := testutil.Store(t)
	posted := testutil.Transaction("posted", false)
	posted.PendingID = testutil.Ptr("pending")
	testutil.Round(t, s, domain.Page{Added: []domain.Transaction{posted}})
	first := testutil.Notes(t, s)
	if len(first) != 1 {
		t.Fatal("unseen predecessor created extra note")
	}
	id := first[0].ID
	testutil.Round(t, s, domain.Page{Added: []domain.Transaction{testutil.Transaction("pending", true)}})
	second := testutil.Notes(t, s)
	if len(second) != 1 || second[0].ID != id || second[0].Transaction.ID != "posted" {
		t.Fatal("late pending arrival changed current identity")
	}
	testutil.Round(t, s, domain.Page{Removed: []string{"pending"}})
	if testutil.Notes(t, s)[0].Transaction.Removed {
		t.Fatal("late predecessor removal removed posted record")
	}
}

func TestExportJournalRejectsMissingOrMismatchedWrites(t *testing.T) {
	s := testutil.Store(t)
	testutil.Round(t, s, domain.Page{Added: []domain.Transaction{testutil.Transaction("one", false)}})
	id := testutil.Notes(t, s)[0].ID
	ctx := context.Background()
	if err := s.FinishExport(ctx, id, "unknown"); err == nil {
		t.Fatal("absent journal reported success")
	}
	if err := s.BeginExport(ctx, id, "original/path", "intended", 1); err != nil {
		t.Fatal(err)
	}
	if err := s.BeginExport(ctx, id, "wrong/path", "wrong", 1); err == nil {
		t.Fatal("path mismatch reported success")
	}
	if err := s.FinishExport(ctx, id, "wrong"); err == nil {
		t.Fatal("hash mismatch reported success")
	}
	if err := s.FinishExport(ctx, id, "intended"); err != nil {
		t.Fatal(err)
	}
}
