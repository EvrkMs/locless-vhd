//go:build !windows

package main

import "errors"

// openTarget exists so the package builds everywhere.
func openTarget() (target, error) {
	return nil, errors.New("writing a disk needs Windows")
}

// openVHDTarget exists so the package builds everywhere.
func openVHDTarget(uint64) (vhdTarget, error) {
	return nil, errors.New("writing a virtual disk needs Windows")
}
