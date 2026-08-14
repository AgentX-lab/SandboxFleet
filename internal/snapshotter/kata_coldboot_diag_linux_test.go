//go:build linux

package snapshotter

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/AgentNaut/SandboxFleet/internal/runtime/kata/overlay"
)

func TestGuestCarrierSharedDebugCmd(t *testing.T) {
	t.Parallel()
	const cid = "e2e-sandbox-a"
	cmd := guestCarrierSharedDebugCmd(cid, []string{"sleep", "3600"}, "/")
	root := overlay.GuestSharedRootfs(cid)
	for _, want := range []string{
		root,
		root + "/bin",
		root + "/bin/sleep",
		"argv0 probes",
		"ls root+argv0",
		"ls root+bin+base",
		"ls root+cwd",
		"read bin/sleep",
		"agent bundle paths",
		"findmnt -R",
		"mount 2>&1",
		"'sleep'",
	} {
		if !strings.Contains(cmd, want) {
			t.Fatalf("guestCarrierSharedDebugCmd missing %q\ncmd=%s", want, cmd)
		}
	}
}

func TestCarrierArgv0HostPath(t *testing.T) {
	t.Parallel()
	root := "/tmp/rootfs"
	cases := []struct {
		argv0 string
		want  string
	}{
		{argv0: "sleep", want: filepath.Join(root, "sleep")},
		{argv0: "/bin/sleep", want: filepath.Join(root, "bin", "sleep")},
		{argv0: "", want: filepath.Join(root, "(missing-argv0)")},
	}
	for _, tc := range cases {
		var args []string
		if tc.argv0 != "" {
			args = []string{tc.argv0}
		}
		if got := carrierArgv0HostPath(root, args); got != tc.want {
			t.Fatalf("carrierArgv0HostPath(%q) = %q, want %q", tc.argv0, got, tc.want)
		}
	}
}

func TestShellSingleQuote(t *testing.T) {
	t.Parallel()
	if got := shellSingleQuote("sleep"); got != "'sleep'" {
		t.Fatalf("got %q", got)
	}
	if got := shellSingleQuote("a'b"); got != `'a'\''b'` {
		t.Fatalf("got %q", got)
	}
}
