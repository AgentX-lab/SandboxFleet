package controller

import (
	"context"
	"testing"

	sandboxv1alpha1 "github.com/AgentNaut/SandboxFleet/api/v1alpha1"
	"github.com/AgentNaut/SandboxFleet/internal/worker"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestSnapshotReconcileCreatesReadySnapshot(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	mustAddScheme(t, corev1.AddToScheme(scheme))
	mustAddScheme(t, sandboxv1alpha1.AddToScheme(scheme))

	pool := &sandboxv1alpha1.SandboxPool{
		ObjectMeta: metav1.ObjectMeta{Namespace: "test", Name: "pool"},
		Spec: sandboxv1alpha1.SandboxPoolSpec{
			Runtime: sandboxv1alpha1.RuntimeConfig{
				Backend: sandboxv1alpha1.RuntimeBackendCRI,
				CRI:     &sandboxv1alpha1.CRIRuntimeConfig{RuntimeHandler: "runsc", Snapshotter: sandboxv1alpha1.SnapshotterGVisor},
			},
			SnapshotStorage: &sandboxv1alpha1.SnapshotStorageSpec{
				S3: &sandboxv1alpha1.S3SnapshotStorage{
					Bucket:               "snaps",
					Endpoint:             "http://minio:9000",
					CredentialsSecretRef: corev1.LocalObjectReference{Name: "creds"},
				},
			},
			SlotProfiles:    []sandboxv1alpha1.SlotProfile{{Name: "default"}},
			WorkerTemplates: []sandboxv1alpha1.WorkerTemplate{{Name: "default", Replicas: 1}},
		},
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: "test", Name: "creds"},
		Data: map[string][]byte{
			"accessKeyID":     []byte("ak"),
			"secretAccessKey": []byte("sk"),
		},
	}
	parent := &sandboxv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Namespace: "test", Name: "parent", UID: "parent-uid"},
		Spec: sandboxv1alpha1.SandboxSpec{
			PoolRef:     "pool",
			SlotProfile: "default",
			Container:   &sandboxv1alpha1.ContainerSpec{Image: "busybox"},
		},
		Status: sandboxv1alpha1.SandboxStatus{
			Phase: sandboxv1alpha1.SandboxPhaseRunning,
			Assignment: &sandboxv1alpha1.Assignment{
				Worker: "worker-0", SlotID: 0, SlotProfile: "default",
			},
		},
	}
	snap := &sandboxv1alpha1.SandboxSnapshot{
		ObjectMeta: metav1.ObjectMeta{Namespace: "test", Name: "snap-1"},
		Spec: sandboxv1alpha1.SandboxSnapshotSpec{
			SourceSandbox: "parent",
			Pool:          "pool",
		},
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "test", Name: "worker-0"},
		Status:     corev1.PodStatus{PodIP: "10.0.0.1"},
	}

	kubernetesClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&sandboxv1alpha1.SandboxSnapshot{}).
		WithObjects(pool, secret, parent, snap, pod).
		Build()

	workerClient := &snapshotRecordingWorker{}
	reconciler := &SnapshotReconciler{
		Client:           kubernetesClient,
		Scheme:           scheme,
		WorkerClient:     workerClient,
		EndpointResolver: PodIPResolver{Port: 8090},
	}
	request := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "test", Name: "snap-1"}}

	for range 3 {
		if _, err := reconciler.Reconcile(ctx, request); err != nil {
			t.Fatalf("Reconcile() error = %v", err)
		}
	}

	var current sandboxv1alpha1.SandboxSnapshot
	if err := kubernetesClient.Get(ctx, request.NamespacedName, &current); err != nil {
		t.Fatalf("get snapshot: %v", err)
	}
	if current.Status.Phase != sandboxv1alpha1.SandboxSnapshotPhaseReady {
		t.Fatalf("phase = %q, want Ready (message=%q)", current.Status.Phase, current.Status.Message)
	}
	if workerClient.createCalls != 1 {
		t.Fatalf("CreateSnapshot calls = %d, want 1", workerClient.createCalls)
	}
	if current.Status.StoragePath == "" || len(current.Status.SnapshotFiles) == 0 {
		t.Fatalf("status incomplete: %#v", current.Status)
	}
}

type snapshotRecordingWorker struct {
	recordingWorkerClient
	createCalls int
}

func (c *snapshotRecordingWorker) CreateSnapshot(_ context.Context, _ string, req worker.CreateSnapshotRequest) (worker.CreateSnapshotResult, error) {
	c.createCalls++
	return worker.CreateSnapshotResult{
		StoragePath:   req.StoragePath,
		SnapshotFiles: []string{"checkpoint.img"},
		SizeBytes:     42,
		FormatVersion: "runsc-checkpoint-v1",
		Runtime:       "runsc",
	}, nil
}
