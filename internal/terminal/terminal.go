// Package terminal handles hidden local prompts and sensitive Hosted Link URLs.
package terminal

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"

	"golang.org/x/sys/unix"
	"golang.org/x/term"
	"obsidian-fin/internal/errs"
)

// ReadSecret never echoes input and remains cancelable while waiting at the terminal.
func ReadSecret(ctx context.Context, output io.Writer) (value string, err error) {
	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return "", errs.New(1, "a local terminal is required to enter the missing Plaid secret; run connect interactively")
	}
	defer tty.Close()
	return readSecret(ctx, output, tty)
}

func readSecret(ctx context.Context, output io.Writer, tty *os.File) (value string, err error) {
	fd := int(tty.Fd())
	if fd < 0 || fd >= unix.FD_SETSIZE {
		return "", errs.New(1, "terminal descriptor exceeds the supported range")
	}
	old, err := term.MakeRaw(fd)
	if err != nil {
		return "", errs.New(1, "could not enable the hidden terminal prompt")
	}
	defer func() {
		restoreErr := term.Restore(fd, old)
		if err == nil && restoreErr != nil {
			value = ""
			err = errs.New(1, "could not restore terminal settings")
		}
		fmt.Fprintln(output)
	}()
	fmt.Fprint(output, "Plaid client secret (hidden; stored in Keychain): ")
	var input []byte
	defer func() { clear(input) }()
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		// Darwin poll(2) does not support /dev/tty and can report POLLNVAL
		// for an open descriptor. select(2) supports this terminal device.
		var read unix.FdSet
		read.Set(fd)
		timeout := unix.Timeval{Usec: 100000}
		ready, err := unix.Select(fd+1, &read, nil, nil, &timeout)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return "", errs.New(1, "terminal input failed")
		}
		if ready == 0 || !read.IsSet(fd) {
			continue
		}
		var b [1]byte
		n, err := tty.Read(b[:])
		if err != nil || n != 1 {
			return "", errs.New(1, "terminal input failed")
		}
		switch b[0] {
		case 3, 4:
			return "", context.Canceled
		case '\r', '\n':
			if len(input) == 0 {
				return "", errs.New(2, "Plaid client secret cannot be empty")
			}
			return string(input), nil
		case 127, 8:
			if len(input) > 0 {
				input = input[:len(input)-1]
			}
		default:
			if b[0] >= 32 {
				if len(input) >= 4096 {
					return "", errs.New(2, "Plaid client secret is too long")
				}
				input = append(input, b[0])
			}
		}
	}
}

func OpenHosted(ctx context.Context, url string) error {
	if runtime.GOOS == "darwin" {
		// Only the temporary Hosted Link URL is passed to open; credentials and access tokens never are.
		cmd := exec.CommandContext(ctx, "/usr/bin/open", url)
		if err := cmd.Run(); err == nil {
			return nil
		} else if ctx.Err() != nil {
			return ctx.Err()
		}
	}
	// The URL is intentionally shown only on the controlling terminal, never in captured stdout/stderr.
	tty, err := os.OpenFile("/dev/tty", os.O_WRONLY, 0)
	if err != nil {
		return errs.New(1, "could not open Hosted Link; rerun connect in a local terminal for a sensitive URL fallback")
	}
	defer tty.Close()
	_, err = fmt.Fprintf(tty, "Open this sensitive Plaid URL in your browser. Do not share or log it:\n%s\n", strings.ReplaceAll(url, "\n", ""))
	if err != nil {
		return errs.New(1, "could not display Hosted Link on the controlling terminal")
	}
	return nil
}
