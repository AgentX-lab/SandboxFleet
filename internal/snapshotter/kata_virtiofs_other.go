//go:build !linux

package snapshotter

import "fmt"

func (k *Kata) prepareChildRootfsDirs([]virtiofsShare, string, string, string) ([]virtiofsShare, error) {
	return nil, fmt.Errorf("kata rootfs prepare is only supported on linux")
}

func unmountChildRootfs(string) error { return nil }
