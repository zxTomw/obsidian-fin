// Package sync stages complete cursor rounds before committing any provider changes.
package sync

import (
	"context"
	"obsidian-fin/internal/domain"
	"obsidian-fin/internal/errs"
)

type Provider interface {
	Sync(context.Context, string, *string) (domain.Page, error)
}
type Repository interface {
	CommitRound(context.Context, string, []domain.Page, string) error
}
type Counts struct {
	Added, Modified, Removed int
	InitialEmpty             bool
	Restarts                 int
}

func Run(ctx context.Context, repo Repository, provider Provider, item domain.Item, token string) (Counts, error) {
	for restart := 0; restart <= 3; restart++ {
		cursor := item.Cursor
		var pages []domain.Page
		counts := Counts{Restarts: restart}
		seen := map[string]bool{}
		if cursor != nil {
			seen[*cursor] = true
		}
		mutated := false
		for {
			if err := ctx.Err(); err != nil {
				return Counts{}, err
			}
			page, err := provider.Sync(ctx, token, cursor)
			if errs.IsAPI(err, "TRANSACTIONS_SYNC_MUTATION_DURING_PAGINATION") {
				mutated = true
				break
			}
			if err != nil {
				return Counts{}, err
			}
			pages = append(pages, page)
			counts.Added += len(page.Added)
			counts.Modified += len(page.Modified)
			counts.Removed += len(page.Removed)
			if !page.HasMore {
				if err := repo.CommitRound(ctx, item.ID, pages, page.NextCursor); err != nil {
					return Counts{}, errs.Wrap(1, "could not commit sync; no changes were saved; rerun sync", err)
				}
				counts.InitialEmpty = item.Cursor == nil && counts.Added+counts.Modified+counts.Removed == 0
				return counts, nil
			}
			if seen[page.NextCursor] || len(pages) > 10000 {
				return Counts{}, errs.New(1, "Plaid pagination did not advance; no changes were saved; rerun sync")
			}
			seen[page.NextCursor] = true
			next := page.NextCursor
			cursor = &next
		}
		if mutated && restart == 3 {
			return Counts{}, errs.New(1, "Plaid pagination kept changing after three restarts; no changes were saved; rerun sync")
		}
	}
	return Counts{}, errs.New(1, "sync failed; rerun sync")
}
