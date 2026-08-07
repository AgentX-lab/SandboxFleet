//go:build !linux

package snapshotter

import "fmt"

func (k *Kata) prepareChildRootfsDirs(planned []virtiofsShare, sourceSandboxID, vmDir, snapDir string) ([]virtiofsShare, error) {
	return nil, fmt.Errorf("kata rootfs prepare is only supported on linux")
}

func unmountChildRootfs(string) error { return nil }
