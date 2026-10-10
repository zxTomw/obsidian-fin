package fsutil_test

import (
	"os"
	"path/filepath"
	"testing"

	"obsidian-fin/internal/fsutil"
)

func TestPrivateStateAtomicWriteAndExclusiveLock(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	if err := fsutil.PrivateDir(dir); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(dir)
	if err != nil || info.Mode().Perm() != 0700 {
		t.Fatal("state permissions")
	}
	path := filepath.Join(dir, "lock")
	lock, err := fsutil.AcquireLock(path)
	if err != nil {
		t.Fatal(err)
	}
	if second, err := fsutil.AcquireLock(path); err == nil {
		second.Close()
		t.Fatal("overlapping lock accepted")
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	again, err := fsutil.AcquireLock(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := again.Close(); err != nil {
		t.Fatal(err)
	}
	path = filepath.Join(dir, "state.json")
	if err := fsutil.AtomicPrivate(path, []byte("complete")); err != nil {
		t.Fatal(err)
	}
	info, err = os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("state file permissions")
	}
	if err := fsutil.AtomicPrivate(path, []byte("replacement")); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil || string(b) != "replacement" {
		t.Fatal("atomic replacement failed")
	}
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, []byte("untouched"), 0600); err != nil {
		t.Fatal(err)
	}
	symlink := filepath.Join(dir, "symlink")
	if err := os.Symlink(outside, symlink); err != nil {
		t.Fatal(err)
	}
	if f, err := fsutil.PrivateFile(symlink); err == nil {
		f.Close()
		t.Fatal("state symlink accepted")
	}
	hardlink := filepath.Join(dir, "hardlink")
	if err := os.Link(outside, hardlink); err != nil {
		t.Fatal(err)
	}
	if f, err := fsutil.PrivateFile(hardlink); err == nil {
		f.Close()
		t.Fatal("state hardlink accepted")
	}
}
