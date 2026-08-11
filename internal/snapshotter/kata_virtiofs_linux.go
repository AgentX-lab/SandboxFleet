//go:build linux

package snapshotter

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"golang.org/x/sys/unix"
)

// prepareChildRootfsDirs materializes each virtiofs share under vmDir for restore.
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
			if err := recreateAnnouncedSubmounts(dst); err != nil {
				_ = unmountChildRootfs(vmDir)
				return nil, err
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

// recreateAnnouncedSubmounts self-binds */rootfs mountpoints so virtiofsd
// --announce-submounts matches the guest layout expected by find-paths restore.
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
