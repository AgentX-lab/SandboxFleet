//go:build linux

package overlay

import (
	"fmt"
	"log"
	"os"
	"strings"

	"golang.org/x/sys/unix"
)

// EnsureSharedPropagation makes path a mount point with rshared propagation
// (self-bind + MS_SHARED|MS_REC), so host bind mounts under the virtiofs shared
// dir are visible to the guest. Without this, a container rootfs (runc default
// rprivate) silently drops propagation: the guest sees an empty rootfs and
// kata-agent CreateContainer fails with ENOENT — same as substrate
// ateom-microvm ensureSharedPropagation.
//
// Idempotent: skips if path is already a shared mount point.
func EnsureSharedPropagation(path string) error {
	if path == "" {
		return fmt.Errorf("EnsureSharedPropagation: empty path")
	}
	if err := os.MkdirAll(path, 0o750); err != nil {
		return fmt.Errorf("creating %q: %w", path, err)
	}
	if alreadySharedMount(path) {
		log.Printf("kata mount: %s already shared", path)
		return nil
	}
	if err := unix.Mount(path, path, "", unix.MS_BIND, ""); err != nil {
		return fmt.Errorf("self-binding %q: %w", path, err)
	}
	if err := unix.Mount("", path, "", unix.MS_SHARED|unix.MS_REC, ""); err != nil {
		return fmt.Errorf("marking %q rshared: %w", path, err)
	}
	log.Printf("kata mount: made %s rshared for virtio-fs propagation", path)
	return nil
}

func alreadySharedMount(path string) bool {
	b, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(b), "\n") {
		fields := strings.Fields(line)
		// mountinfo: ... root mountpoint opts [optional...] - fstype ...
		if len(fields) >= 7 && fields[4] == path && strings.Contains(line, "shared:") {
			return true
		}
	}
	return false
}
