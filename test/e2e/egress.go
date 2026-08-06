//go:build e2e

package e2e

import (
	"context"
	"strings"
	"testing"
	"time"

	sandboxfleet "github.com/AgentNaut/SandboxFleet/clients/go/sandboxfleet"
)

// guestEgressURL is a plain-HTTP connectivity probe (no TLS required in busybox wget).
const guestEgressURL = "http://www.gstatic.com/generate_204"

// assertGuestEgress requires the sandbox guest to reach the public internet.
func assertGuestEgress(t *testing.T, ctx context.Context, session *sandboxfleet.Sandbox) {
	t.Helper()
	result, err := session.Exec(ctx, sandboxfleet.ExecOptions{
		Command: []string{"wget", "-q", "-T", "20", "-O", "/dev/null", guestEgressURL},
		Timeout: 45 * time.Second,
	})
	if err != nil {
		t.Fatalf("egress Exec %s: %v", session.Name(), err)
	}
	if result.ExitCode != 0 {
		t.Fatalf("egress Exec %s exit=%d stderr=%q stdout=%q",
			session.Name(), result.ExitCode, strings.TrimSpace(result.Stderr), strings.TrimSpace(result.Stdout))
	}
	t.Logf("egress ok for %s", session.Name())
}
