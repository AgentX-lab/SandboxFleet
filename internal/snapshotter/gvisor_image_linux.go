//go:build linux

package snapshotter

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/containerd/containerd"
	"github.com/containerd/containerd/mount"
	"github.com/containerd/containerd/namespaces"
	"github.com/opencontainers/image-spec/identity"
	"golang.org/x/sys/unix"
)

const (
	defaultPauseImage       = "registry.k8s.io/pause:3.10"
	gvisorImageViewKeyFile  = "image-view.key"
	gvisorImageLowerDirName = "lower"
)

func pauseImageRef() string {
	return firstNonEmpty(os.Getenv("SANDBOXFLEET_PAUSE_IMAGE"), defaultPauseImage)
}

func containerdAddress() string {
	return firstNonEmpty(os.Getenv("SANDBOXFLEET_CONTAINERD_ADDRESS"), "/run/containerd/containerd.sock")
}

func containerdNamespace() string {
	return firstNonEmpty(os.Getenv("SANDBOXFLEET_CONTAINERD_NAMESPACE"), "k8s.io")
}

// setupImageRootfs mounts a writable overlay rootfs from a containerd image,
// matching substrate's SetupBundleRootfs shape (image layers as lower, bundle
// upper/work as the private writable side).
func setupImageRootfs(ctx context.Context, imageRef, bundleDir string) error {
	if imageRef == "" {
		return fmt.Errorf("image ref is empty")
	}
	if err := os.MkdirAll(bundleDir, 0o755); err != nil {
		return err
	}
	rootfs := filepath.Join(bundleDir, "rootfs")
	upper := filepath.Join(bundleDir, "upper")
	work := filepath.Join(bundleDir, "work")
	lower := filepath.Join(bundleDir, gvisorImageLowerDirName)
	for _, d := range []string{rootfs, upper, work, lower} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}
	_ = unix.Unmount(rootfs, unix.MNT_DETACH)
	_ = unix.Unmount(lower, unix.MNT_DETACH)

	ctx = namespaces.WithNamespace(ctx, containerdNamespace())
	client, err := containerd.New(containerdAddress())
	if err != nil {
		return fmt.Errorf("containerd connect: %w", err)
	}
	defer client.Close()

	img, err := client.GetImage(ctx, imageRef)
	if err != nil {
		img, err = client.Pull(ctx, imageRef, containerd.WithPullUnpack)
		if err != nil {
			return fmt.Errorf("pull image %q: %w", imageRef, err)
		}
	} else if unpacked, uerr := img.IsUnpacked(ctx, containerd.DefaultSnapshotter); uerr == nil && !unpacked {
		if err := img.Unpack(ctx, containerd.DefaultSnapshotter); err != nil {
			return fmt.Errorf("unpack image %q: %w", imageRef, err)
		}
	}

	diffIDs, err := img.RootFS(ctx)
	if err != nil {
		return fmt.Errorf("image rootfs %q: %w", imageRef, err)
	}
	parent := identity.ChainID(diffIDs).String()
	viewKey := "sandboxfleet-gvisor-" + strings.ReplaceAll(filepath.Base(bundleDir), "/", "-") + "-" + shortDigest(parent)
	sn := client.SnapshotService(containerd.DefaultSnapshotter)
	_ = sn.Remove(ctx, viewKey)
	mounts, err := sn.View(ctx, viewKey, parent)
	if err != nil {
		return fmt.Errorf("snapshot view %q: %w", imageRef, err)
	}
	if err := os.WriteFile(filepath.Join(bundleDir, gvisorImageViewKeyFile), []byte(viewKey+"\n"), 0o600); err != nil {
		_ = sn.Remove(ctx, viewKey)
		return err
	}
	if err := mount.All(mounts, lower); err != nil {
		_ = sn.Remove(ctx, viewKey)
		return fmt.Errorf("mount image lower %q: %w", imageRef, err)
	}
	if err := mountWritableOverlay(rootfs, lower, upper, work); err != nil {
		_ = unix.Unmount(lower, unix.MNT_DETACH)
		_ = sn.Remove(ctx, viewKey)
		return err
	}
	// CRI images usually ship empty proc/sys/dev; ensure destinations for binds.
	for _, d := range []string{"proc", "sys", "dev", "etc"} {
		_ = os.MkdirAll(filepath.Join(rootfs, d), 0o755)
	}
	return nil
}

func mountWritableOverlay(mountpoint, lower, upper, work string) error {
	opts := fmt.Sprintf("lowerdir=%s,upperdir=%s,workdir=%s", lower, upper, work)
	if err := unix.Mount("overlay", mountpoint, "overlay", 0, opts); err != nil {
		return fmt.Errorf("mount overlay at %q: %w", mountpoint, err)
	}
	return nil
}

func teardownImageRootfs(bundleDir string) {
	if bundleDir == "" {
		return
	}
	rootfs := filepath.Join(bundleDir, "rootfs")
	lower := filepath.Join(bundleDir, gvisorImageLowerDirName)
	_ = unix.Unmount(rootfs, unix.MNT_DETACH)
	_ = unix.Unmount(lower, unix.MNT_DETACH)

	raw, err := os.ReadFile(filepath.Join(bundleDir, gvisorImageViewKeyFile))
	if err != nil {
		return
	}
	viewKey := strings.TrimSpace(string(raw))
	if viewKey == "" {
		return
	}
	ctx := namespaces.WithNamespace(context.Background(), containerdNamespace())
	client, err := containerd.New(containerdAddress())
	if err != nil {
		return
	}
	defer client.Close()
	_ = client.SnapshotService(containerd.DefaultSnapshotter).Remove(ctx, viewKey)
}

func shortDigest(s string) string {
	s = strings.TrimPrefix(s, "sha256:")
	if len(s) > 12 {
		return s[:12]
	}
	return s
}
