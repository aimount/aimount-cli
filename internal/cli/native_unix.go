//go:build darwin || linux

package cli

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

func authLock(path string) (*os.File, error) {
	fd, e := unix.Open(path, unix.O_CREAT|unix.O_RDWR|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
	if e != nil {
		return nil, errors.New("invalid or non-owner-only auth lock")
	}
	f := os.NewFile(uintptr(fd), path)
	if e = checkLockIdentity(f, path); e != nil {
		f.Close()
		return nil, e
	}
	if e = unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); e != nil {
		f.Close()
		return nil, commandError("validation_failed", "auth lock unavailable; retry explicitly", 2)
	}
	if e = checkLockIdentity(f, path); e != nil {
		f.Close()
		return nil, e
	}
	return f, nil
}
func isTerminal(f *os.File) bool {
	_, e := unix.IoctlGetTermios(int(f.Fd()), terminalGet)
	return e == nil
}
