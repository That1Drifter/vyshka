//go:build windows

package main

import (
	"os"

	"golang.org/x/sys/windows"
)

// stdinIsTerminal reports whether stdin is a console someone can type into.
// A file mode check is not enough here: the NUL device a script redirects
// from is a character device too, so it would pass for a terminal and the
// destructive-action prompt would be put to nobody. Only a console handle
// answers GetConsoleMode.
func stdinIsTerminal() bool {
	var mode uint32
	return windows.GetConsoleMode(windows.Handle(os.Stdin.Fd()), &mode) == nil
}
