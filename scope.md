# obsidian-fin V1 — Local BMO → Plaid → Obsidian CLI

**Status:** Implementation scope · **Target:** macOS, one user, one BMO Plaid Item per environment, one existing Obsidian vault · **Date:** 2026-10-10

## 1. Goal and boundaries

Build a Go executable named `obsidian-fin`. The user manually connects BMO, downloads transaction changes into local SQLite, and exports readable Markdown notes into an Obsidian vault. Every command exits when finished; nothing runs in the background.

V1 must be restartable, idempotent, and preserve the user's Obsidian notes and categories. SQLite owns imported banking data; Markdown outside a clearly marked generated block belongs to the user. Keep the Plaid integration independent of the exporter.

**Implementation defaults:** macOS Keychain; Sandbox by default; Canadian BMO (`CA`) as the initial target, with country configurable for the appropriate BMO institution; 90 days of requested initial history; all transaction-capable accounts authorized through Link. History and transaction availability depend on Plaid and the institution.

**Prerequisites / feasibility gate:** The user supplies Plaid developer credentials and an existing vault path. Before the live-account milestone, verify Transactions access, the correct BMO institution/country, and any required OAuth registration in the user's Plaid environment. Sandbox success does not prove live BMO connectivity. Do not promise coverage or substitute scraping if access is unavailable. See [Plaid institution coverage](https://plaid.com/docs/institutions/) and [institution API requirements](https://plaid.com/docs/api/institutions/).

## 2. Architecture

```text
Manual command
  ├─ connect → browser: Plaid Hosted Link ↔ BMO
  │              → Plaid API → macOS Keychain + Item/account metadata
  ├─ sync    → Plaid /transactions/sync → SQLite
  └─ export obsidian → SQLite → managed Markdown files in the vault
```

