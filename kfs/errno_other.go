//go:build !windows

package kfs

import (
	"errors"
	"syscall"
)

// notEmpty reports whether err says a directory is not empty.
func notEmpty(err error) bool { return errors.Is(err, syscall.ENOTEMPTY) }
