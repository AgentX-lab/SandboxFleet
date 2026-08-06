package controller

import (
	"context"
	"testing"

	sandboxv1alpha1 "github.com/AgentNaut/SandboxFleet/api/v1alpha1"
	"github.com/AgentNaut/SandboxFleet/internal/scheduler"
	"github.com/AgentNaut/SandboxFleet/internal/slot"
	"github.com/AgentNaut/SandboxFleet/internal/worker"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestPoolReconcileCreatesTemplateWorkloads(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	mustAddScheme(t, corev1.AddToScheme(scheme))
	mustAddScheme(t, appsv1.AddToScheme(scheme))
	mustAddScheme(t, sandboxv1alpha1.AddToScheme(scheme))

	pool := heterogenousPool("pool")
	kubernetesClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&sandboxv1alpha1.SandboxPool{}).
		WithObjects(pool).
		Build()

	workerClient := &poolTestWorkerClient{}
	reconciler := &PoolReconciler{
		Client:           kubernetesClient,
		Scheme:           scheme,
		Scheduler:        scheduler.New(scheduler.StableStrategy{}),
		WorkerClient:     workerClient,
		EndpointResolver: PodIPResolver{Port: 8090},
		WorkloadBuilder:  StatefulSetBuilder{DefaultImage: "worker:test", Port: 8090},
		WorkerPort:       8090,
	}
	if _, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "test", Name: "pool"}}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	var sts appsv1.StatefulSet
	if err := kubernetesClient.Get(ctx, types.NamespacedName{Namespace: "test", Name: "pool-mixed-worker"}, &sts); err != nil {
		t.Fatalf("get StatefulSet: %v", err)
	}
	if sts.Spec.Replicas == nil || *sts.Spec.Replicas != 1 {
		t.Fatalf("replicas = %v, want 1", sts.Spec.Replicas)
	}
	// No Ready Workers yet: push is a no-op, but Status still records desired topology.
	if len(workerClient.applied) != 0 {
		t.Fatalf("applied topology = %d, want 0 before Workers are Ready", len(workerClient.applied))
	}
	var updatedPool sandboxv1alpha1.SandboxPool
	if err := kubernetesClient.Get(ctx, types.NamespacedName{Namespace: "test", Name: "pool"}, &updatedPool); err != nil {
		t.Fatalf("get pool: %v", err)
	}
	if got := len(appliedSlotsFor(updatedPool, "mixed")); got != 3 {
		t.Fatalf("status appliedSlots = %d, want 3", got)
	}
}

