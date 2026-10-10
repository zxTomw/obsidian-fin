package storage

import (
	"context"
	"database/sql"
	"errors"
	"sort"
	"time"

	"obsidian-fin/internal/domain"
	"obsidian-fin/internal/fsutil"
	"obsidian-fin/internal/money"
)

// CommitRound applies the ordered round, aliases and cursor in one transaction.
func (s *Store) CommitRound(ctx context.Context, itemID string, pages []domain.Page, cursor string) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := stamp()
	for _, page := range pages {
		if err := upsertAccounts(ctx, tx, itemID, page.Accounts, now); err != nil {
			return err
		}
	}
	for _, page := range pages {
		for _, batch := range [][]domain.Transaction{page.Added, page.Modified} {
			for _, t := range batch {
				if err := upsertTransaction(ctx, tx, itemID, t, now); err != nil {
					return err
				}
			}
		}
		for _, id := range page.Removed {
			if id == "" {
				return errors.New("empty removed transaction ID")
			}
			_, err := tx.ExecContext(ctx, `INSERT INTO transactions(item_id,transaction_id,removed,removed_at,updated_at) VALUES (?,?,1,?,?)
			ON CONFLICT(item_id,transaction_id) DO UPDATE SET removed=1,removed_at=coalesce(transactions.removed_at,excluded.removed_at),updated_at=excluded.updated_at`, itemID, id, now, now)
			if err != nil {
				return err
			}
		}
	}
	if err := reconcile(ctx, tx, itemID, now); err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, "UPDATE items SET sync_cursor=?,last_sync_at=? WHERE item_id=?", cursor, now, itemID)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return errors.New("connection disappeared before sync commit")
	}
	return tx.Commit()
}

