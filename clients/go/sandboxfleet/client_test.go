package sandboxfleet

import (
	"context"
	"errors"
	"net"
	"net/http/httptest"
	"strconv"
	"testing"

	sandboxv1alpha1 "github.com/AgentNaut/SandboxFleet/api/v1alpha1"
	"github.com/AgentNaut/SandboxFleet/internal/runtime"
	"github.com/AgentNaut/SandboxFleet/internal/worker"
	"github.com/AgentNaut/SandboxFleet/internal/worker/httpapi"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestCreateAndWaitSandboxReady(t *testing.T) {
	ctx := context.Background()
	client := newTestClient(t)
	created, err := client.CreateSandbox(ctx, CreateOptions{
		Namespace: "default",
		Name:      "sandbox",
		PoolRef:   "pool",
		Container: sandboxv1alpha1.ContainerSpec{Image: "busybox"},
	})
	if err != nil {
		t.Fatalf("CreateSandbox() error = %v", err)
	}

	created.Status.Phase = sandboxv1alpha1.SandboxPhaseRunning
	created.Status.Conditions = []metav1.Condition{{
		Type:   sandboxv1alpha1.ConditionReady,
		Status: metav1.ConditionTrue,
		Reason: "Running",
	}}
	sdk := client.(*sdkClient)
	if err := sdk.kubernetes.Status().Update(ctx, created); err != nil {
		t.Fatalf("update Sandbox status: %v", err)
	}

	ready, err := client.WaitSandboxReady(ctx, "default", "sandbox")
	if err != nil {
		t.Fatalf("WaitSandboxReady() error = %v", err)
	}
	if ready.Status.Phase != sandboxv1alpha1.SandboxPhaseRunning {
		t.Fatalf("WaitSandboxReady().Phase = %q, want Running", ready.Status.Phase)
	}
}

func TestWaitSandboxReadyReturnsFailedError(t *testing.T) {
	ctx := context.Background()
	client := newTestClient(t)
	created, err := client.CreateSandbox(ctx, CreateOptions{
		Namespace: "default",
		Name:      "sandbox",
		PoolRef:   "pool",
		Container: sandboxv1alpha1.ContainerSpec{Image: "busybox"},
	})
	if err != nil {
		t.Fatalf("CreateSandbox() error = %v", err)
	}
	created.Status.Phase = sandboxv1alpha1.SandboxPhaseFailed
	sdk := client.(*sdkClient)
	if err := sdk.kubernetes.Status().Update(ctx, created); err != nil {
		t.Fatalf("update Sandbox status: %v", err)
	}

	_, err = client.WaitSandboxReady(ctx, "default", "sandbox")
	var failed *SandboxFailedError
	if !errors.As(err, &failed) {
		t.Fatalf("WaitSandboxReady() error = %v, want SandboxFailedError", err)
	}
}

func TestDeleteSandboxIsIdempotent(t *testing.T) {
	ctx := context.Background()
	client := newTestClient(t)
	if err := client.DeleteSandbox(ctx, "default", "missing"); err != nil {
		t.Fatalf("DeleteSandbox() error = %v", err)
	}
}

func TestExecSandboxCallsAssignedWorker(t *testing.T) {
	ctx := context.Background()
	manager := worker.NewSlotManager(worker.Config{Slots: 1}, &sdkTestRuntime{})
	workerServer := httptest.NewServer(httpapi.NewServer(manager))
	defer workerServer.Close()

	host, portText, err := net.SplitHostPort(workerServer.Listener.Addr().String())
	if err != nil {
		t.Fatalf("SplitHostPort() error = %v", err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatalf("Atoi(%q) error = %v", portText, err)
	}

	scheme := k8sruntime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme() error = %v", err)
	}
	if err := sandboxv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme() error = %v", err)
	}

	workerPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "demo-worker-0", Namespace: "default"},
		Status:     corev1.PodStatus{PodIP: host},
	}
	kubernetesClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&sandboxv1alpha1.Sandbox{}).
		WithObjects(workerPod).
		Build()

	sdk := &sdkClient{
		kubernetes:   kubernetesClient,
		worker:       httpapi.NewClient(workerServer.Client()),
		workerPort:   int32(port),
		pollInterval: defaultPollInterval,
	}

	identity := worker.SandboxIdentity{Namespace: "default", Name: "sandbox", UID: "uid-1"}
	if err := manager.ReserveSlot(ctx, worker.SandboxSlotRef{SlotID: 0, Identity: identity}); err != nil {
		t.Fatalf("ReserveSlot() error = %v", err)
	}
	if err := manager.StartSandbox(ctx, worker.StartSandboxRequest{
		SlotID:    0,
		Identity:  identity,
		Container: sandboxv1alpha1.ContainerSpec{Image: "busybox"},
	}); err != nil {
		t.Fatalf("StartSandbox() error = %v", err)
	}

	sandbox := &sandboxv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "sandbox", Namespace: "default", UID: "uid-1"},
		Spec: sandboxv1alpha1.SandboxSpec{
			PoolRef:   "demo",
			Container: sandboxv1alpha1.ContainerSpec{Image: "busybox"},
		},
	}
	if err := kubernetesClient.Create(ctx, sandbox); err != nil {
		t.Fatalf("Create sandbox: %v", err)
	}
	sandbox.Status.Phase = sandboxv1alpha1.SandboxPhaseRunning
	sandbox.Status.Assignment = &sandboxv1alpha1.Assignment{Worker: "demo-worker-0", SlotID: 0}
	if err := kubernetesClient.Status().Update(ctx, sandbox); err != nil {
		t.Fatalf("update sandbox status: %v", err)
	}

	result, err := sdk.ExecSandbox(ctx, "default", "sandbox", ExecOptions{Command: []string{"echo", "hi"}})
	if err != nil {
		t.Fatalf("ExecSandbox() error = %v", err)
	}
	if result.ExitCode != 0 || result.Stdout != "echo" {
		t.Fatalf("ExecSandbox() = %#v, want exit 0 stdout echo", result)
	}
}

func newTestClient(t *testing.T) Client {
	t.Helper()
	scheme := k8sruntime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme() error = %v", err)
	}
	if err := sandboxv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme() error = %v", err)
	}
	kubernetesClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&sandboxv1alpha1.Sandbox{}).
		Build()
	return NewWithClient(kubernetesClient)
}

type sdkTestRuntime struct {
	id runtime.ID
}

func (r *sdkTestRuntime) Create(context.Context, runtime.CreateRequest) (runtime.ID, error) {
	r.id = runtime.ID{Value: "runtime-1"}
	return r.id, nil
}
func (*sdkTestRuntime) Start(context.Context, runtime.ID) error  { return nil }
func (*sdkTestRuntime) Stop(context.Context, runtime.ID) error   { return nil }
func (*sdkTestRuntime) Delete(context.Context, runtime.ID) error { return nil }
func (*sdkTestRuntime) Status(context.Context, runtime.ID) (runtime.Status, error) {
	return runtime.Status{State: runtime.StateRunning}, nil
}
func (*sdkTestRuntime) List(context.Context) ([]runtime.Info, error) { return nil, nil }
func (*sdkTestRuntime) Exec(_ context.Context, _ runtime.ID, req runtime.ExecRequest) (runtime.ExecResult, error) {
	stdout := ""
	if len(req.Command) > 0 {
		stdout = req.Command[0]
	}
	return runtime.ExecResult{ExitCode: 0, Stdout: stdout}, nil
}
