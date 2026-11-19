//go:build !windows

// This file belongs to the mouse9527/multica fork (not present upstream).
package main

import (
	"os"

	"golang.org/x/sys/unix"
)

// isNullDevice reports whether f is open on the null device.
func isNullDevice(f *os.File) bool {
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	null, err := os.Stat(os.DevNull)
	if err != nil {
		return false
	}
	return os.SameFile(fi, null)
}

// redirectNullStdioToErrLog points each of files that is open on the null
// device at the bounded crash sink. dup2 replaces the descriptor itself, so
// the Go runtime's own panic output, which writes to fd 2 directly, lands in
// the file too.
func redirectNullStdioToErrLog(path string, files ...*os.File) error {
	errLog, err := openBoundedErrLog(path)
	if err != nil {
		return err
	}
	defer errLog.Close()
	for _, f := range files {
		if !isNullDevice(f) {
			continue
		}
		if err := unix.Dup2(int(errLog.Fd()), int(f.Fd())); err != nil {
			return err
		}
	}
	return nil
}
