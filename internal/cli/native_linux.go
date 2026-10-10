//go:build linux

package cli

import "golang.org/x/sys/unix"

const terminalGet = unix.TCGETS

func exchangeDirectories(a, b string) error {
	return unix.Renameat2(unix.AT_FDCWD, a, unix.AT_FDCWD, b, unix.RENAME_EXCHANGE)
}
