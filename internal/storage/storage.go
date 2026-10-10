// Package storage owns the committed provider data and export journal.
package storage

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"
	"obsidian-fin/internal/domain"
	"obsidian-fin/internal/errs"
	"obsidian-fin/internal/fsutil"
)

//go:embed migrations/*.sql
var migrations embed.FS

type Store struct {
	DB          *sql.DB
	Environment string
}

func Open(ctx context.Context, path, environment string) (*Store, error) {
	if environment != "sandbox" && environment != "production" {
		return nil, errs.New(2, "invalid database environment")
	}
	if err := fsutil.PrivateDir(filepath.Dir(path)); err != nil {
		return nil, err
	}
	f, err := fsutil.PrivateFile(path)
	if err != nil {
		return nil, err
	}
	if err := f.Close(); err != nil {
		return nil, err
	}
	u := &url.URL{Scheme: "file", Path: path}
	q := url.Values{}
	q.Add("_pragma", "foreign_keys(1)")
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "journal_mode(DELETE)")
	q.Add("_pragma", "synchronous(FULL)")
	u.RawQuery = q.Encode()
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	s := &Store{DB: db, Environment: environment}
	if err := s.migrate(ctx); err != nil {
		db.Close()
		return nil, err
	}
	item, err := s.Item(ctx)
	if err != nil {
		db.Close()
		return nil, err
	}
	if item != nil && item.Environment != environment {
		db.Close()
		return nil, errs.New(2, "database environment mismatch")
	}
	return s, nil
}
func (s *Store) Close() error { return s.DB.Close() }
func stamp() string           { return time.Now().UTC().Format(time.RFC3339Nano) }
func parseTime(v *string) *time.Time {
	if v == nil {
		return nil
	}
	t, err := time.Parse(time.RFC3339Nano, *v)
	if err != nil {
		return nil
	}
	return &t
}

func (s *Store) migrate(ctx context.Context) error {
	var exists int
	if err := s.DB.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_master WHERE type='table' AND name='schema_migrations'").Scan(&exists); err != nil {
		return err
	}
	current := 0
	if exists != 0 {
		if err := s.DB.QueryRowContext(ctx, "SELECT coalesce(max(version),0) FROM schema_migrations").Scan(&current); err != nil {
			return err
		}
	}
	entries, err := migrations.ReadDir("migrations")
	if err != nil {
		return err
	}
	if current > len(entries) {
		return errors.New("database schema is newer than this obsidian-fin version")
	}
	for _, entry := range entries {
		version, err := strconv.Atoi(strings.SplitN(entry.Name(), "_", 2)[0])
		if err != nil {
			return err
		}
		if version <= current {
			continue
		}
		content, err := migrations.ReadFile("migrations/" + entry.Name())
		if err != nil {
			return err
		}
		tx, err := s.DB.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, string(content)); err == nil {
			_, err = tx.ExecContext(ctx, "INSERT INTO schema_migrations(version,applied_at) VALUES (?,?)", version, stamp())
		}
		if err != nil {
			tx.Rollback()
			return fmt.Errorf("apply migration %d: %w", version, err)
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) Item(ctx context.Context) (*domain.Item, error) {
	var item domain.Item
	var connected string
	var last *string
	err := s.DB.QueryRowContext(ctx, `SELECT item_id,environment,institution_id,institution_name,token_ref,client_user_id,sync_cursor,connected_at,last_sync_at FROM items`).Scan(
		&item.ID, &item.Environment, &item.InstitutionID, &item.InstitutionName, &item.TokenRef, &item.ClientUserID, &item.Cursor, &connected, &last)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	item.ConnectedAt, err = time.Parse(time.RFC3339Nano, connected)
	if err != nil {
		return nil, err
	}
	item.LastSyncAt = parseTime(last)
	return &item, nil
}

func (s *Store) SaveConnection(ctx context.Context, item domain.Item, accounts []domain.Account) error {
	if item.Environment != s.Environment {
		return errs.New(2, "connection environment mismatch")
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, `INSERT INTO items(item_id,environment,institution_id,institution_name,token_ref,client_user_id,connected_at) VALUES (?,?,?,?,?,?,?)`, item.ID, item.Environment, item.InstitutionID, item.InstitutionName, item.TokenRef, item.ClientUserID, item.ConnectedAt.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return err
	}
	if err := upsertAccounts(ctx, tx, item.ID, accounts, stamp()); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) UpdateAccounts(ctx context.Context, itemID string, accounts []domain.Account) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := upsertAccounts(ctx, tx, itemID, accounts, stamp()); err != nil {
		return err
	}
	return tx.Commit()
}

