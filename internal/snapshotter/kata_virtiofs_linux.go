//go:build linux

package snapshotter

import (
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// prepareChildRootfsDirs makes each child's rootfs directory under vmDir.
// Prefer unpacking RootfsTar (cross-Worker); else bind the live parent rootfs (same node).
func (k *Kata) prepareChildRootfsDirs(planned []virtiofsShare, sourceSandboxID, vmDir, snapDir string) ([]virtiofsShare, error) {
	live := k.findParentRootfsShares(sourceSandboxID)
	out := make([]virtiofsShare, 0, len(planned))
	for i, share := range planned {
		dst := childRootfsDir(vmDir, i)
		if share.RootfsTar != "" {
			tarPath := filepath.Join(snapDir, share.RootfsTar)
			if err := unpackRootfsTar(tarPath, dst); err != nil {
				_ = unmountChildRootfs(vmDir)
				return nil, fmt.Errorf("extract rootfs tar %q: %w", share.RootfsTar, err)
			}
			// find-paths may reopen /proc,/sys,/dev placeholders under the share root.
			for _, d := range []string{"proc", "sys", "dev"} {
				_ = os.MkdirAll(filepath.Join(dst, d), 0o755)
			}
			share.SharedDir = dst
			out = append(out, share)
			continue
		}
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
	return out, nil
}

// findParentRootfsShares finds the parent sandbox's rootfs share dirs on this Worker.
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
	if err := unix.Mount(src, dst, "", unix.MS_BIND|unix.MS_REC, ""); err != nil {
		return err
	}
	return nil
}

// unmountChildRootfs detaches child rootfs bind mounts under vmDir.
func unmountChildRootfs(vmDir string) error {
	root := filepath.Join(vmDir, "virtiofs")
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	for _, e := range entries {
		_ = unix.Unmount(filepath.Join(root, e.Name()), unix.MNT_DETACH)
	}
	return nil
}
