//go:build !linux

package snapshotter

import (
	"context"
	"fmt"
)

func (k *Kata) prepareChildRootfsDirs(context.Context, []virtiofsShare, kataRootfsPlan, string, string, string) ([]virtiofsShare, error) {
	return nil, fmt.Errorf("kata rootfs prepare is only supported on linux")
}

func unmountChildRootfs(string) error { return nil }
