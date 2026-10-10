package export

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
	"obsidian-fin/internal/fsutil"
)

var ErrConflict = errors.New("export conflict")

type File struct {
	Exists bool
	Bytes  []byte
	Info   os.FileInfo
}
type Files interface {
	Read(string) (File, error)
	WriteAtomic(string, File, []byte) error
}

// Vault anchors every operation to an open directory handle. Even a path swap
// cannot redirect os.Root writes outside the resolved vault.
type Vault struct {
	root *os.Root
	Path string
	lock *os.File
}

func OpenVault(path string) (*Vault, error) {
	if !filepath.IsAbs(path) {
		return nil, errors.New("vault must be an absolute path")
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return nil, errors.New("vault must be an existing directory")
	}
	root, err := os.OpenRoot(resolved)
	if err != nil {
		return nil, err
	}
	v := &Vault{root: root, Path: resolved}
	if err := v.directory("Finance/obsidian-fin"); err != nil {
		root.Close()
		return nil, err
	}
	f, err := root.OpenFile("Finance/obsidian-fin/.export.lock", os.O_RDWR|os.O_CREATE|unix.O_NOFOLLOW, 0600)
	if err != nil {
		root.Close()
		return nil, err
	}
	var stat unix.Stat_t
	info, infoErr := f.Stat()
	if err := unix.Fstat(int(f.Fd()), &stat); err != nil || infoErr != nil || !info.Mode().IsRegular() || stat.Nlink != 1 {
		f.Close()
		root.Close()
		return nil, errors.New("export lock must have one link")
	}
	if err := f.Chmod(0600); err != nil {
		f.Close()
		root.Close()
		return nil, err
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		f.Close()
		root.Close()
		return nil, errors.New("another obsidian-fin export holds the vault lock")
	}
	v.lock = f
	return v, nil
}
func (v *Vault) Close() error { return errors.Join(v.lock.Close(), v.root.Close()) }
func local(path string) bool {
	return filepath.IsLocal(path) && strings.HasPrefix(filepath.ToSlash(path), "Finance/obsidian-fin/") && !strings.Contains(path, "\\")
}
func (v *Vault) directory(path string) error {
	if !filepath.IsLocal(path) {
		return ErrConflict
	}
	parts := strings.Split(filepath.ToSlash(path), "/")
	for i := range parts {
		prefix := strings.Join(parts[:i+1], "/")
		info, err := v.root.Lstat(prefix)
		if errors.Is(err, os.ErrNotExist) {
			if err := v.root.Mkdir(prefix, 0700); err != nil && !errors.Is(err, os.ErrExist) {
				return err
			}
			parent, openErr := v.root.Open(filepath.Dir(prefix))
			if openErr != nil {
				return openErr
			}
			syncErr := parent.Sync()
			closeErr := parent.Close()
			if syncErr != nil || closeErr != nil {
				return errors.Join(syncErr, closeErr)
			}
			info, err = v.root.Lstat(prefix)
		}
		if err != nil {
			return err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return ErrConflict
		}
	}
	return nil
}
func (v *Vault) Read(path string) (File, error) {
	if !local(path) {
		return File{}, ErrConflict
	}
	// Validate existing ancestors without creating anything during the safety check.
	parts := strings.Split(filepath.ToSlash(path), "/")
	for i := range parts {
		info, err := v.root.Lstat(strings.Join(parts[:i+1], "/"))
		if errors.Is(err, os.ErrNotExist) {
			return File{}, nil
		}
		if err != nil {
			return File{}, err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return File{}, ErrConflict
		}
		if i < len(parts)-1 && !info.IsDir() {
			return File{}, ErrConflict
		}
	}
	f, err := v.root.OpenFile(path, os.O_RDONLY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return File{}, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return File{}, err
	}
	var stat unix.Stat_t
	if err := unix.Fstat(int(f.Fd()), &stat); err != nil {
		return File{}, err
	}
	if !info.Mode().IsRegular() || stat.Nlink != 1 || info.Size() > 16<<20 {
		return File{}, ErrConflict
	}
	// Read through the same opened handle used for metadata; do not follow another pathname.
	content, err := readBounded(f, 16<<20)
	if err != nil {
		return File{}, err
	}
	return File{Exists: true, Bytes: content, Info: info}, nil
}
func same(a, b File) bool {
	if a.Exists != b.Exists {
		return false
	}
	if !a.Exists {
		return true
	}
	return os.SameFile(a.Info, b.Info) && a.Info.Size() == b.Info.Size() && a.Info.ModTime().Equal(b.Info.ModTime()) && bytes.Equal(a.Bytes, b.Bytes)
}
func (v *Vault) WriteAtomic(path string, before File, content []byte) error {
	if !local(path) {
		return ErrConflict
	}
	if err := v.directory(filepath.Dir(path)); err != nil {
		return err
	}
	id, err := fsutil.ID()
	if err != nil {
		return err
	}
	tmp := filepath.Join(filepath.Dir(path), ".obsidian-fin-"+id+".tmp")
	f, err := v.root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	defer v.root.Remove(tmp)
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
	now, err := v.Read(path)
	if err != nil {
		return err
	}
	if !same(before, now) {
		return ErrConflict
	}
	if before.Exists {
		if err := v.root.Rename(tmp, path); err != nil {
			return err
		}
	} else {
		// A native exclusive rename creates no transient hardlink alias, so a crash
		// at creation cannot make recovery reject our own two-link temporary file.
		dir, err := v.root.Open(filepath.Dir(path))
		if err != nil {
			return err
		}
		renameErr := renameNew(dir, filepath.Base(tmp), filepath.Base(path))
		closeErr := dir.Close()
		if renameErr != nil {
			if errors.Is(renameErr, os.ErrExist) {
				return ErrConflict
			}
			return renameErr
		}
		if closeErr != nil {
			return closeErr
		}
	}
	dir, err := v.root.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
