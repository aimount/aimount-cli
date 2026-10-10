//go:build darwin

package cli

import "golang.org/x/sys/unix"

const terminalGet = unix.TIOCGETA

func exchangeDirectories(a, b string) error {
	return unix.RenameatxNp(unix.AT_FDCWD, a, unix.AT_FDCWD, b, unix.RENAME_SWAP)
}
