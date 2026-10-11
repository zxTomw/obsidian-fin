# obsidian-fin v1

obsidian-fin is a manual macOS CLI for BMO → Plaid Transactions → SQLite → an existing Obsidian vault. Run a command when you want an update; it exits when finished. SQLite owns imported data. Your Markdown frontmatter, categories, tags, links and Notes remain yours.

This implementation follows [scope.md](scope.md). The defaults are Plaid Sandbox, Canadian BMO, and 90 requested days of history. Production requires explicit selection through a flag or configuration. Sandbox and Production have separate databases and Keychain namespaces, and each database permits one Item.

## Build and checks

Use Go **1.26.9** (pinned in `go.mod`, `.go-version` and the Makefile), macOS and the Xcode Command Line Tools. The Keychain adapter calls Security.framework directly through cgo. SQLite uses the pure-Go `modernc.org/sqlite` driver. There are no secrets in process arguments, no runtime services, and no Obsidian plugin to install.

```sh
make build
./bin/obsidian-fin --version
./bin/obsidian-fin --help
make check
```

`make build` produces a single native macOS executable at `bin/obsidian-fin`. It enables cgo for Keychain. Go may download the pinned toolchain and checksummed modules on the first build. `make check` runs `go test ./...`, `go test -race ./...`, `go vet ./...` and the macOS build. Default tests are offline, use synthetic fixtures and in-memory secret stores, and never open the browser or access your Keychain.

Linux can run the offline suite, but actual connections require macOS with cgo. A build without cgo fails safely when Keychain is needed; it has no plaintext token fallback. Go 1.26.9 was selected after checking the older installed toolchain against the Go vulnerability database.

## Configure

Create a private configuration directory and copy the example configuration:

```sh
install -d -m 700 "$HOME/.config/obsidian-fin"
install -m 600 config.example.toml "$HOME/.config/obsidian-fin/config.toml"
```

Edit that file with your **Plaid client ID** and, optionally, your vault path:

```toml
client_id = "YOUR_PLAID_CLIENT_ID"
environment = "sandbox"
country = "CA"
history_days = 90
vault = "/Users/you/Documents/ObsidianVault"
```

The only accepted keys are `client_id`, `environment`, `country`, `history_days`, `data_dir` and `vault`. `country` may be `CA` or `US` for the appropriate BMO institution; history may be 1–730 days, subject to Plaid/institution availability. Paths must be absolute; `~` is not expanded in TOML. Unknown keys, including secret keys, are rejected without echoing their values.

Precedence is explicit flags → TOML → defaults. Global flags are `--config`, `--data-dir`, and `--environment sandbox|production`. `export obsidian` accepts `--vault`. There is no environment-variable credential configuration and no secret argument. The first interactive `connect` prompts for a missing Plaid client secret with terminal echo disabled and stores it in Keychain. It never asks for your bank password or MFA code; those belong exclusively in the Plaid/BMO browser flow.

The default configuration file is `~/.config/obsidian-fin/config.toml`. Use `--config` to select a different file. SQLite and connection state remain in Application Support; `data_dir` or `--data-dir` changes their location independently. The state directory is created when a command needs it.

Default layout:

```text
~/.config/obsidian-fin/
  config.toml

~/Library/Application Support/obsidian-fin/
  .process.lock
  sandbox/
    obsidian-fin.db
    client-user-id
    connection-recovery.json   # only while a connection needs recovery
  production/
    obsidian-fin.db
    client-user-id
```

Keep the state root outside the vault. obsidian-fin rejects a state directory within the resolved vault. New state directories are `0700`; database, lock and temporary files are `0600`. Keep the configuration directory `0700` and its file `0600` too. An existing configuration elsewhere can be selected with `--config` or moved to the new default path; it is not automatically relocated.

## Connect, sync and export

```sh
./bin/obsidian-fin connect
./bin/obsidian-fin sync
./bin/obsidian-fin export obsidian --vault "/absolute/path/to/Vault"
```

