//go:build unix

package main

import (
	"os"

	"golang.org/x/sys/unix"
)

// stdinIsTerminal reports whether stdin is a terminal someone can type into.
// A file mode check is not enough here: /dev/null is a character device too,
// so a script redirecting from it would pass for a terminal and the
// destructive-action prompt would be put to nobody. Only a terminal answers
// the window-size ioctl, which every Unix names the same way.
func stdinIsTerminal() bool {
	_, err := unix.IoctlGetWinsize(int(os.Stdin.Fd()), unix.TIOCGWINSZ)
	return err == nil
}
