//go:build linux

package snapshotter

import (
	"fmt"
	"log"
	"os"
	"strings"

	"golang.org/x/sys/unix"
)

// SetupCgroupDelegation prepares the Worker cgroup so runsc can nest container
// leaves (substrate setupCgroupDelegation). Privileged Workers inherit the host
// cgroup namespace and skip — runsc then uses absolute cgroupsPath under the
// host hierarchy.
func SetupCgroupDelegation() error {
	const root = "/sys/fs/cgroup"
	const leaf = root + "/sandboxfleet"

	private, err := inPrivateCgroupNamespace()
	if err != nil {
		return err
	}
	if !private {
		log.Printf("cgroup: not in private namespace; skip delegation (privileged Worker)")
		return nil
	}

	if err := os.Mkdir(leaf, 0o755); err != nil && !os.IsExist(err) {
		if err := unix.Mount("none", root, "", unix.MS_BIND|unix.MS_REMOUNT, ""); err != nil {
			return fmt.Errorf("remount %s rw: %w", root, err)
		}
		if err := os.Mkdir(leaf, 0o755); err != nil && !os.IsExist(err) {
			return fmt.Errorf("mkdir %s: %w", leaf, err)
		}
	}
	if err := moveCgroupProcs(root+"/cgroup.procs", leaf+"/cgroup.procs"); err != nil {
		return err
	}
	avail, err := os.ReadFile(root + "/cgroup.controllers")
	if err != nil {
		return fmt.Errorf("read cgroup.controllers: %w", err)
	}
	for _, c := range strings.Fields(string(avail)) {
		if err := os.WriteFile(root+"/cgroup.subtree_control", []byte("+"+c), 0o644); err != nil {
			log.Printf("cgroup: enable %s: %v", c, err)
		}
	}
	log.Printf("cgroup: delegation ready under %s", root)
	return nil
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