`connect` requests Transactions through [Plaid Hosted Link](https://plaid.com/docs/link/hosted-link/), opens the sensitive Hosted Link URL in the default browser, and polls `/link/token/get` ordinarily every five seconds, for at most ten minutes. It obtains the public token from session `results` and exchanges it once. If opening the browser fails, the sensitive URL is shown only on the controlling terminal, never captured stdout/stderr. Do not share or log that URL.

Choose BMO for the configured country and authorize all transaction-capable accounts you intend to import. obsidian-fin checks the returned institution, country and Transactions support before saving the connection. Account balances returned by Plaid are discarded. Success means both the Keychain token and SQLite Item/account metadata are durable.

A second plain `connect` reports the existing connection without making a new Item. If Plaid reports `ITEM_LOGIN_REQUIRED`, reauthenticate:

```sh
./bin/obsidian-fin connect --update
```

Update mode sends the existing access token, omits initial products/history settings, and keeps the Item, cursor and note identities. It never exchanges a new-Item public token. Automatic replacement of revoked/invalid Items is outside v1.

`sync` stages all available `/transactions/sync` pages before opening a database write transaction. It commits account metadata, ordered additions/modifications/removals, pending replacements and the final cursor together. A pagination mutation restarts the entire round from its original durable cursor, at most three times. API/network/page/commit failures leave the previous transactions and cursor intact. Initial sync omits the cursor, never uses `"now"`, and may legitimately be empty while Plaid prepares history; run it again later.

Sync downloads changes already available at Plaid. It does not force a bank refresh or guarantee real-time freshness. Requested history is not a coverage guarantee. Amounts are preserved as canonical decimal text directly from JSON numbers, without cent rounding or floating-point money arithmetic. Plaid's positive-outflow/negative-inflow convention and null currencies/dates are retained.

API calls have a 60-second request timeout and honor cancellation. Network failures, rate limits and server errors receive at most three retries with exponential backoff and jitter. `Retry-After` is respected when within the bounded wait; a longer requested delay returns failure rather than retrying early. Token exchange is attempted once because a lost response may hide success.

`export obsidian` reads a committed SQLite snapshot and needs neither the network nor Keychain. It creates one note per logical transaction at:

```text
Finance/obsidian-fin/<environment>/<item-hash>/<opaque-note-id>.md
```

An identified pending → posted replacement updates the same note, including when its pending predecessor was never locally downloaded. Provider records are retained separately. An unmatched posting stays distinct; obsidian-fin never matches by merchant, date or amount. Removed transactions remain readable with status `removed`; removal-only IDs with no known transaction data do not create notes.

For Production, first complete the access checks below, then explicitly select it consistently:

```sh
./bin/obsidian-fin --environment production connect
./bin/obsidian-fin --environment production sync
./bin/obsidian-fin --environment production export obsidian --vault "/absolute/path/to/Vault"
```

The Production client secret is prompted/stored separately. Tokens and references from another environment or client are rejected.

## Editing notes and resolving conflicts

New notes initialize this user-owned content outside the generated block:

```markdown
---
category: ""
tags: []
---
<!-- obsidian-fin:begin OPAQUE_NOTE_ID -->
# Imported title and fields live here
<!-- obsidian-fin:end OPAQUE_NOTE_ID -->

## Notes
User-owned notes go here.
```

Edit frontmatter, add links, change tags, and write Notes freely. Thereafter every byte outside the generated block is preserved, including line endings. Imported text is escaped to prevent Markdown/HTML, executable content, external links, wikilinks, frontmatter or marker injection. There is no run timestamp in the block. An unchanged export leaves both file bytes and modification time unchanged.

obsidian-fin records the SHA-256 hash of the last successfully written block. It accepts that block, a journaled pending block from an interrupted write, or already-desired bytes. It refuses to overwrite edited generated content, missing/duplicate/wrong markers, an untracked occupied path or a missing previously exported file. It reports the conflicted path and preserves the file. There is no force option or automatic relocation discovery.

To recover a conflict:

1. Make a protected copy of the affected note.
2. Move edits made inside the generated block into the Notes section.
3. Restore the **exact prior generated block**, including its markers and whitespace, from undo history, Obsidian version history or a protected backup. Keep your user text outside it. For a renamed/moved/deleted note, restore it at its original managed path. For an untracked collision, move your unrelated file out of that path after reviewing it.
4. Rerun the export. Other independent safe notes may have already succeeded.

Do not delete markers, edit `exports` hashes, or remove export records to bypass conflict protection. If the prior bytes cannot be recovered, retain the conflict for manual review.

Before each write, SQLite durably records the path and intended block hash. A same-directory `0600` temporary file is flushed, the original is rechecked, and the file is atomically replaced; success is recorded afterward. First creation uses an atomic create-if-absent operation so a newly occupied path cannot be overwritten. A retry recognizes an interrupted write, promotes it before journaling a changed source block, and reconciles with current SQLite data. You can simply rerun after a crash or I/O failure.

The vault root is resolved and filesystem operations are anchored by `os.Root`. Symlinked managed directories/files, traversal and hardlinked notes are rejected. An exclusive state-root process lock prevents overlapping commands; a separate managed-vault lock prevents overlapping exports from different state roots. Lock files remain after exit, but the OS releases their locks; deleting them to bypass a running command is unsafe.

Pause simultaneous edits or vault-sync activity during the brief export write window. obsidian-fin detects changed originals where possible; atomic replacement cannot coordinate with another application's edits between the final check and rename. Existing unrelated notes are not changed.

## Connection failure recovery

A non-secret `connection-recovery.json` guard is written before token exchange. This protects against an uncertain exchange response or a crash after Plaid has created an Item. Token exchange is never blindly retried.

If secret/metadata persistence fails, obsidian-fin attempts `/item/remove` **before** deleting the access token. It checkpoints successful revocation, so a subsequent Keychain-delete failure or crash can be resumed without calling Plaid with a revoked token. Successful cleanup clears the guard and returns failure so you can retry. If revocation fails, it retains the Keychain token and recovery reference and blocks a new connection. Run `connect` again to retry cleanup. If SQLite already committed before the guard was cleared, a rerun recognizes the saved Item and clears bookkeeping without revoking it.

If Keychain also denied recovery storage, or an exchange response was lost before its Item ID/token was known, no plaintext fallback is written. The guard still blocks another Item. Inspect the selected environment in Plaid Dashboard and revoke any incomplete Item using the Dashboard's available Item management tools or an approved `/item/remove` API workflow. If necessary, remove Plaid/BMO authorization through the bank's access controls. Verify revocation before manually removing the recovery file; do not clear it merely to bypass an access problem. An expired/revoked token may require this manual path.

## Privacy, revocation and local cleanup

Client secrets and access tokens are generic passwords in macOS Keychain, under service **`obsidian-fin`**. Accounts are namespaced as `obsidian-fin/<environment>/<client-id-hash>/client-secret` and `obsidian-fin/<environment>/<client-id-hash>/access/<item-id-hash>`. `items.token_ref` is the non-secret account/reference; the token itself never enters SQLite, TOML, shell arguments, logs or Markdown. Keychain access denial fails safely. The adapter does not invoke the `security` command with a password argument.

SQLite and the vault contain **readable financial data**. Keychain does not encrypt either one. Use FileVault, restricted filesystem access and protected backups. obsidian-fin emits no telemetry and has no background service. Routine output contains counts and outcomes, not transaction contents, account details or tokens. Public API failures include sanitized Plaid error codes/request IDs; API bodies, provider messages and sensitive URLs are excluded.

To retire a connection, revoke its Plaid Item using available Dashboard Item tools or [Plaid's `/item/remove` workflow](https://plaid.com/docs/api/items/#itemremove), or revoke the bank authorization. Never paste secrets/access tokens into subprocess arguments. Then use **Keychain Access** to remove the corresponding `obsidian-fin` generic-password entries, and manually remove the selected environment's state directory after preserving any needed protected backups. Remove the client-secret entry only when no other state directory uses that client/environment.

Exported notes are not automatically deleted. You can retain/archive them or remove the managed item subtree yourself after reviewing its user text. Keep SQLite and export records if you intend to continue the same Item: deleting state loses the cursor and stable note associations. Creating a replacement after deleting state may create distinct paths; v1 does not merge/relink old Items.

## Exit codes

| Code | Meaning |
| --- | --- |
| 0 | Success, including no changes |
| 1 | API, Keychain, SQLite or filesystem failure |
| 2 | Invalid arguments, configuration or environment/client mismatch |
| 3 | Export conflicts or partial completion |
| 130 | User cancellation |

Concise counts/outcomes go to stdout. Errors/progress/conflict paths go to stderr. `sync` never implicitly exports.

## Sandbox and live acceptance

The separately opted-in browser smoke test uses your configured Sandbox state and native Keychain. It retains the Sandbox Item and notes for inspection. Run it in a local terminal; a missing client secret is entered through the hidden prompt. The TOML file must include an existing disposable vault, client ID and a dedicated state path outside that vault. Only the non-secret config path and opt-in switch are environment variables:

```sh
OBSIDIAN_FIN_SANDBOX_SMOKE=1 OBSIDIAN_FIN_SMOKE_CONFIG="/absolute/path/to/sandbox.toml" \
  go test -tags sandbox -run TestSandboxHostedLinkSmoke -v ./internal/cli
```

It runs connect, repeated connect, sync, repeated export and update mode. Select the appropriate BMO Sandbox institution in the browser and use Plaid's [Sandbox test credentials](https://plaid.com/docs/sandbox/test-credentials/). An initially empty sync can require another manual run. To simulate login-required state, use Plaid's separately documented Sandbox reset-login tooling, then run `connect --update`.

**Before a live account test**, verify in your Plaid environment that Transactions is enabled, the BMO institution for your configured country is supported, and any institution-required OAuth application registration is complete. Consult [institution coverage](https://plaid.com/docs/institutions/) and [institution API requirements](https://plaid.com/docs/api/institutions/). Hosted Link handles the browser flow; obsidian-fin has no HTTP listener, webhook, local callback server or scraping fallback. Account access/product/OAuth restrictions must be resolved through Plaid/BMO.

For a live test, connect → sync → export → inspect a small selection in Obsidian → edit Notes/category → repeat sync/export and verify no duplicate notes or lost user edits. Record access/coverage blockers separately. Sandbox success does not prove live BMO connectivity. See [VALIDATION.md](VALIDATION.md) for this checkout's check results and unverified external milestones.

## Source map

`cmd/obsidian-fin` contains the entry point. `internal/cli` handles commands/output; `config` handles strict TOML; `plaid` isolates the official Go SDK and preserves amount JSON lexemes; `secrets` calls native Keychain; `connect` handles Link and recovery; `storage` owns embedded migrations and repositories; `sync` stages rounds; `export` owns rendering/journaling/contained atomic writes. Small provider, secret-store, repository and file interfaces allow failures to be tested offline.

Tests cover SQLite integration/commit rollback, pagination/mutation exhaustion, replay and provider lifecycle, user-byte preservation, interrupted exports with source changes, marker/collision/missing-file conflicts, symlinks/hardlinks, permissions, lock contention, exact decimal/null handling, API retries/timeouts/redaction, configuration isolation and the offline CLI workflow. Synthetic fixtures are constructed in `internal/testutil` and test files; real financial fixtures must never be committed.