func TestPoolReconcileBlocksSlotScaleUpWithoutEnvelope(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	mustAddScheme(t, corev1.AddToScheme(scheme))
	mustAddScheme(t, appsv1.AddToScheme(scheme))
	mustAddScheme(t, sandboxv1alpha1.AddToScheme(scheme))

	pool := heterogenousPool("pool")
	initial := []slot.Config{
		{ID: 0, Profile: "small", Resources: smallResources()},
		{ID: 1, Profile: "large", Resources: largeResources()},
	}
	pool.Status.Templates = []sandboxv1alpha1.WorkerTemplateStatus{{
		Name: "mixed",
		AppliedSlots: []sandboxv1alpha1.AppliedSlot{
			{ID: 0, Profile: "small"},
			{ID: 1, Profile: "large"},
		},
	}}
	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: "test", Name: "pool-mixed-worker"},
		Spec: appsv1.StatefulSetSpec{
			Replicas: ptr.To(int32(1)),
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{Containers: []corev1.Container{{
					Name:      "worker",
					Resources: slot.SumResources(initial),
				}}},
			},
		},
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "test",
			Name:      "pool-mixed-worker-0",
			Labels:    workerLabels("pool", "mixed"),
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Name:      "worker",
			Resources: slot.SumResources(initial),
		}}},
		Status: corev1.PodStatus{
			PodIP: "10.0.0.1",
			Conditions: []corev1.PodCondition{{
				Type:   corev1.PodReady,
				Status: corev1.ConditionTrue,
			}},
		},
	}
	pool.Spec.WorkerTemplates[0].Slots = []sandboxv1alpha1.SlotGroup{
		{Profile: "small", Count: 2},
		{Profile: "large", Count: 1},
	}

	kubernetesClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&sandboxv1alpha1.SandboxPool{}).
		WithObjects(pool, sts, pod).
		Build()

	workerClient := &poolTestWorkerClient{
		slots: []slot.Info{
			{ID: 0, Profile: "small", State: slot.StateFree},
			{ID: 1, Profile: "large", State: slot.StateFree},
		},
	}
	reconciler := &PoolReconciler{
		Client:           kubernetesClient,
		Scheme:           scheme,
		Scheduler:        scheduler.New(scheduler.StableStrategy{}),
		WorkerClient:     workerClient,
		EndpointResolver: PodIPResolver{Port: 8090},
		WorkloadBuilder:  StatefulSetBuilder{DefaultImage: "worker:test", Port: 8090},
		WorkerPort:       8090,
	}
	if _, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "test", Name: "pool"}}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	if len(workerClient.applied) != 2 {
		t.Fatalf("applied specs = %d, want 2 (scale-up blocked)", len(workerClient.applied))
	}

	var updatedPool sandboxv1alpha1.SandboxPool
	if err := kubernetesClient.Get(ctx, types.NamespacedName{Namespace: "test", Name: "pool"}, &updatedPool); err != nil {
		t.Fatalf("get pool: %v", err)
	}
	if got := len(appliedSlotsFor(updatedPool, "mixed")); got != 2 {
		t.Fatalf("status appliedSlots = %d, want 2", got)
	}
	updating := meta.FindStatusCondition(updatedPool.Status.Conditions, sandboxv1alpha1.ConditionUpdating)
	if updating == nil || updating.Status != metav1.ConditionTrue {
		t.Fatalf("Updating condition = %#v, want True", updating)
	}
}

func TestPoolReconcilePushesTopologyToReadyWorkers(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	mustAddScheme(t, corev1.AddToScheme(scheme))
	mustAddScheme(t, appsv1.AddToScheme(scheme))
	mustAddScheme(t, sandboxv1alpha1.AddToScheme(scheme))

	pool := heterogenousPool("pool")
	desiredResources := slot.SumResources([]slot.Config{
		{ID: 0, Profile: "small", Resources: smallResources()},
		{ID: 1, Profile: "small", Resources: smallResources()},
		{ID: 2, Profile: "large", Resources: largeResources()},
	})
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "test",
			Name:      "pool-mixed-worker-0",
			Labels:    workerLabels("pool", "mixed"),
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Name:      "worker",
			Resources: desiredResources,
		}}},
		Status: corev1.PodStatus{
			PodIP: "10.0.0.1",
			Conditions: []corev1.PodCondition{{
				Type:   corev1.PodReady,
				Status: corev1.ConditionTrue,
			}},
		},
	}
	kubernetesClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&sandboxv1alpha1.SandboxPool{}).
		WithObjects(pool, pod).
		Build()

	workerClient := &poolTestWorkerClient{}
	reconciler := &PoolReconciler{
		Client:           kubernetesClient,
		Scheme:           scheme,
		Scheduler:        scheduler.New(scheduler.StableStrategy{}),
		WorkerClient:     workerClient,
		EndpointResolver: PodIPResolver{Port: 8090},
		WorkloadBuilder:  StatefulSetBuilder{DefaultImage: "worker:test", Port: 8090},
		WorkerPort:       8090,
	}
	if _, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "test", Name: "pool"}}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(workerClient.applied) != 3 {
		t.Fatalf("applied specs = %d, want 3", len(workerClient.applied))
	}
	var updatedPool sandboxv1alpha1.SandboxPool
	if err := kubernetesClient.Get(ctx, types.NamespacedName{Namespace: "test", Name: "pool"}, &updatedPool); err != nil {
		t.Fatalf("get pool: %v", err)
	}
	if got := len(appliedSlotsFor(updatedPool, "mixed")); got != 3 {
		t.Fatalf("status appliedSlots = %d, want 3", got)
	}
}

