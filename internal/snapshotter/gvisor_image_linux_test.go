//go:build linux

package snapshotter

import (
	"testing"

	"github.com/containerd/containerd/mount"
)

func TestSplitOverlayLowerdirs(t *testing.T) {
	t.Parallel()
	got := splitOverlayLowerdirs(`/a/b:/c/d`)
	if len(got) != 2 || got[0] != "/a/b" || got[1] != "/c/d" {
		t.Fatalf("got %#v", got)
	}
	got = splitOverlayLowerdirs(`/path/with\:colon:/other`)
	if len(got) != 2 || got[0] != `/path/with:colon` || got[1] != "/other" {
		t.Fatalf("escaped got %#v", got)
	}
}

func TestLowerDirsFromMountsOverlay(t *testing.T) {
	t.Parallel()
	got, err := lowerDirsFromMounts([]mount.Mount{{
		Type:    "overlay",
		Options: []string{"workdir=/w", "lowerdir=/top/fs:/bottom/fs", "upperdir=/u"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != "/top/fs" || got[1] != "/bottom/fs" {
		t.Fatalf("got %#v", got)
	}
}

func TestLowerDirsFromMountsBind(t *testing.T) {
	t.Parallel()
	got, err := lowerDirsFromMounts([]mount.Mount{
		{Type: "bind", Source: "/snap/2/fs"},
		{Type: "bind", Source: "/snap/1/fs"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != "/snap/2/fs" {
		t.Fatalf("got %#v", got)
	}
}
