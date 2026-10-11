//go:build darwin

package terminal

import (
	"bytes"
	"context"
	"errors"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
	"golang.org/x/term"
	"obsidian-fin/internal/errs"
)

// Native Darwin PTYs keep these tests credential-free and independent of the
// terminal emulator or additional test dependencies.
func terminalPair(t *testing.T) (*os.File, *os.File) {
	t.Helper()
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { master.Close() })
	fd := int(master.Fd())
	for _, request := range []uint{unix.TIOCPTYGRANT, unix.TIOCPTYUNLK} {
		if err := unix.IoctlSetInt(fd, request, 0); err != nil {
			t.Fatal(err)
		}
	}
	var name [128]byte
	_, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), uintptr(unix.TIOCPTYGNAME), uintptr(unsafe.Pointer(&name[0])))
	if errno != 0 {
		t.Fatal(errno)
	}
	slave, err := os.OpenFile(unix.ByteSliceToString(name[:]), os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { slave.Close() })
	return master, slave
}

type promptCapture struct {
	mu     sync.Mutex
	buffer bytes.Buffer
	ready  chan struct{}
	once   sync.Once
}

func (p *promptCapture) Write(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	n, err := p.buffer.Write(b)
	if bytes.Contains(b, []byte("Plaid client secret")) {
		p.once.Do(func() { close(p.ready) })
	}
	return n, err
}
func (p *promptCapture) String() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.buffer.String()
}

func TestHiddenPromptOnDarwinTTY(t *testing.T) {
	for _, tc := range []struct {
		name, input, want string
		code              int
		cancel            bool
	}{
		{name: "delayed input", input: "synthetic-secret\r", want: "synthetic-secret"},
		{name: "pasted input", input: strings.Repeat("abcdef01", 8) + "\n", want: strings.Repeat("abcdef01", 8)},
		{name: "backspace", input: "syntheticX\x7f-secret\r", want: "synthetic-secret"},
		{name: "empty input", input: "\r", code: 2},
		{name: "control C", input: "\x03", code: 130},
		{name: "control D", input: "\x04", code: 130},
		{name: "context cancellation", cancel: true, code: 130},
		{name: "input limit", input: strings.Repeat("a", 4097), code: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			master, slave := terminalPair(t)
			before, err := term.GetState(int(slave.Fd()))
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			output := &promptCapture{ready: make(chan struct{})}
			type result struct {
				value string
				err   error
			}
			done := make(chan result, 1)
			go func() { value, err := readSecret(ctx, output, slave); done <- result{value, err} }()
			select {
			case <-output.ready:
			case <-ctx.Done():
				t.Fatal("prompt did not start")
			}
			// No keystroke is available yet. The prompt must wait rather than
			// mistaking Darwin's unsupported device polling for a closed TTY.
			select {
			case r := <-done:
				t.Fatalf("prompt exited before input: %v", r.err)
			case <-time.After(100 * time.Millisecond):
			}
			settings, err := unix.IoctlGetTermios(int(slave.Fd()), unix.TIOCGETA)
			if err != nil {
				t.Fatal(err)
			}
			if settings.Lflag&unix.ECHO != 0 {
				t.Fatal("secret input would be echoed")
			}
			if tc.cancel {
				cancel()
			} else if _, err := master.Write([]byte(tc.input)); err != nil {
				t.Fatal(err)
			}
			var got result
			select {
			case got = <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("prompt did not complete")
			}
			if errs.ExitCode(got.err) != tc.code || got.value != tc.want {
				t.Fatalf("unexpected prompt outcome: code %d, expected %d; correct value: %v", errs.ExitCode(got.err), tc.code, got.value == tc.want)
			}
			if tc.code == 130 && !errors.Is(got.err, context.Canceled) {
				t.Fatal("cancellation cause lost")
			}
			after, err := term.GetState(int(slave.Fd()))
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(before, after) {
				t.Fatal("terminal state not restored")
			}
			if tc.want != "" && strings.Contains(output.String(), tc.want) {
				t.Fatal("secret leaked to prompt output")
			}
		})
	}
}
