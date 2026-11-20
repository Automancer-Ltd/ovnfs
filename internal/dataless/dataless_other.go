//go:build !darwin

package dataless

// Materialize does nothing: only macOS fails reads of cloud-only files by
// policy.
func Materialize() error { return nil }
