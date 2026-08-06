//go:build !linux

package snapshotter

// SetupCgroupDelegation is a no-op on non-Linux.
func SetupCgroupDelegation() error { return nil }

func restoreCgroupsPath(sandboxName string) string { return "/" + sandboxName }

func ensureRestoreCgroup(string) error { return nil }
