//go:build linux

package snapshotter

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

const cgroupMount = "/sys/fs/cgroup"

// SetupCgroupDelegation prepares cgroups so runsc IsOnlyV2() sees a real
// cgroup2 hierarchy and can nest restore leaves (substrate-aligned).
//
// - Private cgroup ns: delegate at /sys/fs/cgroup (same as substrate ateom).
// - Privileged / host cgroup ns: do NOT carve up the host root; delegate under
//   this process's current cgroup, and point restore cgroupsPath there.
func SetupCgroupDelegation() error {
	if err := ensureCgroupV2Mount(); err != nil {
		return err
	}

	private, err := inPrivateCgroupNamespace()
	if err != nil {
		return fmt.Errorf("detect cgroup namespace: %w", err)
	}

	var base string
	if private {
		base = cgroupMount
		log.Printf("cgroup: private namespace; delegating at %s", base)
	} else {
		rel, err := currentCgroupV2RelPath()
		if err != nil {
			return err
		}
		base = filepath.Join(cgroupMount, strings.TrimPrefix(rel, "/"))
		log.Printf("cgroup: host namespace; delegating under %s", base)
	}

	leaf := filepath.Join(base, "sandboxfleet")
	if err := os.Mkdir(leaf, 0o755); err != nil && !os.IsExist(err) {
		if err := unix.Mount("none", base, "", unix.MS_BIND|unix.MS_REMOUNT, ""); err != nil {
			// base may already be writable; still try mkdir once more below
			log.Printf("cgroup: remount %s rw: %v", base, err)
		}
		if err := os.Mkdir(leaf, 0o755); err != nil && !os.IsExist(err) {
			return fmt.Errorf("mkdir %s: %w", leaf, err)
		}
	}
	if err := moveCgroupProcs(filepath.Join(base, "cgroup.procs"), filepath.Join(leaf, "cgroup.procs")); err != nil {
		return fmt.Errorf("move procs into %s: %w", leaf, err)
	}
	avail, err := os.ReadFile(filepath.Join(base, "cgroup.controllers"))
	if err != nil {
		return fmt.Errorf("read cgroup.controllers: %w", err)
	}
	var enabled []string
	for _, c := range strings.Fields(string(avail)) {
		if err := os.WriteFile(filepath.Join(base, "cgroup.subtree_control"), []byte("+"+c), 0o644); err != nil {
			log.Printf("cgroup: enable %s: %v", c, err)
			continue
		}
		enabled = append(enabled, c)
	}
	log.Printf("cgroup: delegation ready base=%s controllers=%v", base, enabled)
	return nil
}

// restoreCgroupsPath is the OCI linux.cgroupsPath for a restored runsc child.
// Colon-free so runsc uses cgroupfs (v2), not systemd.
func restoreCgroupsPath(sandboxName string) string {
	private, err := inPrivateCgroupNamespace()
	if err == nil && private {
		// Same as substrate: absolute path under the ns cgroup root.
		return "/" + sandboxName
	}
	rel, err := currentCgroupV2RelPath()
	if err != nil || rel == "" || rel == "/" {
		return "/" + sandboxName
	}
	// Nest under the Worker pod cgroup (sibling of the sandboxfleet init leaf).
	// Prefer parent of .../sandboxfleet when we already moved into that leaf.
	rel = strings.TrimSuffix(rel, "/sandboxfleet")
	return filepath.Join(rel, sandboxName)
}

func ensureRestoreCgroup(sandboxName string) error {
	path := restoreCgroupsPath(sandboxName)
	dir := filepath.Join(cgroupMount, strings.TrimPrefix(path, "/"))
	return os.MkdirAll(dir, 0o755)
}

// ensureCgroupV2Mount makes /sys/fs/cgroup a cgroup2 filesystem in this mount
// namespace so runsc IsOnlyV2() is true (avoids probing v1 .../memory).
func ensureCgroupV2Mount() error {
	if isCgroup2FS(cgroupMount) {
		return nil
	}
	// Hybrid layouts: tmpfs at /sys/fs/cgroup with controllers under unified/.
	unified := filepath.Join(cgroupMount, "unified")
	if _, err := os.Stat(filepath.Join(unified, "cgroup.controllers")); err == nil {
		log.Printf("cgroup: binding %s onto %s for pure cgroup2 view", unified, cgroupMount)
		if err := unix.Mount(unified, cgroupMount, "", unix.MS_BIND|unix.MS_REC, ""); err != nil {
			return fmt.Errorf("bind %s -> %s: %w", unified, cgroupMount, err)
		}
		if isCgroup2FS(cgroupMount) {
			return nil
		}
	}
	if _, err := os.Stat(filepath.Join(cgroupMount, "cgroup.controllers")); err == nil {
		// Controllers file present but Statfs is not cgroup2 — unusual; continue
		// and let delegation surface a clearer error.
		log.Printf("cgroup: %s has cgroup.controllers but Statfs type is not cgroup2", cgroupMount)
		return nil
	}
	return fmt.Errorf("%s is not a cgroup2 mount (runsc IsOnlyV2 would be false)", cgroupMount)
}

func isCgroup2FS(path string) bool {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return false
	}
	return st.Type == unix.CGROUP2_SUPER_MAGIC
}

func inPrivateCgroupNamespace() (bool, error) {
	b, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return false, err
	}
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if path, ok := strings.CutPrefix(line, "0::"); ok {
			return path == "/", nil
		}
	}
	return false, fmt.Errorf("no cgroup v2 entry in /proc/self/cgroup")
}

func currentCgroupV2RelPath() (string, error) {
	b, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if path, ok := strings.CutPrefix(line, "0::"); ok {
			if path == "" {
				return "/", nil
			}
			return path, nil
		}
	}
	return "", fmt.Errorf("no cgroup v2 entry in /proc/self/cgroup")
}

func moveCgroupProcs(src, dst string) error {
	for range 100 {
		b, err := os.ReadFile(src)
		if err != nil {
			return err
		}
		pids := strings.Fields(string(b))
		if len(pids) == 0 {
			return nil
		}
		for _, pid := range pids {
			_ = os.WriteFile(dst, []byte(pid), 0o644)
		}
	}
	return fmt.Errorf("%s did not drain", src)
}
