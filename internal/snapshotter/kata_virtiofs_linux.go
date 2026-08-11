//go:build linux

package snapshotter

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"golang.org/x/sys/unix"
)

// prepareChildRootfsDirs materializes virtiofs shares for restore (substrate/gVisor-style).
func (k *Kata) prepareChildRootfsDirs(ctx context.Context, planned []virtiofsShare, plan kataRootfsPlan, sourceSandboxID, vmDir, snapDir string) ([]virtiofsShare, error) {
	live := k.findParentRootfsShares(sourceSandboxID)
	out := make([]virtiofsShare, 0, len(planned))
	for i, share := range planned {
		dst := childRootfsDir(vmDir, i)
		switch {
		case plan.appImage != "" && plan.containerID != "" && isKataSharedTag(share.Tag):
			if err := k.materializeKataSharedRootfs(ctx, dst, plan, share, snapDir); err != nil {
				_ = unmountChildRootfs(vmDir)
				return nil, err
			}
			share.SharedDir = dst
			out = append(out, share)
		case share.RootfsTar != "":
			tarPath := filepath.Join(snapDir, share.RootfsTar)
			if err := unpackRootfsTar(tarPath, dst); err != nil {
				_ = unmountChildRootfs(vmDir)
				return nil, fmt.Errorf("extract legacy rootfs tar %q: %w", share.RootfsTar, err)
			}
			if err := recreateAnnouncedSubmounts(dst); err != nil {
				_ = unmountChildRootfs(vmDir)
				return nil, err
			}
			share.SharedDir = dst
			out = append(out, share)
		default:
			src, err := findLiveParentRootfs(share, live)
			if err != nil {
				_ = unmountChildRootfs(vmDir)
				return nil, err
			}
			if err := bindMountRootfs(src, dst); err != nil {
				_ = unmountChildRootfs(vmDir)
				return nil, fmt.Errorf("bind parent sharedDir %q -> %q: %w", src, dst, err)
			}
			share.SharedDir = dst
			out = append(out, share)
		}
	}
	return out, nil
}

func (k *Kata) materializeKataSharedRootfs(ctx context.Context, shareDir string, plan kataRootfsPlan, share virtiofsShare, snapDir string) error {
	if err := os.MkdirAll(shareDir, 0o755); err != nil {
		return err
	}
	rootfsDir, err := reconstructShareFromImage(ctx, shareDir, plan.containerID, plan.appImage)
	if err != nil {
		return err
	}
	if share.UpperTar != "" {
		if err := mergeUpperFromTar(filepath.Join(snapDir, share.UpperTar), rootfsDir); err != nil {
			return fmt.Errorf("merge rootfs upper: %w", err)
		}
	}
	// Bind+RO (not overlay-at-cid/rootfs): virtiofsd+guest exec after CH restore
	// can return EBADF when the announced submount is the overlay mount itself.
	if err := remountBindReadOnly(rootfsDir); err != nil {
		return err
	}
	return recreateAnnouncedSubmounts(shareDir)
}

// reconstructShareFromImage builds an OCI overlay under .image-bundle, then
// bind-mounts it to shareDir/<cid>/rootfs (find-paths path). Guest RAM carries
// process state; the bind keeps virtiofs submounts stable across restore.
func reconstructShareFromImage(ctx context.Context, shareDir, containerID, imageRef string) (string, error) {
	if containerID == "" || imageRef == "" {
		return "", fmt.Errorf("container id and image ref are required")
	}
	bundle := filepath.Join(shareDir, ".image-bundle")
	if err := setupImageRootfs(ctx, imageRef, bundle); err != nil {
		return "", fmt.Errorf("setup image rootfs %q: %w", imageRef, err)
	}
	dst := filepath.Join(shareDir, containerID, "rootfs")
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return "", err
	}
	src := filepath.Join(bundle, "rootfs")
	if err := bindMountRootfs(src, dst); err != nil {
		return "", fmt.Errorf("bind image rootfs -> %q: %w", dst, err)
	}
	for _, d := range []string{"proc", "sys", "dev"} {
		_ = os.MkdirAll(filepath.Join(dst, d), 0o755)
	}
	return dst, nil
}

func remountBindReadOnly(target string) error {
	return unix.Mount("", target, "", unix.MS_REMOUNT|unix.MS_BIND|unix.MS_RDONLY, "")
}

// recreateAnnouncedSubmounts self-binds */rootfs mountpoints for find-paths restore.
func recreateAnnouncedSubmounts(shareRoot string) error {
	for _, rel := range discoverRootfsRelPaths(shareRoot) {
		abs := filepath.Join(shareRoot, rel)
		if !dirExists(abs) {
			continue
		}
		for _, d := range []string{"proc", "sys", "dev"} {
			_ = os.MkdirAll(filepath.Join(abs, d), 0o755)
		}
		if err := unix.Mount(abs, abs, "", unix.MS_BIND, ""); err != nil {
			return fmt.Errorf("self-bind submount %q: %w", rel, err)
		}
	}
	for _, d := range []string{"proc", "sys", "dev"} {
		_ = os.MkdirAll(filepath.Join(shareRoot, d), 0o755)
	}
	return nil
}

func (k *Kata) findParentRootfsShares(sourceSandboxID string) []virtiofsShare {
	if sourceSandboxID == "" {
		return nil
	}
	if name, ok := StripPrefix(sourceSandboxID, kataIDPrefix); ok {
		return discoverVirtiofsShares(filepath.Join(k.StateDir, name))
	}
	for _, root := range []string{
		filepath.Join("/run/vc/vm", sourceSandboxID),
		filepath.Join("/run/vc/sbs", sourceSandboxID),
	} {
		if shares := discoverVirtiofsShares(root); len(shares) > 0 {
			return shares
		}
	}
	return fallbackKataSharedDir(sourceSandboxID)
}

func bindMountRootfs(src, dst string) error {
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return err
	}
	return unix.Mount(src, dst, "", unix.MS_BIND|unix.MS_REC, "")
}

func unmountChildRootfs(vmDir string) error {
	root := filepath.Join(vmDir, "virtiofs")
	targets := mountPointsUnder(root)
	sort.Slice(targets, func(i, j int) bool { return len(targets[i]) > len(targets[j]) })
	for _, t := range targets {
		_ = unix.Unmount(t, unix.MNT_DETACH)
	}
	return nil
}

func mountPointsUnder(root string) []string {
	root = filepath.Clean(root)
	if root == "" {
		return nil
	}
	prefix := root + string(os.PathSeparator)
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 5 {
			continue
		}
		mp := fields[4]
		if mp == root || strings.HasPrefix(mp, prefix) {
			out = append(out, mp)
		}
	}
	return out
}
