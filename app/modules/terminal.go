package modules

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"syscall"
	"unsafe"
)

// isTerminal reports whether fd is a terminal.
func isTerminal(fd uintptr) bool {
	var t syscall.Termios
	_, _, e := syscall.Syscall(syscall.SYS_IOCTL, fd, syscall.TCGETS, uintptr(unsafe.Pointer(&t)))
	return e == 0
}

// stdin is the single buffered reader of standard input, shared by every
// read so that values given one per line are not lost between reads.
var stdin = bufio.NewReader(os.Stdin)

// ReadSecret reads a secret without putting it on the command line: from the
// terminal with echo turned off, or as the first line of standard input when
// it is redirected (for example from printf in a pipe, which does not appear
// in the process list or in the shell history as an argument).
func ReadSecret(prompt string) (string, error) {
	fd := os.Stdin.Fd()
	if isTerminal(fd) {
		fmt.Fprint(os.Stderr, prompt)
		var old syscall.Termios
		if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, fd, syscall.TCGETS, uintptr(unsafe.Pointer(&old))); e != 0 {
			return "", e
		}
		noEcho := old
		noEcho.Lflag &^= syscall.ECHO
		noEcho.Lflag |= syscall.ICANON | syscall.ISIG
		if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, fd, syscall.TCSETS, uintptr(unsafe.Pointer(&noEcho))); e != 0 {
			return "", e
		}
		defer func() {
			_, _, _ = syscall.Syscall(syscall.SYS_IOCTL, fd, syscall.TCSETS, uintptr(unsafe.Pointer(&old)))
			fmt.Fprintln(os.Stderr)
		}()
	}
	line, err := stdin.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	v := strings.TrimSpace(line)
	if v == "" {
		return "", errors.New("no value was given on standard input")
	}
	return v, nil
}
