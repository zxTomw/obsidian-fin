//go:build !darwin && !linux

package export

import (
	"errors"
	"os"
)

func renameNew(*os.File, string, string) error {
	return errors.New("exclusive note creation requires macOS or Linux")
}