func upsertAccounts(ctx context.Context, tx *sql.Tx, itemID string, accounts []domain.Account, now string) error {
	for _, a := range accounts {
		if a.ID == "" || a.Name == "" || a.Type == "" {
			return errors.New("incomplete account metadata")
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO accounts(item_id,account_id,name,official_name,mask,type,subtype,updated_at) VALUES (?,?,?,?,?,?,?,?)
		ON CONFLICT(item_id,account_id) DO UPDATE SET name=excluded.name,official_name=excluded.official_name,mask=excluded.mask,type=excluded.type,subtype=excluded.subtype,updated_at=excluded.updated_at`, itemID, a.ID, a.Name, a.OfficialName, a.Mask, a.Type, a.Subtype, now)
		if err != nil {
			return err
		}
	}
	return nil
}

const transactionColumns = `transaction_id,account_id,note_id,pending_transaction_id,superseded_by_transaction_id,date,authorized_date,name,merchant_name,amount_decimal,iso_currency_code,unofficial_currency_code,pending,category_primary,category_detail,category_confidence,removed`

type scanner interface{ Scan(...any) error }

func scanTransaction(row scanner) (domain.Transaction, error) {
	var t domain.Transaction
	err := row.Scan(&t.ID, &t.AccountID, &t.NoteID, &t.PendingID, &t.SupersededBy, &t.Date, &t.AuthorizedDate, &t.Name, &t.MerchantName, &t.Amount, &t.Currency, &t.UnofficialCurrency, &t.Pending, &t.CategoryPrimary, &t.CategoryDetail, &t.CategoryConfidence, &t.Removed)
	return t, err
}

func (s *Store) Notes(ctx context.Context) ([]domain.Note, error) {
	// Materialize a single committed snapshot; export bookkeeping starts after it closes.
	tx, err := s.DB.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT n.note_id,n.item_id,i.environment,`+prefixColumns("t", transactionColumns)+`,a.account_id,a.name,a.official_name,a.mask,a.type,a.subtype,e.relative_path,e.block_sha256,e.pending_block_sha256,e.template_version,e.exported_at
	FROM notes n JOIN items i ON i.item_id=n.item_id JOIN transactions t ON t.item_id=n.item_id AND t.transaction_id=n.current_transaction_id
	JOIN accounts a ON a.item_id=t.item_id AND a.account_id=t.account_id LEFT JOIN exports e ON e.note_id=n.note_id
	WHERE t.amount_decimal IS NOT NULL ORDER BY n.note_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var notes []domain.Note
	for rows.Next() {
		var n domain.Note
		t := &n.Transaction
		a := &n.Account
		var path, exported *string
		var version *int
		err := rows.Scan(&n.ID, &n.ItemID, &n.Environment, &t.ID, &t.AccountID, &t.NoteID, &t.PendingID, &t.SupersededBy, &t.Date, &t.AuthorizedDate, &t.Name, &t.MerchantName, &t.Amount, &t.Currency, &t.UnofficialCurrency, &t.Pending, &t.CategoryPrimary, &t.CategoryDetail, &t.CategoryConfidence, &t.Removed,
			&a.ID, &a.Name, &a.OfficialName, &a.Mask, &a.Type, &a.Subtype, &path, &n.Export.BlockHash, &n.Export.PendingHash, &version, &exported)
		if err != nil {
			return nil, err
		}
		if path != nil {
			n.Export.Path = *path
		}
		if version != nil {
			n.Export.TemplateVersion = *version
		}
		n.Export.ExportedAt = parseTime(exported)
		notes = append(notes, n)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return notes, nil
}
func prefixColumns(prefix, columns string) string {
	parts := strings.Split(columns, ",")
	for i := range parts {
		parts[i] = prefix + "." + parts[i]
	}
	return strings.Join(parts, ",")
}

func (s *Store) BeginExport(ctx context.Context, noteID, path, hash string, version int) error {
	result, err := s.DB.ExecContext(ctx, `INSERT INTO exports(note_id,relative_path,pending_block_sha256,template_version) VALUES (?,?,?,?)
	ON CONFLICT(note_id) DO UPDATE SET pending_block_sha256=excluded.pending_block_sha256,template_version=excluded.template_version WHERE exports.relative_path=excluded.relative_path`, noteID, path, hash, version)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected != 1 {
		return errors.New("export path did not match durable journal")
	}
	return nil
}
func (s *Store) FinishExport(ctx context.Context, noteID, hash string) error {
	result, err := s.DB.ExecContext(ctx, `UPDATE exports SET block_sha256=?,pending_block_sha256=NULL,exported_at=? WHERE note_id=? AND pending_block_sha256=?`, hash, stamp(), noteID, hash)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected != 1 {
		return errors.New("export hash did not match durable journal")
	}
	return nil
}
