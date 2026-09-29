//go:build !unix && !windows

package main

// stdinIsTerminal is the answer on a platform without a console or terminal
// query this command knows: no terminal, so a destructive action always needs
// --yes there, which errs on the side of asking for the flag.
func stdinIsTerminal() bool {
	return false
}
