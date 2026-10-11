# Validation record

Validated on **2026-10-11**, on **macOS arm64**, using pinned **Go 1.26.9** and the native cgo Keychain build. The application binary is `bin/obsidian-fin`, version **0.1.0**. This is the v1 source implementation/release candidate; live account acceptance remains separate.

The executable, Go module/entry point, CLI messages, Plaid display name, state/database paths, Keychain namespace, managed notes/markers and documentation all use **obsidian-fin**. The complete `make check` passed after the rename; the rebuilt binary's version, help, error messages and database path were also checked.

Configuration now defaults to `~/.config/obsidian-fin/config.toml`, while SQLite state remains in Application Support. The full `make check` passed after this change. The requested personal configuration contains the supplied non-secret client ID with Sandbox/CA/90-day defaults, directory mode `0700` and file mode `0600`; no client secret is stored in TOML.

## Completed checks

| Check | Result |
| --- | --- |
| `go test ./...` | PASS — 47 offline test functions, table-driven cases and decimal fuzz seeds |
| `go test -race ./...` | PASS — no race reports |
| `go vet ./...` | PASS |
| `make build` / native macOS build | PASS — Mach-O arm64 executable, cgo enabled, Security/CoreFoundation frameworks linked |
| `make check` | PASS — all four required checks together after the final source changes |
| `govulncheck` v1.8.0, Go 1.26.9 and final dependency set | PASS — zero reported vulnerability findings |
| Compiled binary CLI checks | PASS — help/version: 0; unknown command/invalid environment: 2; missing connection: 1 |
| `gofmt` | PASS — source formatted |
| Opt-in Sandbox smoke test compilation | PASS — test compiled and deliberately skipped without opt-in/credentials |
| macOS hidden secret prompt | PASS — native pseudoterminal cases for delayed/pasted input, backspace, no echo, empty/oversized input, Ctrl-C/Ctrl-D, context cancellation and terminal restoration; direct `/dev/tty` entry verified using synthetic input |

The installed Go 1.26.1 toolchain initially had vulnerability findings. The project was repinned and rebuilt using Go 1.26.9, and the SDK's older OAuth dependency was upgraded to pinned `golang.org/x/oauth2` v0.37.0. The subsequent scan reported zero findings. Modules are pinned/checksummed in `go.mod` and `go.sum`; the temporary build cache is not a runtime dependency.

## Offline evidence

| Area | Evidence |
| --- | --- |
| Connection | Mocked Hosted Link completion, cancellation, expiry, timeout, update without exchange, repeated connect, Keychain denial, metadata/SQLite failure, ambiguous exchange guard, failed cleanup token retention, revocation checkpoint after delete failure, committed-Item recovery and stable local user ID |
| Official SDK adapter | Initial product/history/Hosted Link requests, update omission of initial settings, session result/public-token handling, both exit error formats, initial cursor omission, exact large/fractional JSON amounts and nullable fields, BMO/country/Transactions checks and discarded balances |
| Retries/privacy | Bounded network/429/5xx retries, jitter/backoff, Retry-After, per-request timeout, cancellation, exchange not retried, sanitized error codes/request IDs, body/URL/token redaction, and environment/token references |
| SQLite/sync | Multi-page initial/incremental rounds, initial empty result, final cursor/timestamp, page failure, write and deferred-commit failure rollback, original-cursor mutation restart/exhaustion, replay identity and one Item per database |
| Provider lifecycle | Posted modifications, both page orders for pending removal/posted addition, unseen pending reservation without removal, late predecessor arrival, repeated/unknown removals, superseded predecessor removal, posted removal and unmatched distinct postings |
| Edit preservation | User frontmatter/categories/tags/links/Notes preserved byte-for-byte, stable pending-to-posted path, removed status without deletion, unchanged bytes/mtime and deterministic generated blocks |
| Export recovery | Journal failures, failures before/after first creation or replacement and before success bookkeeping, source changes before recovery, a second interrupted recovery, complete files and accepted pending-block promotion |
| Conflicts/containment | Edited/missing/duplicate/wrong markers, missing exported file, untracked matching-path collision, a new collision after the initial read, concurrent edits, file/directory/root symlinks, hardlinked state-file rejection, traversal, private permissions, permission failure/recovery, state/vault lock contention and unchanged unrelated notes |
| CLI/configuration | Strict TOML/flag/default precedence, unsupported secret keys and invalid-value redaction, help/version, scoped command arguments/exit codes, environment isolation including a copied database, state and environment directories outside the vault, and offline connect → sync → export using injected providers/secrets |

The storage/sync packages also received a separate read-only review. It found no blocking atomicity, query-safety or lifecycle issues. Its suggested export-journal row-count checks and stricter reserved/tombstone constraints were implemented and tested.

The macOS secret prompt originally treated `POLLNVAL` from an open `/dev/tty` descriptor as closed input. This was reproduced directly; [Apple's poll(2) documentation](https://developer.apple.com/library/archive/documentation/System/Conceptual/ManPages_iPhoneOS/man2/poll.2.html) documents device polling limitations. Readiness now uses `select(2)` with bounded waits for cancellation. The corrected prompt accepted hidden synthetic input in an interactive macOS terminal, restored terminal settings, and passed the full check suite. No test secret was stored in Keychain or sent to Plaid.

## Pending external milestones

- **Sandbox browser/Keychain smoke:** not run. The client ID is configured, but a Plaid client secret and an existing test-vault path were not supplied. Native Keychain/framework code compiled; actual OS permission prompts and browser authorization still require the opted-in user-run test documented in [README.md](README.md).
- **Sandbox transaction inspection:** not run against Plaid. The SQLite, synchronization and export evidence above uses synthetic offline data.
- **Live BMO:** not run or claimed. Verify Transactions access in the selected Plaid environment, the appropriate CA/US BMO institution/coverage, and required OAuth registration before the live workflow. No live access/coverage verdict is inferred from mocks or Sandbox.
- **Other platforms:** Intel macOS and Linux execution were not tested on this arm64 host. Actual Keychain connection requires macOS with cgo; non-macOS/no-cgo adapters fail safely without storing plaintext secrets.

No Plaid/BMO API calls, bank credentials, real financial fixtures, user Keychain entries or real vault notes were used by the default checks. See [README.md](README.md) for setup, the opt-in Sandbox smoke, live acceptance, conflict recovery and manual revocation/removal.
