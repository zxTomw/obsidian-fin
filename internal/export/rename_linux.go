//go:build linux

package export

import (
	"golang.org/x/sys/unix"
	"os"
)

func renameNew(dir *os.File, from, to string) error {
	return unix.Renameat2(int(dir.Fd()), from, int(dir.Fd()), to, unix.RENAME_NOREPLACE)
}