Use the official [Plaid Go SDK](https://github.com/plaid/plaid-go), a maintained SQLite driver, embedded versioned migrations, and ordinary filesystem writes. Pin dependencies; prefer a pure-Go SQLite driver where practical. No Obsidian API or installed plugin is required.

Suggested package boundaries:

```text
cmd/obsidian-fin/          command parsing and user messages
internal/config/      non-secret configuration
internal/plaid/       Link, Item/accounts, transaction API adapter
internal/secrets/     Keychain adapter
internal/storage/     SQLite repositories and embedded migrations
internal/sync/        pagination, reconciliation, atomic commit
internal/export/      deterministic rendering and safe file updates
```

Expose small interfaces for Plaid, secret storage, and file operations so failure cases can be tested without live banking credentials. Avoid a generic integration framework.

## 3. Functional requirements

### Authentication and connection

- Implement Plaid Link through **Hosted Link**: create a Link token for `transactions`, open its hosted URL in the default browser, then poll `/link/token/get` for completion and obtain the public token from session `results`. Exchange it using `/item/public_token/exchange`. This is the chosen no-webhook implementation of Link. See [Hosted Link](https://plaid.com/docs/link/hosted-link/).
- Credentials and MFA are entered only in Plaid/BMO's browser flow. The CLI must never collect or store bank login credentials.
- Use a stable locally generated user identifier. Bound polling to 10 minutes, ordinarily every 5 seconds; cancellation/expiry returns an actionable error. Provide a terminal-only fallback for opening the hosted URL, treating that URL as sensitive.
- Persist the access token in Keychain and store only its reference in SQLite. Connection succeeds only after secret storage and Item/account metadata are durable. On persistence failure, attempt to revoke the newly created Item before deleting its token; if cleanup fails, retain the Keychain token and a non-secret recovery reference, and block another new connection until cleanup completes. Never report partial persistence as success.
- A second plain `connect` reports the existing connection and exits without creating another Item. `connect --update` launches Link update mode against the existing access token and retains the Item, cursor, and note identities. Omit initial `products`/history settings in update mode; do not perform a new-Item token exchange. See [Link update mode](https://plaid.com/docs/link/update-mode/).
- On login-required errors, direct the user to `connect --update`; never silently create a replacement Item. Replacement of an invalid/revoked Item is outside V1.
- Retrieve account IDs, names, masks, types, and subtypes. Discard balances if included in metadata responses; do not persist, display, or export them.

### Incremental synchronization

1. Load the Item's durable starting cursor. For initial sync, omit the cursor; never use the special `"now"` cursor to skip history.
2. Fetch `/transactions/sync` pages using each `next_cursor` until `has_more=false`. Stage responses in memory; do not advance durable state between pages.
3. On `TRANSACTIONS_SYNC_MUTATION_DURING_PAGINATION`, discard the entire staged round and restart from its original cursor, with at most three restarts. On exhaustion, fail with no committed changes. This follows [Plaid's pagination contract](https://plaid.com/docs/transactions/).
4. In one SQLite transaction, apply all account metadata and transaction changes, reconcile pending replacements across the entire round, then save the final cursor and successful-sync timestamp. Roll back everything on failure. Never hold a database write transaction open during network calls.

Apply `added` and `modified` as upserts keyed by `(item_id, transaction_id)`. Apply `removed` as durable tombstones, retaining previous data and note identity; unknown removal IDs also get tombstones. Preserve response-page order; within each page apply added, modified, then removed. Repeated responses must produce the same final state.

A posted transaction can have a new ID and a `pending_transaction_id` pointing to its removed pending predecessor; both events can occur on different pages. Retain both provider records, associate them with **one stable logical note ID**, and select the posted record as current. Allocate/reserve this association even when the pending record was never locally seen. A later removal of the predecessor must not mark the posted note removed. If no pending reference is supplied, keep provider IDs distinct; do not deduplicate by merchant, amount, or date. See [Plaid transaction states](https://plaid.com/docs/transactions/transactions-data/).

Preserve source names, merchant names, transaction/authorized dates, amount, currency, pending status, and available Plaid personal-finance category fields. Dates remain date-only strings. Store amounts as canonical decimal text, without rounding to cents or using floating-point arithmetic for money. Preserve Plaid's sign convention: positive = outflow, negative = inflow; retain null/unknown currency explicitly. See [Transactions API](https://plaid.com/docs/api/products/transactions/).

An initial empty response is valid while Plaid prepares data; report that the user may need to run `sync` again. Sync retrieves changes already available at Plaid; it does not force a bank refresh or guarantee real-time freshness.

### Markdown export and edit preservation

- Export only from SQLite; require neither network access nor Keychain access. Include pending, posted, and removed states.
- Create one note per logical transaction under `Finance/obsidian-fin/<environment>/<item-hash>/<note-id>.md`. Use stable opaque IDs, never merchant/date-based filenames; a pending-to-posted transition updates the same path.
- Each note has exactly one generated block. Put the title and imported fields inside it. Initialize optional user frontmatter (`category`, `tags`) and a Notes section outside it; thereafter preserve **all bytes outside the block**, including user categories, notes, links, and tags.

````markdown
---
category: ""
tags: []
---
<!-- obsidian-fin:begin NOTE_ID -->
# Example merchant

- Date: 2026-10-09
- Account: Chequing (1234)
- Amount: CAD 42.50 (outflow)
- Status: posted
- Plaid transaction ID: EXAMPLE_ID
- Plaid category: FOOD_AND_DRINK / RESTAURANTS
<!-- obsidian-fin:end NOTE_ID -->

## Notes
User-owned notes go here.
````

- Save a SHA-256 hash of the last successfully exported generated block. Replace it only when its current bytes match that hash or a recorded pending-write hash from interrupted export. Already-desired bytes need no rewrite. Changed generated content, missing/duplicate/wrong markers, an untracked occupied path, or a missing previously exported file are conflicts: preserve the file, report its path, and return nonzero. No automatic overwrite/force option. Explain that generated-block edits can be moved into Notes and the prior block restored before retrying.
- A removed current transaction changes the generated status to `removed`; retain the file and user text. A superseded pending record does not create a second note. Do not create notes for removal-only IDs with no known transaction data.
- Escape provider text so it cannot inject Markdown HTML, block markers, or frontmatter. No generated executable content or unsolicited external links.
- Before writing, durably record the intended path/block hash in `exports`; this permits recovery of first-time file creation too. Write via a same-directory temporary file, flush, and atomically replace. Recheck the original file before replacement; if it changed, report a conflict. Mark export successful and clear the pending hash only after writing. On rerun, recognize either the last successful or pending block, then reconcile with current source data. Independent safe notes may succeed even if others conflict; report partial completion.
- Export is deterministic: identical source data/template produces identical blocks. A no-change rerun leaves file bytes and modification times unchanged. No run timestamp inside the generated block.

## 4. Minimum data schema

Each environment uses a separate database and Keychain namespace. IDs below are text; timestamps are UTC; nullable provider fields remain nullable. Enable foreign keys and use parameterized SQL.

| Table | Required columns and constraints |
|---|---|
| `schema_migrations` | `version` primary key, `applied_at` |
| `items` | `item_id` primary key; `environment`, `institution_id`, `institution_name`, `token_ref`, `client_user_id`, nullable `sync_cursor`, `connected_at`, nullable `last_sync_at`; enforce one Item in this database |
| `accounts` | `(item_id, account_id)` composite primary key; `name`, nullable `official_name`, `mask`, `type`, `subtype`, `updated_at`; Item foreign key |
| `transactions` | `(item_id, transaction_id)` composite primary key; nullable `account_id`, `note_id`, `pending_transaction_id`, `superseded_by_transaction_id`, `date`, `authorized_date`, `name`, `merchant_name`, `amount_decimal`, `iso_currency_code`, `unofficial_currency_code`, `pending`, category primary/detail/confidence; `removed`, nullable `removed_at`, `updated_at` |
| `notes` | `note_id` primary key; `item_id`, `current_transaction_id`; one current provider record per logical note; references to Item and transaction |
| `exports` | `note_id` primary key/foreign key; unique `relative_path`; nullable last-successful `block_sha256`, nullable `pending_block_sha256`, `template_version`, nullable `exported_at`; null `exported_at` means never successfully exported |

Normal transaction rows require an account, date, amount, pending flag, and note association; tombstone-only rows may omit them. Use account/transaction foreign keys where applicable and indexes for note association, pending predecessor, and account/date queries. `note_id` is intentionally non-unique in `transactions`: pending and posted aliases share it. Preserve relationships on replay; never derive note IDs from mutable fields. Full raw API payload retention is unnecessary.

## 5. CLI and configuration contract

```sh
obsidian-fin connect
obsidian-fin connect --update
obsidian-fin sync
obsidian-fin export obsidian --vault "/absolute/path/to/Vault"
```

These are the V1 commands; `sync` does not implicitly export. Support global `--config`, `--data-dir`, and `--environment sandbox|production`, plus `--help` and `--version`. Vault may be configured instead of passed each time. Use precedence: explicit flags → TOML configuration → defaults. Never accept secrets in command-line arguments or TOML.

Default configuration/state root: `~/Library/Application Support/obsidian-fin/`; partition SQLite/state by environment. Keep it outside the vault. Configuration contains only client ID, environment, country, requested history days, and paths. First `connect` obtains any missing Plaid client secret through a hidden terminal prompt and stores it in Keychain; automated tests inject an in-memory secret store. Production must be explicitly selected. Reject environment/token mismatches.

Print concise counts and outcomes to stdout; errors/progress go to stderr. Sync reports added/modified/removed counts and completion; export reports created/updated/unchanged/conflicted counts. Never print transaction details or tokens in routine output.

Exit codes: `0` success (including no changes), `1` operational/API/Keychain/I/O failure, `2` invalid arguments/configuration, `3` export conflicts or partial export, `130` user cancellation. Missing connection identifies `connect` as the remedy. Retryable errors identify rerunning the command; login errors identify update mode. Include sanitized Plaid error code/request ID where available.

## 6. Nonfunctional and security requirements

- Build one macOS executable; pin the Go version and dependencies. No Docker, external database service, telemetry, or runtime scheduler dependency.
- Use cancellation-aware API calls, a 60-second per-request timeout, and bounded retries for rate limits/network/5xx errors (three retries, exponential backoff with jitter and `Retry-After` where supplied). Do not blindly retry token exchange after an ambiguous result.
- Hold an exclusive process lock for commands using the same data directory; fail clearly on overlapping runs. Lock the export destination too. Export consumes a consistent committed database snapshot.
- Use macOS Keychain through a secret-store adapter; no plaintext token fallback. Store Plaid client secrets and access tokens there, namespaced by environment/client/Item. Keychain denial or unavailability fails safely. Never put client secrets or access tokens in subprocess arguments.
- Use HTTPS with standard certificate verification. Redact API bodies, temporary Link/public tokens, hosted URLs, account details, and transaction contents from logs and errors. Debug logging must obey the same policy.
- New state directories are `0700`; database/config/temp files and sidecars are `0600`. SQLite and exported Markdown contain readable financial data: document this and recommend FileVault plus protected backups. Keychain does not encrypt the database or vault.
- Resolve the vault root, restrict writes to the managed subtree, reject path traversal and symlink escapes, and never alter unrelated notes. Supported operation assumes no simultaneous edits by Obsidian/sync tools during the brief export write window; detect changes where possible and document that atomic replacement alone cannot coordinate other applications.
- No secrets or real financial fixtures in source control. Document how to revoke the Plaid connection and remove Keychain/local state manually.

## 7. Acceptance criteria and required tests

| Acceptance criterion | Required evidence |
|---|---|
| Connection and reauthentication work without a listener/webhook; repeat connect creates no second Item | Sandbox browser smoke test; mocked completion, cancellation, expiry, update-mode, and Keychain failure tests |
| Paginated initial and incremental sync are complete and atomic | SQLite integration tests: multi-page results, empty initial response, cursor persistence, page failure and commit failure; transactions and cursor remain unchanged on failure |
| Mutation restart uses the original cursor; replay is idempotent | Scripted Plaid mock with mutation on a later page; successful retry and bounded exhaustion; repeated batches retain row/note counts |
| Provider lifecycle changes remain correct | Fixtures: modified posted data, cross-page pending removal/posted addition in either page order, repeated removal, unknown removal, removal of superseded pending, posted removal, and unmatched posting |
| User edits survive export and pending-to-posted transitions | Temporary-vault tests assert outside-block byte equality, stable note/path identity, removal status without deletion, and conflict preservation |
| Export is restartable and contained | Failure injection before/after first creation or replacement and before success bookkeeping, including source changes before retry; no truncated file; no-change bytes/mtime unchanged; unrelated files unchanged; missing paths, collisions, malformed markers, malicious provider text, symlinks, permissions, and lock contention tested |
| Financial values and secrets are handled correctly | Positive/negative/fractional amounts, nullable fields/currencies/dates; log redaction and environment isolation tests |
| Real BMO workflow succeeds when access is available | User-run live test: connect → sync → export → inspect a small set of transactions in Obsidian → repeat with no duplicates; record coverage/access blockers separately |

Default tests must be offline and credential-free. A separately opted-in Sandbox smoke test may use developer credentials. Before handoff, run `go test ./...`, `go test -race ./...`, `go vet ./...`, and a macOS build; document results and any platform limitations. Never use live BMO data as an automated fixture.

## 8. Milestones and handoff

1. **Foundation:** runnable command skeleton, config precedence/environment isolation, Keychain adapter, migrations/schema, package interfaces, example config and build instructions.
2. **Connect:** Hosted Link and update mode, secret/metadata persistence, cancellation and failures; complete Sandbox connection smoke test.
3. **Sync:** pagination and cursor transaction, idempotent lifecycle reconciliation, failure/replay tests; inspect SQLite results from Sandbox.
4. **Export:** stable notes, managed blocks, hash-based conflicts, atomic writes/recovery; complete edit-preservation and containment tests.
5. **Release candidate:** run required checks, document setup/recovery/privacy and manual workflow, then validate a live BMO account when credentials/coverage permit.

Deliver source, `go.mod`/`go.sum`, embedded migrations, secret-free example TOML, README with exact setup/usage and conflict recovery, offline fixtures/tests, and build/check results. Implement these milestones in order and keep this scope authoritative; surface external access blockers without adding infrastructure.

## 9. Explicitly out of scope

- Daemon, background service, `launchd`/cron scheduling, continuous polling, server, cloud/serverless deployment, Docker, webhooks, or a local authentication HTTP listener.
- Account balances, investment holdings, loans/liabilities, payments/transfers, recurring-transaction analysis, or forced `/transactions/refresh`.
- Custom Obsidian plugin, Obsidian Headless/Sync integration, Notion export, remote vault delivery, or managing the user's existing vault-sync service.
- Multiple users/Items/banks, automatic replacement/relink deduplication, fuzzy matching, CSV import, historical backfill beyond the configured initial request, or a hosted app/UI.
- Budgets, dashboards, monthly summaries, FX conversion, automatic category overrides, or bidirectional edits from Markdown into banking data.
- Automatic note relocation/rename discovery, deletion/purging of old notes, overwrite-conflict resolution, database/vault encryption implemented by obsidian-fin, or secrets stored in plaintext.
- `sync --export obsidian` convenience command; keep synchronization and export explicit in V1.
