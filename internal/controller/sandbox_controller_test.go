package controller

import (
	"context"
	"testing"

	sandboxv1alpha1 "github.com/AgentNaut/SandboxFleet/api/v1alpha1"
	"github.com/AgentNaut/SandboxFleet/internal/scheduler"
	"github.com/AgentNaut/SandboxFleet/internal/slot"
	"github.com/AgentNaut/SandboxFleet/internal/worker"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestSandboxReconcileStartsAssignedSandbox(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	mustAddScheme(t, corev1.AddToScheme(scheme))
	mustAddScheme(t, sandboxv1alpha1.AddToScheme(scheme))

	pool := &sandboxv1alpha1.SandboxPool{
		ObjectMeta: metav1.ObjectMeta{Namespace: "test", Name: "pool"},
		Spec: sandboxv1alpha1.SandboxPoolSpec{
			Runtime: sandboxv1alpha1.RuntimeConfig{Backend: sandboxv1alpha1.RuntimeBackendCRI},
			SlotProfiles: []sandboxv1alpha1.SlotProfile{{
				Name: "default",
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m")},
				},
			}},
			WorkerTemplates: []sandboxv1alpha1.WorkerTemplate{{
				Name:     "default",
				Replicas: 1,
				Slots:    []sandboxv1alpha1.SlotGroup{{Profile: "default", Count: 1}},
			}},
		},
		Status: sandboxv1alpha1.SandboxPoolStatus{Conditions: []metav1.Condition{{
			Type:   sandboxv1alpha1.ConditionReady,
			Status: metav1.ConditionTrue,
			Reason: "WorkerAvailable",
		}}},
	}
	sandbox := &sandboxv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Namespace: "test", Name: "sandbox", UID: "sandbox-uid"},
		Spec: sandboxv1alpha1.SandboxSpec{
			PoolRef:     "pool",
			SlotProfile: "default",
			Container:   sandboxv1alpha1.ContainerSpec{Image: "busybox"},
		},
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "test", Name: "pool-default-worker-0"},
		Status:     corev1.PodStatus{PodIP: "10.0.0.1"},
	}
	kubernetesClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&sandboxv1alpha1.Sandbox{}).
		WithObjects(pool, sandbox, pod).
		Build()

	slotScheduler := scheduler.New(scheduler.StableStrategy{})
	slotScheduler.UpdateWorker(scheduler.WorkerState{
		Key:     scheduler.WorkerKey{Namespace: "test", Pool: "pool", Name: "pool-default-worker-0"},
		Healthy: true,
		Slots:   map[int32]slot.Info{0: {ID: 0, Profile: "default", State: slot.StateFree}},
	})
	workerClient := &recordingWorkerClient{}
	reconciler := &SandboxReconciler{
		Client:           kubernetesClient,
		Scheme:           scheme,
		Scheduler:        slotScheduler,
		WorkerClient:     workerClient,
		EndpointResolver: PodIPResolver{Port: 8090},
	}
	request := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "test", Name: "sandbox"}}

	for range 3 {
		if _, err := reconciler.Reconcile(ctx, request); err != nil {
			t.Fatalf("Reconcile() error = %v", err)
		}
	}

	var current sandboxv1alpha1.Sandbox
	if err := kubernetesClient.Get(ctx, request.NamespacedName, &current); err != nil {
		t.Fatalf("get Sandbox: %v", err)
	}
	if current.Status.Phase != sandboxv1alpha1.SandboxPhaseRunning {
		t.Fatalf("Sandbox phase = %q, want Running", current.Status.Phase)
	}
	if current.Status.Assignment == nil || current.Status.Assignment.Worker != "pool-default-worker-0" {
		t.Fatalf("Sandbox assignment = %#v, want pool-default-worker-0", current.Status.Assignment)
	}
	if current.Status.Assignment.SlotProfile != "default" {
		t.Fatalf("assignment slotProfile = %q, want default", current.Status.Assignment.SlotProfile)
	}
	if workerClient.reserveCalls != 1 || workerClient.startCalls != 1 {
		t.Fatalf("Worker calls = reserve %d, start %d; want 1 each", workerClient.reserveCalls, workerClient.startCalls)
	}
}

type recordingWorkerClient struct {
	reserveCalls int
	startCalls   int
}

func (*recordingWorkerClient) Health(context.Context, string) error { return nil }
func (*recordingWorkerClient) ListSlots(context.Context, string) ([]slot.Info, error) {
	return nil, nil
}
func (*recordingWorkerClient) ApplyTopology(context.Context, string, []slot.Config) error {
	return nil
}
func (c *recordingWorkerClient) ReserveSlot(context.Context, string, worker.SandboxSlotRef) error {
	c.reserveCalls++
	return nil
}
func (c *recordingWorkerClient) StartSandbox(context.Context, string, worker.StartSandboxRequest) error {
	c.startCalls++
	return nil
}
func (*recordingWorkerClient) StopSandbox(context.Context, string, worker.SandboxSlotRef) error {
	return nil
}
func (*recordingWorkerClient) ReleaseSlot(context.Context, string, worker.SandboxSlotRef) error {
	return nil
}

func mustAddScheme(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("add scheme: %v", err)
	}
}
