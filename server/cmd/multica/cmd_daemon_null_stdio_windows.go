//go:build windows

// This file belongs to the mouse9527/multica fork (not present upstream).
package main

import "os"

// isNullDevice is always false on Windows, which keeps upstream's sink
// choice there unchanged; the null-stdio case this fork handles is
// launchd's.
func isNullDevice(_ *os.File) bool { return false }

func redirectNullStdioToErrLog(_ string, _ ...*os.File) error { return nil }
