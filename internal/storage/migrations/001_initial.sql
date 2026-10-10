CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY, applied_at TEXT NOT NULL);
CREATE TABLE items (
    item_id TEXT PRIMARY KEY,
    singleton INTEGER NOT NULL DEFAULT 1 UNIQUE CHECK (singleton = 1),
    environment TEXT NOT NULL CHECK (environment IN ('sandbox','production')),
    institution_id TEXT NOT NULL, institution_name TEXT NOT NULL,
    token_ref TEXT NOT NULL, client_user_id TEXT NOT NULL,
    sync_cursor TEXT, connected_at TEXT NOT NULL, last_sync_at TEXT
);
CREATE TABLE accounts (
    item_id TEXT NOT NULL, account_id TEXT NOT NULL, name TEXT NOT NULL,
    official_name TEXT, mask TEXT, type TEXT NOT NULL, subtype TEXT, updated_at TEXT NOT NULL,
    PRIMARY KEY (item_id,account_id),
    FOREIGN KEY (item_id) REFERENCES items(item_id)
);
CREATE TABLE transactions (
    item_id TEXT NOT NULL, transaction_id TEXT NOT NULL,
    account_id TEXT, note_id TEXT, pending_transaction_id TEXT, superseded_by_transaction_id TEXT,
    date TEXT, authorized_date TEXT, name TEXT, merchant_name TEXT, amount_decimal TEXT,
    iso_currency_code TEXT, unofficial_currency_code TEXT, pending INTEGER CHECK (pending IN (0,1)),
    category_primary TEXT, category_detail TEXT, category_confidence TEXT,
    removed INTEGER NOT NULL DEFAULT 0 CHECK (removed IN (0,1)), removed_at TEXT, updated_at TEXT NOT NULL,
    PRIMARY KEY (item_id,transaction_id),
    FOREIGN KEY (item_id) REFERENCES items(item_id),
    FOREIGN KEY (item_id,account_id) REFERENCES accounts(item_id,account_id),
    FOREIGN KEY (note_id) REFERENCES notes(note_id) DEFERRABLE INITIALLY DEFERRED,
    FOREIGN KEY (item_id,pending_transaction_id) REFERENCES transactions(item_id,transaction_id) DEFERRABLE INITIALLY DEFERRED,
    FOREIGN KEY (item_id,superseded_by_transaction_id) REFERENCES transactions(item_id,transaction_id) DEFERRABLE INITIALLY DEFERRED,
    CHECK ((account_id IS NULL AND date IS NULL AND amount_decimal IS NULL AND pending IS NULL AND (removed=1 OR superseded_by_transaction_id IS NOT NULL)) OR
           (account_id IS NOT NULL AND date IS NOT NULL AND amount_decimal IS NOT NULL AND pending IS NOT NULL AND note_id IS NOT NULL))
);
CREATE TABLE notes (
    note_id TEXT PRIMARY KEY, item_id TEXT NOT NULL, current_transaction_id TEXT NOT NULL,
    UNIQUE (item_id,current_transaction_id),
    FOREIGN KEY (item_id) REFERENCES items(item_id),
    FOREIGN KEY (item_id,current_transaction_id) REFERENCES transactions(item_id,transaction_id) DEFERRABLE INITIALLY DEFERRED
);
CREATE TABLE exports (
    note_id TEXT PRIMARY KEY REFERENCES notes(note_id), relative_path TEXT NOT NULL UNIQUE,
    block_sha256 TEXT, pending_block_sha256 TEXT, template_version INTEGER NOT NULL,
    exported_at TEXT
);
CREATE INDEX transactions_note ON transactions(note_id);
CREATE INDEX transactions_pending ON transactions(item_id,pending_transaction_id);
CREATE INDEX transactions_account_date ON transactions(item_id,account_id,date);
