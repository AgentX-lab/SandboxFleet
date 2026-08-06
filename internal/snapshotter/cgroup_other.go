//go:build !linux

package snapshotter

// SetupCgroupDelegation is a no-op on non-Linux.
func SetupCgroupDelegation() error { return nil }
