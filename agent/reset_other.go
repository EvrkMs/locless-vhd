//go:build !windows

package main

import "errors"

// runReset exists so the package builds everywhere.
func runReset() error {
	return errors.New("the reset service needs Windows")
}
