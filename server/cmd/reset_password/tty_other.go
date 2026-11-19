//go:build !linux && !darwin

package main

import (
	"errors"
	"os"
)

// disableEcho is only implemented where the server image runs (Linux) and on
// macOS for local use. Elsewhere, pipe the password on stdin.
func disableEcho(*os.File) (func(), error) {
	return nil, errors.New("terminal input is not supported on this platform")
}
