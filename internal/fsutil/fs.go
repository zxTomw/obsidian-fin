// Package fsutil implements private state files and exclusive process locks.
package fsutil

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

func ID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func PrivateDir(path string) error {
	var created []string
	for current := path; ; current = filepath.Dir(current) {
		_, err := os.Stat(current)
		if err == nil {
			break
		}
		if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		created = append(created, current)
		if filepath.Dir(current) == current {
			return errors.New("cannot locate parent state directory")
		}
	}
	if err := os.MkdirAll(path, 0700); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("state directory must be a real directory")
	}
	if err := os.Chmod(path, 0700); err != nil {
		return err
	}
	// Flush parent entries for every newly created level, not just the leaf.
	for i := len(created) - 1; i >= 0; i-- {
		if err := SyncDir(filepath.Dir(created[i])); err != nil {
			return err
		}
	}
	return nil
}

func PrivateFile(path string) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_RDWR|unix.O_CREAT|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), path)
	info, err := f.Stat()
	var stat unix.Stat_t
	statErr := unix.Fstat(fd, &stat)
	if err != nil || statErr != nil || !info.Mode().IsRegular() || stat.Nlink > 1 {
		f.Close()
		return nil, errors.New("state file must be a regular file with one link")
	}
	if err := f.Chmod(0600); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

type Lock struct{ file *os.File }

func AcquireLock(path string) (*Lock, error) {
	f, err := PrivateFile(path)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("another obsidian-fin command holds the lock: %w", err)
	}
	return &Lock{file: f}, nil
}
func (l *Lock) Close() error { return l.file.Close() }

func SyncDir(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

func AtomicPrivate(path string, content []byte) error {
	id, err := ID()
	if err != nil {
		return err
	}
	tmp := filepath.Join(filepath.Dir(path), ".obsidian-fin-"+id+".tmp")
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	defer os.Remove(tmp)
	if _, err = f.Write(content); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	return SyncDir(filepath.Dir(path))
}
