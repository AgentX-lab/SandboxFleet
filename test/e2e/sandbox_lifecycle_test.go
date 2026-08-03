//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	sandboxv1alpha1 "github.com/AgentNaut/SandboxFleet/api/v1alpha1"
	sandboxfleet "github.com/AgentNaut/SandboxFleet/clients/go/sandboxfleet"
	"github.com/AgentNaut/SandboxFleet/test/e2e/framework"
)

func TestSandboxLifecycleAndExec(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	tc := framework.New(t)
	ns := fmt.Sprintf("sandboxfleet-e2e-%d", time.Now().UnixNano())
	poolName := "e2e-pool"

	tc.CreateNamespace(ctx, ns)
	t.Logf("created namespace %s", ns)

	// Pool comes from test/e2e/testdata/sandboxpool.yaml (slotsPerWorker=2).
	tc.CreatePool(ctx, ns, poolName)
	t.Logf("created SandboxPool %s/%s", ns, poolName)

	tc.WaitPoolReady(ctx, ns, poolName)
	t.Logf("SandboxPool %s/%s is Ready", ns, poolName)

	container := sandboxv1alpha1.ContainerSpec{
		Image:   "registry.k8s.io/e2e-test-images/busybox:1.36.1-1",
		Command: []string{"sleep", "3600"},
	}

	a, err := tc.SDK.CreateSandbox(ctx, sandboxfleet.CreateOptions{
		Namespace: ns,
		Name:      "e2e-sandbox-a",
		PoolRef:   poolName,
		Container: container,
	})
	if err != nil {
		t.Fatalf("CreateSandbox a: %v", err)
	}
	t.Logf("created Sandbox %s/%s", a.Namespace, a.Name)

	b, err := tc.SDK.CreateSandbox(ctx, sandboxfleet.CreateOptions{
		Namespace: ns,
		Name:      "e2e-sandbox-b",
		PoolRef:   poolName,
		Container: container,
	})
	if err != nil {
		t.Fatalf("CreateSandbox b: %v", err)
	}
	t.Logf("created Sandbox %s/%s", b.Namespace, b.Name)

	readyA, err := tc.SDK.WaitSandboxReady(ctx, a.Namespace, a.Name)
	if err != nil {
		t.Fatalf("WaitSandboxReady a: %v", err)
	}
	readyB, err := tc.SDK.WaitSandboxReady(ctx, b.Namespace, b.Name)
	if err != nil {
		t.Fatalf("WaitSandboxReady b: %v", err)
	}
	if readyA.Status.Assignment == nil || readyB.Status.Assignment == nil {
		t.Fatal("expected both Sandboxes to be assigned")
	}

	assignA, assignB := readyA.Status.Assignment, readyB.Status.Assignment
	t.Logf("Sandbox a Running on worker=%s slot=%d", assignA.Worker, assignA.SlotID)
	t.Logf("Sandbox b Running on worker=%s slot=%d", assignB.Worker, assignB.SlotID)

	if assignA.Worker != assignB.Worker {
		t.Fatalf("expected both Sandboxes on the same Worker, got %q and %q", assignA.Worker, assignB.Worker)
	}
	if assignA.SlotID == assignB.SlotID {
		t.Fatalf("expected distinct SlotIDs on one Worker, both got %d", assignA.SlotID)
	}

	for _, sb := range []*sandboxv1alpha1.Sandbox{readyA, readyB} {
		want := fmt.Sprintf("hello-%s", sb.Name)
		result := tc.ExecSandbox(ctx, sb, []string{"echo", want})
		if result.ExitCode != 0 {
			t.Fatalf("ExecSandbox %s exit=%d stderr=%q", sb.Name, result.ExitCode, result.Stderr)
		}
		if !strings.Contains(result.Stdout, want) {
			t.Fatalf("ExecSandbox %s stdout=%q, want %q", sb.Name, result.Stdout, want)
		}
		t.Logf("ExecSandbox %s ok: stdout=%q", sb.Name, strings.TrimSpace(result.Stdout))
	}

	for _, sb := range []*sandboxv1alpha1.Sandbox{readyA, readyB} {
		if err := tc.SDK.DeleteSandbox(ctx, sb.Namespace, sb.Name); err != nil {
			t.Fatalf("DeleteSandbox %s: %v", sb.Name, err)
		}
		if err := tc.SDK.WaitSandboxDeleted(ctx, sb.Namespace, sb.Name); err != nil {
			t.Fatalf("WaitSandboxDeleted %s: %v", sb.Name, err)
		}
		t.Logf("Sandbox %s/%s deleted", sb.Namespace, sb.Name)
	}
}
