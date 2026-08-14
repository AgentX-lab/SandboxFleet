//go:build !linux

package overlay

// EnsureSharedPropagation is a no-op off Linux (kata Workers are Linux-only).
func EnsureSharedPropagation(string) error { return nil }
