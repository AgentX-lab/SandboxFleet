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

const defaultPauseImage = "registry.k8s.io/pause:3.10"

func pauseImageRef() string {
	return firstNonEmpty(os.Getenv("SANDBOXFLEET_PAUSE_IMAGE"), defaultPauseImage)
}

func containerdAddress() string {
	return firstNonEmpty(os.Getenv("SANDBOXFLEET_CONTAINERD_ADDRESS"), "/run/containerd/containerd.sock")
}

func containerdNamespace() string {
	return firstNonEmpty(os.Getenv("SANDBOXFLEET_CONTAINERD_NAMESPACE"), "k8s.io")
}

// setupImageRootfs mounts a writable overlay rootfs from containerd image
// layer directories — same shape as substrate SetupBundleRootfs:
// lowers = image layer fs dirs, upper/work = bundle-private, one overlay mount.
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
	for _, d := range []string{rootfs, upper, work} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}
	_ = unix.Unmount(rootfs, unix.MNT_DETACH)

	ctx = namespaces.WithNamespace(ctx, containerdNamespace())
	client, err := containerd.New(containerdAddress())
	if err != nil {
		return fmt.Errorf("containerd connect: %w", err)
	}
	defer client.Close()

	img, err := getOrPullContainerdImage(ctx, client, imageRef)
	if err != nil {
		return err
	}

	diffIDs, err := img.RootFS(ctx)
	if err != nil {
		return fmt.Errorf("image rootfs %q: %w", imageRef, err)
	}
	parent := identity.ChainID(diffIDs).String()
	sn := client.SnapshotService(containerd.DefaultSnapshotter)

	// Temporary View only to resolve committed layer paths; never mount it
	// (mounting View then overlay-on-overlay yields EINVAL on many kernels).
	viewKey := "sandboxfleet-gvisor-layers-" + filepath.Base(bundleDir) + "-" + shortDigest(parent)
	_ = sn.Remove(ctx, viewKey)
	mounts, err := sn.View(ctx, viewKey, parent)
	if err != nil {
		return fmt.Errorf("snapshot view %q: %w", imageRef, err)
	}
	lowers, err := lowerDirsFromMounts(mounts)
	_ = sn.Remove(ctx, viewKey)
	if err != nil {
		return fmt.Errorf("resolve layer dirs %q: %w", imageRef, err)
	}
	if len(lowers) == 0 {
		return fmt.Errorf("image %q has no overlay lowerdirs", imageRef)
	}

	if err := mountOverlayLowers(rootfs, lowers, upper, work); err != nil {
		return err
	}
	// Ensure bind-mount destinations exist in the merged rootfs.
	for _, d := range []string{"proc", "sys", "dev", "etc"} {
		_ = os.MkdirAll(filepath.Join(rootfs, d), 0o755)
	}
	return nil
}

// getOrPullContainerdImage resolves short names like substrate/CRI, prefers a
// local GetImage hit, and only Pulls with a normalized reference.
func getOrPullContainerdImage(ctx context.Context, client *containerd.Client, imageRef string) (containerd.Image, error) {
	candidates := imageRefCandidates(imageRef)
	var lastErr error
	for _, c := range candidates {
		img, err := client.GetImage(ctx, c)
		if err != nil {
			lastErr = err
			continue
		}
		if unpacked, uerr := img.IsUnpacked(ctx, containerd.DefaultSnapshotter); uerr == nil && !unpacked {
			if err := img.Unpack(ctx, containerd.DefaultSnapshotter); err != nil {
				return nil, fmt.Errorf("unpack image %q: %w", c, err)
			}
		}
		return img, nil
	}
	pullRef := imageRef
	if n, err := normalizeImageRef(imageRef); err == nil {
		pullRef = n
	}
	img, err := client.Pull(ctx, pullRef, containerd.WithPullUnpack)
	if err != nil {
		if lastErr != nil {
			return nil, fmt.Errorf("pull image %q (GetImage tried %v: %v): %w", pullRef, candidates, lastErr, err)
		}
		return nil, fmt.Errorf("pull image %q: %w", pullRef, err)
	}
	return img, nil
}

