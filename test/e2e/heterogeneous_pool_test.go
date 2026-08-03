//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"testing"
	"time"

	sandboxv1alpha1 "github.com/AgentNaut/SandboxFleet/api/v1alpha1"
	sandboxfleet "github.com/AgentNaut/SandboxFleet/clients/go/sandboxfleet"
	"github.com/AgentNaut/SandboxFleet/test/e2e/framework"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestHeterogeneousPoolTopologyAndProfiles(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()

	tc := framework.New(t)
	ns := fmt.Sprintf("sandboxfleet-e2e-hetero-%d", time.Now().UnixNano())
	poolName := "hetero-pool"

	tc.CreateNamespace(ctx, ns)
	tc.CreatePoolFrom(ctx, "heterogeneous_pool.yaml", ns, poolName)
	tc.WaitPoolReady(ctx, ns, poolName)

	applied := tc.WaitAppliedSlots(ctx, ns, poolName, "mixed", 3)
	profiles := map[string]int{}
	for _, slot := range applied {
		profiles[slot.Profile]++
	}
	if profiles["small"] != 2 || profiles["large"] != 1 {
		t.Fatalf("appliedSlots profiles = %#v, want small=2 large=1", applied)
	}
	t.Logf("appliedSlots ok: %#v", applied)

	container := sandboxv1alpha1.ContainerSpec{
		Image:   "registry.k8s.io/e2e-test-images/busybox:1.36.1-1",
		Command: []string{"sleep", "3600"},
	}
	small, err := tc.SDK.CreateSandbox(ctx, sandboxfleet.CreateOptions{
		Namespace: ns, Name: "small-sandbox", PoolRef: poolName, SlotProfile: "small", Container: container,
	})
	if err != nil {
		t.Fatalf("CreateSandbox small: %v", err)
	}
	large, err := tc.SDK.CreateSandbox(ctx, sandboxfleet.CreateOptions{
		Namespace: ns, Name: "large-sandbox", PoolRef: poolName, SlotProfile: "large", Container: container,
	})
	if err != nil {
		t.Fatalf("CreateSandbox large: %v", err)
	}

	sessionSmall, err := tc.SDK.OpenSandboxReady(ctx, small.Namespace, small.Name)
	if err != nil {
		t.Fatalf("OpenSandboxReady small: %v", err)
	}
	defer sessionSmall.Close()
	sessionLarge, err := tc.SDK.OpenSandboxReady(ctx, large.Namespace, large.Name)
	if err != nil {
		t.Fatalf("OpenSandboxReady large: %v", err)
	}
	defer sessionLarge.Close()

	assignSmall := sessionSmall.Object().Status.Assignment
	assignLarge := sessionLarge.Object().Status.Assignment
	if assignSmall == nil || assignLarge == nil {
		t.Fatal("expected both sandboxes assigned")
	}
	if assignSmall.SlotProfile != "small" {
		t.Fatalf("small assignment profile = %q, want small", assignSmall.SlotProfile)
	}
	if assignLarge.SlotProfile != "large" {
		t.Fatalf("large assignment profile = %q, want large", assignLarge.SlotProfile)
	}
	if assignSmall.Worker != assignLarge.Worker {
		t.Fatalf("expected same Worker, got %q and %q", assignSmall.Worker, assignLarge.Worker)
	}
	t.Logf("profile scheduling ok: small slot=%d large slot=%d worker=%s",
		assignSmall.SlotID, assignLarge.SlotID, assignSmall.Worker)

	// Scale replicas 1 -> 2 and ensure a second Worker becomes Ready.
	tc.UpdatePool(ctx, ns, poolName, func(pool *sandboxv1alpha1.SandboxPool) {
		pool.Spec.WorkerTemplates[0].Replicas = 2
	})
	tc.WaitReadyWorkers(ctx, ns, poolName, 2)
	t.Logf("replicas scale-up ok: ReadyWorkers=%d", tc.GetPool(ctx, ns, poolName).Status.ReadyWorkers)

	sessionSmall.Close()
	sessionLarge.Close()
	for _, name := range []string{small.Name, large.Name} {
		if err := tc.SDK.DeleteSandbox(ctx, ns, name); err != nil {
			t.Fatalf("DeleteSandbox %s: %v", name, err)
		}
		if err := tc.SDK.WaitSandboxDeleted(ctx, ns, name); err != nil {
			t.Fatalf("WaitSandboxDeleted %s: %v", name, err)
		}
	}
}

func TestSlotScaleUpBlockedWithoutEnvelope(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	tc := framework.New(t)
	ns := fmt.Sprintf("sandboxfleet-e2e-tight-%d", time.Now().UnixNano())
	poolName := "tight-pool"

	tc.CreateNamespace(ctx, ns)
	tc.CreatePoolFrom(ctx, "tight_pool.yaml", ns, poolName)
	tc.WaitPoolReady(ctx, ns, poolName)
	tc.WaitAppliedSlots(ctx, ns, poolName, "tight", 1)

	tc.UpdatePool(ctx, ns, poolName, func(pool *sandboxv1alpha1.SandboxPool) {
		pool.Spec.WorkerTemplates[0].Slots[0].Count = 2
	})

	tc.WaitPoolCondition(ctx, ns, poolName, sandboxv1alpha1.ConditionUpdating, metav1.ConditionTrue, nil)
	applied := tc.WaitAppliedSlots(ctx, ns, poolName, "tight", 1)
	if len(applied) != 1 {
		t.Fatalf("appliedSlots = %#v, want 1 after blocked scale-up", applied)
	}
	t.Logf("blocked scale-up ok: Updating=True appliedSlots=%d", len(applied))
}
