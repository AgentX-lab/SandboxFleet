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

// TestSandboxPool covers Pool basics and scale behavior as one suite.
func TestSandboxPool(t *testing.T) {
	t.Run("topologyAndProfiles", testTopologyAndProfiles)
	t.Run("workerScaleUp", testWorkerScaleUp)
	t.Run("workerScaleDownSuccess", testWorkerScaleDownSuccess)
	t.Run("workerScaleDownBlocked", testWorkerScaleDownBlocked)
	t.Run("slotScaleUpSuccess", testSlotScaleUpSuccess)
	t.Run("slotScaleUpBlocked", testSlotScaleUpBlocked)
	t.Run("slotScaleDownSuccess", testSlotScaleDownSuccess)
	t.Run("slotScaleDownBlocked", testSlotScaleDownBlocked)
}

func testTopologyAndProfiles(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()

	tc, ns := newPoolCase(t, ctx, "pool_heterogeneous.yaml", "hetero-pool")
	tc.WaitAppliedSlots(ctx, ns, "hetero-pool", "default", 3)

	applied := tc.GetPool(ctx, ns, "hetero-pool").Status.Templates
	profiles := map[string]int{}
	for _, template := range applied {
		if template.Name != "default" {
			continue
		}
		for _, slot := range template.AppliedSlots {
			profiles[slot.Profile]++
		}
	}
	if profiles["small"] != 2 || profiles["large"] != 1 {
		t.Fatalf("appliedSlots profiles = %#v, want small=2 large=1", profiles)
	}

	small := createReadySandbox(t, ctx, tc, ns, "hetero-pool", "small-sandbox", "small")
	defer deleteSandbox(t, ctx, tc, small)
	large := createReadySandbox(t, ctx, tc, ns, "hetero-pool", "large-sandbox", "large")
	defer deleteSandbox(t, ctx, tc, large)

	assignSmall := small.Object().Status.Assignment
	assignLarge := large.Object().Status.Assignment
	if assignSmall.SlotProfile != "small" || assignLarge.SlotProfile != "large" {
		t.Fatalf("assignments = small:%#v large:%#v", assignSmall, assignLarge)
	}
	if assignSmall.Worker != assignLarge.Worker {
		t.Fatalf("expected same Worker, got %q and %q", assignSmall.Worker, assignLarge.Worker)
	}
}

func testWorkerScaleUp(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	tc, ns := newPoolCase(t, ctx, "pool_single_slot.yaml", "worker-up")
	if got := tc.GetPool(ctx, ns, "worker-up").Status.ReadyWorkers; got != 1 {
		t.Fatalf("ReadyWorkers = %d, want 1", got)
	}

	tc.UpdatePool(ctx, ns, "worker-up", func(pool *sandboxv1alpha1.SandboxPool) {
		pool.Spec.WorkerTemplates[0].Replicas = 2
	})
	tc.WaitReadyWorkers(ctx, ns, "worker-up", 2)
	tc.WaitPoolCondition(ctx, ns, "worker-up", sandboxv1alpha1.ConditionUpdating, metav1.ConditionFalse, nil)
}

func testWorkerScaleDownSuccess(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	tc, ns := newPoolCase(t, ctx, "pool_two_workers.yaml", "worker-down-ok")
	tc.WaitReadyWorkers(ctx, ns, "worker-down-ok", 2)

	tc.UpdatePool(ctx, ns, "worker-down-ok", func(pool *sandboxv1alpha1.SandboxPool) {
		pool.Spec.WorkerTemplates[0].Replicas = 1
	})
	tc.WaitReadyWorkers(ctx, ns, "worker-down-ok", 1)
	tc.WaitPoolCondition(ctx, ns, "worker-down-ok", sandboxv1alpha1.ConditionUpdating, metav1.ConditionFalse, func(pool *sandboxv1alpha1.SandboxPool) bool {
		return pool.Status.ReadyWorkers == 1
	})
}

func testWorkerScaleDownBlocked(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()

	tc, ns := newPoolCase(t, ctx, "pool_two_workers.yaml", "worker-down-busy")
	tc.WaitReadyWorkers(ctx, ns, "worker-down-busy", 2)

	// One Slot per Worker: two Running Sandboxes occupy both Workers.
	a := createReadySandbox(t, ctx, tc, ns, "worker-down-busy", "busy-a", "small")
	defer deleteSandbox(t, ctx, tc, a)
	b := createReadySandbox(t, ctx, tc, ns, "worker-down-busy", "busy-b", "small")
	defer deleteSandbox(t, ctx, tc, b)

	tc.UpdatePool(ctx, ns, "worker-down-busy", func(pool *sandboxv1alpha1.SandboxPool) {
		pool.Spec.WorkerTemplates[0].Replicas = 1
	})
	tc.WaitPoolCondition(ctx, ns, "worker-down-busy", sandboxv1alpha1.ConditionUpdating, metav1.ConditionTrue, nil)
	if got := tc.GetPool(ctx, ns, "worker-down-busy").Status.ReadyWorkers; got < 2 {
		t.Fatalf("ReadyWorkers = %d, want still 2 while scale-down is blocked", got)
	}
}

