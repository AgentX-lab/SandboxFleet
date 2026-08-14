//go:build linux

package snapshotter

import (
	"strings"
	"testing"

	"github.com/AgentNaut/SandboxFleet/internal/runtime/kata/overlay"
)

func TestGuestCarrierSharedDebugCmd(t *testing.T) {
	t.Parallel()
	const cid = "e2e-sandbox-a"
	cmd := guestCarrierSharedDebugCmd(cid)
	root := overlay.GuestSharedRootfs(cid)
	for _, want := range []string{
		root,
		root + "/bin",
		root + "/bin/sleep",
		"findmnt -R",
		"mount 2>&1",
	} {
		if !strings.Contains(cmd, want) {
			t.Fatalf("guestCarrierSharedDebugCmd missing %q\ncmd=%s", want, cmd)
		}
	}
}