// lowerDirsFromMounts extracts overlay lowerdir paths (top-most first) from a
// containerd snapshot View/Prepare mount list without mounting it.
func lowerDirsFromMounts(mounts []mount.Mount) ([]string, error) {
	var lowers []string
	for _, m := range mounts {
		switch m.Type {
		case "overlay", "overlayfs":
			for _, opt := range m.Options {
				if rest, ok := strings.CutPrefix(opt, "lowerdir="); ok {
					parts := splitOverlayLowerdirs(rest)
					if len(parts) == 0 {
						return nil, fmt.Errorf("empty lowerdir in mount options")
					}
					return parts, nil
				}
			}
		case "bind":
			if m.Source != "" {
				lowers = append(lowers, m.Source)
			}
		}
	}
	if len(lowers) > 0 {
		return lowers, nil
	}
	return nil, fmt.Errorf("no overlay/bind lowerdirs in %#v", mounts)
}

// splitOverlayLowerdirs splits overlay lowerdir= values. Paths use ':' as
// separator; '\:' is a literal colon (containerd escaping).
func splitOverlayLowerdirs(v string) []string {
	var out []string
	var b strings.Builder
	escaped := false
	for _, r := range v {
		if escaped {
			b.WriteRune(r)
			escaped = false
			continue
		}
		if r == '\\' {
			escaped = true
			continue
		}
		if r == ':' {
			out = append(out, b.String())
			b.Reset()
			continue
		}
		b.WriteRune(r)
	}
	if b.Len() > 0 || strings.HasSuffix(v, ":") {
		out = append(out, b.String())
	}
	return out
}

// mountOverlayLowers mounts one writable overlay (substrate-style). Prefers
// fsconfig lowerdir+ (kernel ≥ 6.5); falls back to classic mount(2).
func mountOverlayLowers(mountpoint string, lowers []string, upper, work string) error {
	if err := mountOverlayFSConfig(mountpoint, lowers, upper, work); err == nil {
		return nil
	} else if !isFsconfigUnsupported(err) {
		return err
	}
	return mountOverlayClassic(mountpoint, lowers, upper, work)
}

func isFsconfigUnsupported(err error) bool {
	return err != nil && (strings.Contains(err.Error(), "fsopen") ||
		strings.Contains(err.Error(), "fsconfig") ||
		strings.Contains(err.Error(), "function not implemented") ||
		strings.Contains(err.Error(), "invalid argument"))
}

func mountOverlayFSConfig(mountpoint string, lowers []string, upper, work string) error {
	fsfd, err := unix.Fsopen("overlay", unix.FSOPEN_CLOEXEC)
	if err != nil {
		return fmt.Errorf("fsopen overlay: %w", err)
	}
	defer unix.Close(fsfd)

	set := func(key, val string) error {
		if err := unix.FsconfigSetString(fsfd, key, val); err != nil {
			return fmt.Errorf("fsconfig %s=%q: %w", key, val, err)
		}
		return nil
	}
	for _, lower := range lowers {
		if err := set("lowerdir+", lower); err != nil {
			return err
		}
	}
	if err := set("upperdir", upper); err != nil {
		return err
	}
	if err := set("workdir", work); err != nil {
		return err
	}
	if err := unix.FsconfigCreate(fsfd); err != nil {
		return fmt.Errorf("fsconfig create: %w", err)
	}
	mfd, err := unix.Fsmount(fsfd, unix.FSMOUNT_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("fsmount: %w", err)
	}
	defer unix.Close(mfd)
	if err := unix.MoveMount(mfd, "", unix.AT_FDCWD, mountpoint, unix.MOVE_MOUNT_F_EMPTY_PATH); err != nil {
		return fmt.Errorf("move_mount to %q: %w", mountpoint, err)
	}
	return nil
}

func mountOverlayClassic(mountpoint string, lowers []string, upper, work string) error {
	escaped := make([]string, len(lowers))
	for i, l := range lowers {
		escaped[i] = strings.ReplaceAll(l, `\`, `\\`)
		escaped[i] = strings.ReplaceAll(escaped[i], `:`, `\:`)
		escaped[i] = strings.ReplaceAll(escaped[i], `,`, `\,`)
	}
	opts := fmt.Sprintf("lowerdir=%s,upperdir=%s,workdir=%s",
		strings.Join(escaped, ":"), upper, work)
	if err := unix.Mount("overlay", mountpoint, "overlay", 0, opts); err != nil {
		return fmt.Errorf("mount overlay at %q: %w", mountpoint, err)
	}
	return nil
}

func teardownImageRootfs(bundleDir string) {
	if bundleDir == "" {
		return
	}
	_ = unix.Unmount(filepath.Join(bundleDir, "rootfs"), unix.MNT_DETACH)
}

func shortDigest(s string) string {
	s = strings.TrimPrefix(s, "sha256:")
	if len(s) > 12 {
		return s[:12]
	}
	return s
}
