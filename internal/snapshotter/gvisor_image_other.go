//go:build !linux

package snapshotter

import (
	"context"
	"fmt"
)

func setupImageRootfs(context.Context, string, string) error {
	return fmt.Errorf("gVisor image rootfs is only supported on linux")
}

func teardownImageRootfs(string) {}

func pauseImageRef() string { return defaultPauseImage }

const defaultPauseImage = "registry.k8s.io/pause:3.10"