func upsertTransaction(ctx context.Context, tx *sql.Tx, itemID string, t domain.Transaction, now string) error {
	if t.ID == "" || t.AccountID == nil || *t.AccountID == "" || t.Date == nil || t.Amount == nil || t.Pending == nil {
		return errors.New("incomplete transaction data")
	}
	if _, err := time.Parse("2006-01-02", *t.Date); err != nil {
		return errors.New("invalid transaction date")
	}
	if t.AuthorizedDate != nil {
		if _, err := time.Parse("2006-01-02", *t.AuthorizedDate); err != nil {
			return errors.New("invalid authorized date")
		}
	}
	amount, err := money.Canonical(*t.Amount)
	if err != nil {
		return err
	}
	id, err := fsutil.ID()
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO transactions(item_id,transaction_id,account_id,note_id,pending_transaction_id,date,authorized_date,name,merchant_name,amount_decimal,iso_currency_code,unofficial_currency_code,pending,category_primary,category_detail,category_confidence,removed,updated_at)
	VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,0,?) ON CONFLICT(item_id,transaction_id) DO UPDATE SET
	account_id=excluded.account_id,note_id=coalesce(transactions.note_id,excluded.note_id),pending_transaction_id=coalesce(excluded.pending_transaction_id,transactions.pending_transaction_id),
	date=excluded.date,authorized_date=excluded.authorized_date,name=excluded.name,merchant_name=excluded.merchant_name,amount_decimal=excluded.amount_decimal,iso_currency_code=excluded.iso_currency_code,
	unofficial_currency_code=excluded.unofficial_currency_code,pending=excluded.pending,category_primary=excluded.category_primary,category_detail=excluded.category_detail,category_confidence=excluded.category_confidence,
	removed=0,removed_at=NULL,updated_at=excluded.updated_at`, itemID, t.ID, t.AccountID, id, t.PendingID, t.Date, t.AuthorizedDate, t.Name, t.MerchantName, amount, t.Currency, t.UnofficialCurrency, t.Pending, t.CategoryPrimary, t.CategoryDetail, t.CategoryConfidence, now)
	return err
}

func reconcile(ctx context.Context, tx *sql.Tx, itemID, now string) error {
	rows, err := tx.QueryContext(ctx, "SELECT "+transactionColumns+" FROM transactions WHERE item_id=? ORDER BY transaction_id", itemID)
	if err != nil {
		return err
	}
	all := map[string]domain.Transaction{}
	for rows.Next() {
		t, err := scanTransaction(rows)
		if err != nil {
			rows.Close()
			return err
		}
		all[t.ID] = t
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	// Reserve a predecessor even when it was never downloaded. A later removal then finds its alias.
	for _, t := range all {
		if t.PendingID == nil {
			continue
		}
		if *t.PendingID == t.ID || *t.PendingID == "" || (t.Pending != nil && *t.Pending) {
			return errors.New("invalid pending transaction relationship")
		}
		p, exists := all[*t.PendingID]
		if !exists {
			p = domain.Transaction{ID: *t.PendingID}
			if _, err := tx.ExecContext(ctx, "INSERT INTO transactions(item_id,transaction_id,superseded_by_transaction_id,updated_at) VALUES (?,?,?,?)", itemID, p.ID, t.ID, now); err != nil {
				return err
			}
		}
		if p.SupersededBy != nil && *p.SupersededBy != t.ID {
			return errors.New("ambiguous pending transaction replacement")
		}
		postedID := t.ID
		p.SupersededBy = &postedID
		all[p.ID] = p
		if _, err := tx.ExecContext(ctx, "UPDATE transactions SET superseded_by_transaction_id=? WHERE item_id=? AND transaction_id=?", t.ID, itemID, p.ID); err != nil {
			return err
		}
	}
	parent := map[string]string{}
	for id := range all {
		parent[id] = id
	}
	var root func(string) string
	root = func(id string) string {
		if parent[id] != id {
			parent[id] = root(parent[id])
		}
		return parent[id]
	}
	for _, t := range all {
		for _, other := range []*string{t.PendingID, t.SupersededBy} {
			if other == nil {
				continue
			}
			if _, ok := all[*other]; !ok {
				return errors.New("missing transaction alias")
			}
			a, b := root(t.ID), root(*other)
			if a != b {
				parent[b] = a
			}
		}
	}
	groups := map[string][]string{}
	for id := range all {
		r := root(id)
		groups[r] = append(groups[r], id)
	}
	tracked := map[string]bool{}
	existing := map[string]bool{}
	nrows, err := tx.QueryContext(ctx, "SELECT note_id FROM notes WHERE item_id=?", itemID)
	if err != nil {
		return err
	}
	for nrows.Next() {
		var id string
		if err := nrows.Scan(&id); err != nil {
			nrows.Close()
			return err
		}
		existing[id] = true
	}
	if err := nrows.Err(); err != nil {
		nrows.Close()
		return err
	}
	if err := nrows.Close(); err != nil {
		return err
	}
	erows, err := tx.QueryContext(ctx, "SELECT note_id FROM exports")
	if err != nil {
		return err
	}
	for erows.Next() {
		var id string
		if err := erows.Scan(&id); err != nil {
			erows.Close()
			return err
		}
		tracked[id] = true
	}
	if err := erows.Err(); err != nil {
		erows.Close()
		return err
	}
	if err := erows.Close(); err != nil {
		return err
	}
	for _, ids := range groups {
		sort.Strings(ids)
		noteIDs := map[string]bool{}
		current := ""
		rank := 0
		for _, id := range ids {
			t := all[id]
			if t.NoteID != nil {
				noteIDs[*t.NoteID] = true
			}
			if t.Amount == nil || t.SupersededBy != nil {
				continue
			}
			r := 1
			if t.Pending != nil && !*t.Pending {
				r = 2
			}
			if r > rank {
				current = id
				rank = r
			} else if r == rank && current != "" {
				return errors.New("multiple current records in one logical note")
			}
		}
		if current == "" {
			// Removal-only IDs have no readable note. A data-bearing cycle is invalid.
			for _, id := range ids {
				if all[id].Amount != nil {
					return errors.New("cyclic transaction replacement")
				}
			}
			continue
		}
		chosen := ""
		candidates := make([]string, 0, len(noteIDs))
		for id := range noteIDs {
			candidates = append(candidates, id)
			if tracked[id] {
				if chosen != "" && chosen != id {
					return errors.New("cannot merge two exported note identities")
				}
				chosen = id
			}
		}
		sort.Strings(candidates)
		if chosen == "" {
			for _, id := range candidates {
				if existing[id] {
					chosen = id
					break
				}
			}
		}
		if chosen == "" && len(candidates) > 0 {
			chosen = candidates[0]
		}
		if chosen == "" {
			var err error
			chosen, err = fsutil.ID()
			if err != nil {
				return err
			}
		}
		for _, id := range ids {
			if _, err := tx.ExecContext(ctx, "UPDATE transactions SET note_id=? WHERE item_id=? AND transaction_id=?", chosen, itemID, id); err != nil {
				return err
			}
		}
		for _, id := range candidates {
			if id != chosen {
				if _, err := tx.ExecContext(ctx, "DELETE FROM notes WHERE note_id=?", id); err != nil {
					return err
				}
			}
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO notes(note_id,item_id,current_transaction_id) VALUES (?,?,?) ON CONFLICT(note_id) DO UPDATE SET current_transaction_id=excluded.current_transaction_id`, chosen, itemID, current); err != nil {
			return err
		}
	}
	return nil
}
