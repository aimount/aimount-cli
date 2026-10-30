//go:build linux

package cli

import "golang.org/x/sys/unix"

const terminalGet = unix.TCGETS
