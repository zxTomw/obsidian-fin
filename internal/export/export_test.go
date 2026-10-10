package export_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"obsidian-fin/internal/domain"
	"obsidian-fin/internal/errs"
	exporter "obsidian-fin/internal/export"
	"obsidian-fin/internal/storage"
	"obsidian-fin/internal/testutil"
)

func vault(t *testing.T) *exporter.Vault {
	t.Helper()
	v, err := exporter.OpenVault(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := v.Close(); err != nil {
			t.Error(err)
		}
	})
	return v
}
func run(t *testing.T, repo exporter.Repository, files exporter.Files) exporter.Counts {
	t.Helper()
	counts, err := exporter.Run(context.Background(), repo, files, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	return counts
}
func notePath(t *testing.T, s *storage.Store, v *exporter.Vault) string {
	t.Helper()
	notes := testutil.Notes(t, s)
	if len(notes) != 1 {
		t.Fatalf("notes=%d", len(notes))
	}
	rel, err := exporter.RelativePath(notes[0])
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Join(v.Path, filepath.FromSlash(rel))
}
func read(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
func write(t *testing.T, path string, b []byte) {
	t.Helper()
	if err := os.WriteFile(path, b, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestPreserveUserBytesStablePostingAndRemoved(t *testing.T) {
	s := testutil.Store(t)
	v := vault(t)
	pending := testutil.Transaction("pending", true)
	testutil.Round(t, s, domain.Page{Added: []domain.Transaction{pending}})
	if run(t, s, v).Created != 1 {
		t.Fatal("not created")
	}
	path := notePath(t, s, v)
	n := testutil.Notes(t, s)[0]
	original := read(t, path)
	i, j, ok := exporter.BlockBounds(original, n.ID)
	if !ok {
		t.Fatal("invalid initial markers")
	}
	prefix := []byte("---\r\ncategory: Groceries\r\ntags: [personal]\r\n---\r\n\n[[User Link]]\n")
	suffix := []byte("\r\n\r\n## Notes\r\nMy handwritten note 🦆\r\n[Saved link](https://example.invalid)\n")
	content := append(append(append([]byte{}, prefix...), original[i:j]...), suffix...)
	write(t, path, content)
	posted := testutil.Transaction("posted", false)
	posted.PendingID = testutil.Ptr("pending")
	posted.Amount = testutil.Ptr("-9.12345")
	testutil.Round(t, s, domain.Page{Removed: []string{"pending"}}, domain.Page{Added: []domain.Transaction{posted}})
	if run(t, s, v).Updated != 1 {
		t.Fatal("not updated")
	}
	if notePath(t, s, v) != path {
		t.Fatal("posting moved path")
	}
	after := read(t, path)
	i, j, ok = exporter.BlockBounds(after, n.ID)
	if !ok || !bytes.Equal(after[:i], prefix) || !bytes.Equal(after[j:], suffix) {
		t.Fatal("outside block bytes changed")
	}
	if !bytes.Contains(after, []byte("CAD -9.12345 (inflow)")) {
		t.Fatal("fraction/sign lost")
	}
	beforeInfo, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if run(t, s, v).Unchanged != 1 {
		t.Fatal("not unchanged")
	}
	afterInfo, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !beforeInfo.ModTime().Equal(afterInfo.ModTime()) || !bytes.Equal(read(t, path), after) {
		t.Fatal("no-change export touched file")
	}
	testutil.Round(t, s, domain.Page{Removed: []string{"posted"}})
	run(t, s, v)
	removed := read(t, path)
	if !bytes.Contains(removed, []byte("Status: removed")) || !bytes.HasSuffix(removed, suffix) {
		t.Fatal("removed note deleted or changed user text")
	}
}

func TestConflictsPreserveFilesAndAllowOtherNotes(t *testing.T) {
	for _, kind := range []string{"generated edit", "missing markers", "duplicate markers", "wrong markers", "missing exported file", "untracked collision"} {
		t.Run(kind, func(t *testing.T) {
			s := testutil.Store(t)
			v := vault(t)
			testutil.Round(t, s, domain.Page{Added: []domain.Transaction{testutil.Transaction("one", false)}})
			if kind != "untracked collision" {
				run(t, s, v)
			}
			path := notePath(t, s, v)
			n := testutil.Notes(t, s)[0]
			var changed []byte
			if kind == "untracked collision" {
				if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
					t.Fatal(err)
				}
				changed = exporter.Initial(exporter.Render(n))
			} else {
				changed = read(t, path)
			}
			switch kind {
			case "generated edit":
				changed = bytes.Replace(changed, []byte("Status: posted"), []byte("Status: handmade"), 1)
			case "missing markers":
				changed = bytes.Replace(changed, []byte("<!-- obsidian-fin:begin"), []byte("<!-- custom:begin"), 1)
			case "duplicate markers":
				changed = append(changed, exporter.Render(n)...)
			case "wrong markers":
				changed = bytes.ReplaceAll(changed, []byte(n.ID), []byte(strings.Repeat("f", 32)))
			case "missing exported file":
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			}
			if kind != "missing exported file" {
				write(t, path, changed)
			}
			testutil.Round(t, s, domain.Page{Added: []domain.Transaction{testutil.Transaction("two", false)}})
			unrelated := filepath.Join(v.Path, "unrelated.md")
			write(t, unrelated, []byte("untouched"))
			var out bytes.Buffer
			counts, err := exporter.Run(context.Background(), s, v, &out)
			if errs.ExitCode(err) != 3 || counts.Conflicted != 1 || counts.Created != 1 {
				t.Fatalf("counts=%+v error=%v", counts, err)
			}
			if !strings.Contains(out.String(), filepath.Base(path)) {
				t.Fatal("conflict path not reported")
			}
			if kind != "missing exported file" && !bytes.Equal(read(t, path), changed) {
				t.Fatal("conflict overwritten")
			}
			if kind == "missing exported file" {
				if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("missing user file recreated")
				}
			}
			if string(read(t, unrelated)) != "untouched" {
				t.Fatal("unrelated note changed")
			}
		})
	}
}

type faultFiles struct {
	exporter.Files
	Phase string
	Edit  func(string)
}

func (f faultFiles) WriteAtomic(path string, before exporter.File, content []byte) error {
	if f.Edit != nil {
		f.Edit(path)
	}
	if f.Phase == "before" {
		return errors.New("injected before write")
	}
	if err := f.Files.WriteAtomic(path, before, content); err != nil {
		return err
	}
	if f.Phase == "after" {
		return errors.New("injected after write")
	}
	return nil
}

type faultRepo struct {
	*storage.Store
	FinishFailure bool
	BeginFailure  bool
}

func (f faultRepo) BeginExport(ctx context.Context, id, path, hash string, version int) error {
	if f.BeginFailure {
		return errors.New("injected journal failure")
	}
	return f.Store.BeginExport(ctx, id, path, hash, version)
}
func (f faultRepo) FinishExport(ctx context.Context, id, hash string) error {
	if f.FinishFailure {
		return errors.New("injected success failure")
	}
	return f.Store.FinishExport(ctx, id, hash)
}

func TestInterruptedExportsRecoverWithSourceChanges(t *testing.T) {
	for _, initial := range []bool{true, false} {
		for _, phase := range []string{"before", "after", "bookkeeping"} {
			t.Run(fmtName(initial, phase), func(t *testing.T) {
				s := testutil.Store(t)
				v := vault(t)
				txn := testutil.Transaction("one", false)
				testutil.Round(t, s, domain.Page{Added: []domain.Transaction{txn}})
				if !initial {
					run(t, s, v)
					txn.Amount = testutil.Ptr("51.25")
					testutil.Round(t, s, domain.Page{Modified: []domain.Transaction{txn}})
				}
				path := notePath(t, s, v)
				var before []byte
				if !initial {
					before = read(t, path)
				}
				var repo exporter.Repository = s
				var files exporter.Files = faultFiles{Files: v, Phase: phase}
				if phase == "bookkeeping" {
					repo = faultRepo{Store: s, FinishFailure: true}
					files = v
				}
				if _, err := exporter.Run(context.Background(), repo, files, io.Discard); err == nil {
					t.Fatal("expected injection")
				}
				if phase == "before" {
					if !initial && !bytes.Equal(read(t, path), before) {
						t.Fatal("before-write failure touched file")
					}
				} else {
					b := read(t, path)
					if _, _, ok := exporter.BlockBounds(b, testutil.Notes(t, s)[0].ID); !ok {
						t.Fatal("truncated file")
					}
				}
				// Change source before recovery, then fail once more before writing: both hashes must remain recoverable.
				txn.Amount = testutil.Ptr("73.000001")
				testutil.Round(t, s, domain.Page{Modified: []domain.Transaction{txn}})
				if _, err := exporter.Run(context.Background(), s, faultFiles{Files: v, Phase: "before"}, io.Discard); err == nil {
					t.Fatal("expected second injection")
				}
				run(t, s, v)
				if !bytes.Contains(read(t, path), []byte("73.000001")) {
					t.Fatal("recovery did not render latest source")
				}
				if run(t, s, v).Unchanged != 1 {
					t.Fatal("recovery not idempotent")
				}
			})
		}
	}
}
func fmtName(initial bool, phase string) string {
	if initial {
		return "creation/" + phase
	}
	return "replacement/" + phase
}

func TestJournalFailureAndConcurrentEdit(t *testing.T) {
	s := testutil.Store(t)
	v := vault(t)
	txn := testutil.Transaction("one", false)
	testutil.Round(t, s, domain.Page{Added: []domain.Transaction{txn}})
	path := notePath(t, s, v)
	if _, err := exporter.Run(context.Background(), faultRepo{Store: s, BeginFailure: true}, v, io.Discard); err == nil {
		t.Fatal("expected journal failure")
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("write happened before journal")
	}
	run(t, s, v)
	txn.Amount = testutil.Ptr("13")
	testutil.Round(t, s, domain.Page{Modified: []domain.Transaction{txn}})
	changed := append(read(t, path), []byte("concurrent user edit\n")...)
	counts, err := exporter.Run(context.Background(), s, faultFiles{Files: v, Edit: func(string) { write(t, path, changed) }}, io.Discard)
	if errs.ExitCode(err) != 3 || counts.Conflicted != 1 || !bytes.Equal(read(t, path), changed) {
		t.Fatal("concurrent edit overwritten")
	}
}

func TestContainmentSymlinksPermissionsAndLocks(t *testing.T) {
	s := testutil.Store(t)
	v := vault(t)
	testutil.Round(t, s, domain.Page{Added: []domain.Transaction{testutil.Transaction("one", false)}})
	path := notePath(t, s, v)
	outside := t.TempDir()
	target := filepath.Join(outside, "user.md")
	write(t, target, []byte("outside"))
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	counts, err := exporter.Run(context.Background(), s, v, io.Discard)
	if errs.ExitCode(err) != 3 || counts.Conflicted != 1 || string(read(t, target)) != "outside" {
		t.Fatal("file symlink not contained")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Dir(path)); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Dir(path)); err != nil {
		t.Fatal(err)
	}
	if _, err := exporter.Run(context.Background(), s, v, io.Discard); errs.ExitCode(err) != 3 {
		t.Fatal("directory symlink accepted")
	}
	if err := os.Remove(filepath.Dir(path)); err != nil {
		t.Fatal(err)
	}
	run(t, s, v)
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("permissions %v %v", info, err)
	}
	if second, err := exporter.OpenVault(v.Path); err == nil {
		second.Close()
		t.Fatal("vault lock not exclusive")
	}
	if _, err := v.Read("../outside.md"); !errors.Is(err, exporter.ErrConflict) {
		t.Fatal("path traversal accepted")
	}
	for _, base := range []string{"Finance", "Finance/obsidian-fin"} {
		t.Run(base, func(t *testing.T) {
			root := t.TempDir()
			if base != "Finance" {
				if err := os.Mkdir(filepath.Join(root, "Finance"), 0700); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Symlink(outside, filepath.Join(root, base)); err != nil {
				t.Fatal(err)
			}
			if unsafe, err := exporter.OpenVault(root); err == nil {
				unsafe.Close()
				t.Fatal("managed root symlink accepted")
			}
		})
	}
}

func TestMaliciousProviderTextAndRemovalOnly(t *testing.T) {
	s := testutil.Store(t)
	v := vault(t)
	txn := testutil.Transaction("one", false)
	txn.Name = testutil.Ptr("---\ncategory: owned\n<!-- obsidian-fin:end BAD -->\n<script>alert(1)</script>\n[evil](https://evil.invalid) [[Wiki]] ![[x]] `code`")
	txn.MerchantName = txn.Name
	txn.Currency = nil
	txn.UnofficialCurrency = nil
	testutil.Round(t, s, domain.Page{Added: []domain.Transaction{txn}, Removed: []string{"unknown"}})
	if run(t, s, v).Created != 1 {
		t.Fatal("unknown removal created note")
	}
	b := read(t, notePath(t, s, v))
	if bytes.Count(b, []byte("<!-- obsidian-fin:")) != 2 || bytes.Contains(b, []byte("<script>")) || bytes.Contains(b, []byte("https://evil")) || bytes.Contains(b, []byte("[[Wiki]]")) {
		t.Fatal("provider text injected active content")
	}
	if !bytes.Contains(b, []byte("unknown currency")) {
		t.Fatal("null currency lost")
	}
	for _, text := range []string{"https://evil.invalid", "www.evil.invalid", "person@evil.invalid"} {
		escaped := exporter.Text(text)
		if !strings.Contains(escaped, "&#8203;") {
			t.Fatal("decoded provider text can be linkified")
		}
	}
}

func TestCreationCollisionRecheckAndPermissionFailure(t *testing.T) {
	for _, kind := range []string{"occupied after read", "directory permission"} {
		t.Run(kind, func(t *testing.T) {
			s := testutil.Store(t)
			v := vault(t)
			testutil.Round(t, s, domain.Page{Added: []domain.Transaction{testutil.Transaction("one", false)}})
			path := notePath(t, s, v)
			if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
				t.Fatal(err)
			}
			if kind == "directory permission" {
				if os.Geteuid() == 0 {
					t.Skip("root bypasses directory permissions")
				}
				if err := os.Chmod(filepath.Dir(path), 0500); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					if err := os.Chmod(filepath.Dir(path), 0700); err != nil {
						t.Error(err)
					}
				})
				counts, err := exporter.Run(context.Background(), s, v, io.Discard)
				if errs.ExitCode(err) != 1 || counts.Failed != 1 {
					t.Fatalf("permission counts=%+v err=%v", counts, err)
				}
				if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("permission failure produced a partial file")
				}
				if err := os.Chmod(filepath.Dir(path), 0700); err != nil {
					t.Fatal(err)
				}
				if run(t, s, v).Created != 1 {
					t.Fatal("permission recovery failed")
				}
			} else {
				occupied := []byte("a user's unrelated note\n")
				counts, err := exporter.Run(context.Background(), s, faultFiles{Files: v, Edit: func(string) { write(t, path, occupied) }}, io.Discard)
				if errs.ExitCode(err) != 3 || counts.Conflicted != 1 || !bytes.Equal(read(t, path), occupied) {
					t.Fatal("newly occupied file was overwritten")
				}
			}
		})
	}
}