func testSlotScaleUpSuccess(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	// Start with 2 Slots so the Worker Pod envelope covers 2.
	tc, ns := newPoolCase(t, ctx, "pool_basic.yaml", "slot-up-ok")
	tc.WaitAppliedSlots(ctx, ns, "slot-up-ok", "default", 2)

	tc.UpdatePool(ctx, ns, "slot-up-ok", func(pool *sandboxv1alpha1.SandboxPool) {
		pool.Spec.WorkerTemplates[0].Slots[0].Count = 1
	})
	tc.WaitAppliedSlots(ctx, ns, "slot-up-ok", "default", 1)

	// Scale back up: existing Pod still has headroom for 2 Slots.
	tc.UpdatePool(ctx, ns, "slot-up-ok", func(pool *sandboxv1alpha1.SandboxPool) {
		pool.Spec.WorkerTemplates[0].Slots[0].Count = 2
	})
	tc.WaitAppliedSlots(ctx, ns, "slot-up-ok", "default", 2)
	tc.WaitPoolCondition(ctx, ns, "slot-up-ok", sandboxv1alpha1.ConditionUpdating, metav1.ConditionFalse, nil)
}

func testSlotScaleUpBlocked(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	tc, ns := newPoolCase(t, ctx, "pool_single_slot.yaml", "slot-up-blocked")
	tc.WaitAppliedSlots(ctx, ns, "slot-up-blocked", "default", 1)

	tc.UpdatePool(ctx, ns, "slot-up-blocked", func(pool *sandboxv1alpha1.SandboxPool) {
		pool.Spec.WorkerTemplates[0].Slots[0].Count = 2
	})
	tc.WaitPoolCondition(ctx, ns, "slot-up-blocked", sandboxv1alpha1.ConditionUpdating, metav1.ConditionTrue, nil)
	tc.WaitAppliedSlots(ctx, ns, "slot-up-blocked", "default", 1)
}

func testSlotScaleDownSuccess(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	tc, ns := newPoolCase(t, ctx, "pool_basic.yaml", "slot-down-ok")
	tc.WaitAppliedSlots(ctx, ns, "slot-down-ok", "default", 2)

	tc.UpdatePool(ctx, ns, "slot-down-ok", func(pool *sandboxv1alpha1.SandboxPool) {
		pool.Spec.WorkerTemplates[0].Slots[0].Count = 1
	})
	tc.WaitAppliedSlots(ctx, ns, "slot-down-ok", "default", 1)
	tc.WaitPoolCondition(ctx, ns, "slot-down-ok", sandboxv1alpha1.ConditionUpdating, metav1.ConditionFalse, nil)
}

func testSlotScaleDownBlocked(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()

	tc, ns := newPoolCase(t, ctx, "pool_basic.yaml", "slot-down-busy")
	tc.WaitAppliedSlots(ctx, ns, "slot-down-busy", "default", 2)

	// Occupy both Slots, then free the lower ID so only the high ID stays busy.
	first := createReadySandbox(t, ctx, tc, ns, "slot-down-busy", "slot-a", "small")
	second := createReadySandbox(t, ctx, tc, ns, "slot-down-busy", "slot-b", "small")
	keepBusy, freeFirst := second, first
	if first.Object().Status.Assignment.SlotID > second.Object().Status.Assignment.SlotID {
		keepBusy, freeFirst = first, second
	}
	deleteSandbox(t, ctx, tc, freeFirst)
	defer deleteSandbox(t, ctx, tc, keepBusy)

	tc.UpdatePool(ctx, ns, "slot-down-busy", func(pool *sandboxv1alpha1.SandboxPool) {
		pool.Spec.WorkerTemplates[0].Slots[0].Count = 1
	})
	tc.WaitPoolCondition(ctx, ns, "slot-down-busy", sandboxv1alpha1.ConditionUpdating, metav1.ConditionTrue, nil)
	if got := len(tc.WaitAppliedSlots(ctx, ns, "slot-down-busy", "default", 2)); got != 2 {
		t.Fatalf("appliedSlots = %d, want 2 while busy high-ID Slot blocks scale-down", got)
	}
}

func newPoolCase(t *testing.T, ctx context.Context, manifest, poolName string) (*framework.Context, string) {
	t.Helper()
	tc := framework.New(t)
	ns := fmt.Sprintf("sandboxfleet-e2e-%s-%d", poolName, time.Now().UnixNano())
	tc.CreateNamespace(ctx, ns)
	tc.CreatePoolFrom(ctx, manifest, ns, poolName)
	tc.WaitPoolReady(ctx, ns, poolName)
	return tc, ns
}

func createReadySandbox(
	t *testing.T,
	ctx context.Context,
	tc *framework.Context,
	namespace, poolName, name, profile string,
) *sandboxfleet.Sandbox {
	t.Helper()
	_, err := tc.SDK.CreateSandbox(ctx, sandboxfleet.CreateOptions{
		Namespace:   namespace,
		Name:        name,
		PoolRef:     poolName,
		SlotProfile: profile,
		Container: sandboxv1alpha1.ContainerSpec{
			Image:   "registry.k8s.io/e2e-test-images/busybox:1.36.1-1",
			Command: []string{"sleep", "3600"},
		},
	})
	if err != nil {
		t.Fatalf("CreateSandbox %s: %v", name, err)
	}
	session, err := tc.SDK.OpenSandboxReady(ctx, namespace, name)
	if err != nil {
		t.Fatalf("OpenSandboxReady %s: %v", name, err)
	}
	return session
}

func deleteSandbox(t *testing.T, ctx context.Context, tc *framework.Context, session *sandboxfleet.Sandbox) {
	t.Helper()
	if session == nil {
		return
	}
	session.Close()
	if err := tc.SDK.DeleteSandbox(ctx, session.Namespace(), session.Name()); err != nil {
		t.Fatalf("DeleteSandbox %s: %v", session.Name(), err)
	}
	if err := tc.SDK.WaitSandboxDeleted(ctx, session.Namespace(), session.Name()); err != nil {
		t.Fatalf("WaitSandboxDeleted %s: %v", session.Name(), err)
	}
}