func appliedSlotsFor(pool sandboxv1alpha1.SandboxPool, templateName string) []sandboxv1alpha1.AppliedSlot {
	for _, template := range pool.Status.Templates {
		if template.Name == templateName {
			return template.AppliedSlots
		}
	}
	return nil
}

func heterogenousPool(name string) *sandboxv1alpha1.SandboxPool {
	return &sandboxv1alpha1.SandboxPool{
		ObjectMeta: metav1.ObjectMeta{Namespace: "test", Name: name, Generation: 1},
		Spec: sandboxv1alpha1.SandboxPoolSpec{
			Runtime: sandboxv1alpha1.RuntimeConfig{
				Backend: sandboxv1alpha1.RuntimeBackendCRI,
				CRI:     &sandboxv1alpha1.CRIRuntimeConfig{RuntimeHandler: "runsc", Snapshotter: sandboxv1alpha1.SnapshotterGVisor},
			},
			SlotProfiles: []sandboxv1alpha1.SlotProfile{
				{Name: "small", Resources: smallResources()},
				{Name: "large", Resources: largeResources()},
			},
			WorkerTemplates: []sandboxv1alpha1.WorkerTemplate{{
				Name:     "mixed",
				Replicas: 1,
				Slots: []sandboxv1alpha1.SlotGroup{
					{Profile: "small", Count: 2},
					{Profile: "large", Count: 1},
				},
			}},
		},
	}
}

func smallResources() corev1.ResourceRequirements {
	return corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m"), corev1.ResourceMemory: resource.MustParse("128Mi")},
		Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m"), corev1.ResourceMemory: resource.MustParse("128Mi")},
	}
}

func largeResources() corev1.ResourceRequirements {
	return corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1"), corev1.ResourceMemory: resource.MustParse("1Gi")},
		Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1"), corev1.ResourceMemory: resource.MustParse("1Gi")},
	}
}

type poolTestWorkerClient struct {
	slots   []slot.Info
	applied []slot.Config
}

func (*poolTestWorkerClient) Health(context.Context, string) error { return nil }
func (c *poolTestWorkerClient) ListSlots(context.Context, string) ([]slot.Info, error) {
	if c.slots == nil {
		return nil, nil
	}
	return c.slots, nil
}
func (c *poolTestWorkerClient) ApplyTopology(_ context.Context, _ string, specs []slot.Config) error {
	c.applied = append([]slot.Config(nil), specs...)
	return nil
}
func (*poolTestWorkerClient) ReserveSlot(context.Context, string, worker.SandboxSlotRef) error {
	return nil
}
func (*poolTestWorkerClient) StartSandbox(context.Context, string, worker.StartSandboxRequest) error {
	return nil
}
func (*poolTestWorkerClient) StopSandbox(context.Context, string, worker.SandboxSlotRef) error {
	return nil
}
func (*poolTestWorkerClient) ReleaseSlot(context.Context, string, worker.SandboxSlotRef) error {
	return nil
}
func (*poolTestWorkerClient) CreateSnapshot(context.Context, string, worker.CreateSnapshotRequest) (worker.CreateSnapshotResult, error) {
	return worker.CreateSnapshotResult{}, nil
}
func (*poolTestWorkerClient) RestoreFromSnapshot(context.Context, string, worker.RestoreFromSnapshotRequest) error {
	return nil
}
func (*poolTestWorkerClient) DeleteSnapshotObjects(context.Context, string, worker.DeleteSnapshotObjectsRequest) error {
	return nil
}
