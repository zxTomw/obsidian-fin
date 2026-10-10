package sync_test

import (
	"context"
	"errors"
	"testing"

	"obsidian-fin/internal/domain"
	"obsidian-fin/internal/errs"
	syncer "obsidian-fin/internal/sync"
	"obsidian-fin/internal/testutil"
)

type step struct {
	Cursor *string
	Page   domain.Page
	Err    error
}
type scripted struct {
	t     *testing.T
	Steps []step
	Calls int
}

func (p *scripted) Sync(_ context.Context, _ string, cursor *string) (domain.Page, error) {
	p.t.Helper()
	if p.Calls >= len(p.Steps) {
		p.t.Fatal("unexpected sync call")
	}
	s := p.Steps[p.Calls]
	p.Calls++
	if (cursor == nil) != (s.Cursor == nil) || (cursor != nil && *cursor != *s.Cursor) {
		p.t.Fatalf("cursor=%v want=%v on call %d", cursor, s.Cursor, p.Calls)
	}
	return s.Page, s.Err
}

func TestInitialPaginationAndDurableCursor(t *testing.T) {
	s := testutil.Store(t)
	item := testutil.Item()
	p := &scripted{t: t, Steps: []step{
		{Page: domain.Page{Accounts: []domain.Account{testutil.Account()}, Added: []domain.Transaction{testutil.Transaction("one", false)}, NextCursor: "first", HasMore: true}},
		{Cursor: testutil.Ptr("first"), Page: domain.Page{Added: []domain.Transaction{testutil.Transaction("two", false)}, NextCursor: "final"}},
	}}
	counts, err := syncer.Run(context.Background(), s, p, item, "access-sandbox-test")
	if err != nil {
		t.Fatal(err)
	}
	if counts.Added != 2 || len(testutil.Notes(t, s)) != 2 {
		t.Fatal("pages not complete")
	}
	saved, err := s.Item(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if saved.Cursor == nil || *saved.Cursor != "final" || saved.LastSyncAt == nil {
		t.Fatal("cursor/timestamp missing")
	}
	p = &scripted{t: t, Steps: []step{{Cursor: testutil.Ptr("final"), Page: domain.Page{NextCursor: "final"}}}}
	if _, err := syncer.Run(context.Background(), s, p, *saved, "access-sandbox-test"); err != nil {
		t.Fatal(err)
	}
}
func TestEmptyInitialAndPageFailure(t *testing.T) {
	s := testutil.Store(t)
	p := &scripted{t: t, Steps: []step{{Page: domain.Page{NextCursor: "empty"}}}}
	counts, err := syncer.Run(context.Background(), s, p, testutil.Item(), "token")
	if err != nil || !counts.InitialEmpty {
		t.Fatalf("empty=%+v %v", counts, err)
	}
	saved, err := s.Item(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	p = &scripted{t: t, Steps: []step{{Cursor: saved.Cursor, Page: domain.Page{Added: []domain.Transaction{testutil.Transaction("discarded", false)}, NextCursor: "part", HasMore: true}}, {Cursor: testutil.Ptr("part"), Err: errors.New("offline failure")}}}
	if _, err := syncer.Run(context.Background(), s, p, *saved, "token"); err == nil {
		t.Fatal("expected page failure")
	}
	after, err := s.Item(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if *after.Cursor != "empty" || len(testutil.Notes(t, s)) != 0 || !after.LastSyncAt.Equal(*saved.LastSyncAt) {
		t.Fatal("failed page advanced durable state")
	}
}
func TestMutationRestartsOriginalCursorAndExhausts(t *testing.T) {
	for _, exhaust := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "exhaustion"}[exhaust], func(t *testing.T) {
			s := testutil.Store(t)
			testutil.Round(t, s, domain.Page{})
			item, err := s.Item(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			mutation := &errs.APIError{Code: "TRANSACTIONS_SYNC_MUTATION_DURING_PAGINATION"}
			steps := []step{}
			rounds := 1
			if exhaust {
				rounds = 4
			}
			for range rounds {
				steps = append(steps, step{Cursor: item.Cursor, Page: domain.Page{Added: []domain.Transaction{testutil.Transaction("discarded", false)}, NextCursor: "part", HasMore: true}}, step{Cursor: testutil.Ptr("part"), Err: mutation})
			}
			if !exhaust {
				steps = append(steps, step{Cursor: item.Cursor, Page: domain.Page{Added: []domain.Transaction{testutil.Transaction("kept", false)}, NextCursor: "done"}})
			}
			p := &scripted{t: t, Steps: steps}
			counts, err := syncer.Run(context.Background(), s, p, *item, "token")
			if exhaust {
				if err == nil || len(testutil.Notes(t, s)) != 0 {
					t.Fatal("exhaustion committed data")
				}
				after, _ := s.Item(context.Background())
				if *after.Cursor != *item.Cursor {
					t.Fatal("exhaustion advanced cursor")
				}
			} else {
				if err != nil || counts.Restarts != 1 || counts.Added != 1 {
					t.Fatalf("restart %+v %v", counts, err)
				}
				notes := testutil.Notes(t, s)
				if len(notes) != 1 || notes[0].Transaction.ID != "kept" {
					t.Fatal("staged data leaked from failed round")
				}
			}
		})
	}
}
func TestCancellationLeavesCursor(t *testing.T) {
	s := testutil.Store(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	p := &scripted{t: t}
	if _, err := syncer.Run(ctx, s, p, testutil.Item(), "token"); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if p.Calls != 0 {
		t.Fatal("called provider after cancellation")
	}
}
