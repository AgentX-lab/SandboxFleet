package controller

import (
	"context"
	"fmt"
	"time"

	sandboxv1alpha1 "github.com/AgentNaut/SandboxFleet/api/v1alpha1"
	"github.com/AgentNaut/SandboxFleet/internal/worker"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

const snapshotRetryInterval = 2 * time.Second

// SnapshotReconciler creates object-storage snapshots from a Running parent Sandbox,
// and deletes them only when no child still references fromSnapshot.
type SnapshotReconciler struct {
	client.Client
	Scheme           *runtime.Scheme
	WorkerClient     WorkerClient
	EndpointResolver WorkerEndpointResolver
}

func (r *SnapshotReconciler) Reconcile(ctx context.Context, request ctrl.Request) (ctrl.Result, error) {
	var snap sandboxv1alpha1.SandboxSnapshot
	if err := r.Get(ctx, request.NamespacedName, &snap); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !snap.DeletionTimestamp.IsZero() {
		return r.syncDelete(ctx, &snap)
	}
	if !controllerutil.ContainsFinalizer(&snap, sandboxv1alpha1.SandboxSnapshotFinalizer) {
		controllerutil.AddFinalizer(&snap, sandboxv1alpha1.SandboxSnapshotFinalizer)
		if err := r.Update(ctx, &snap); err != nil {
			return ctrl.Result{}, fmt.Errorf("add SandboxSnapshot finalizer: %w", err)
		}
		return ctrl.Result{}, nil
	}

	switch snap.Status.Phase {
	case sandboxv1alpha1.SandboxSnapshotPhaseReady, sandboxv1alpha1.SandboxSnapshotPhaseFailed:
		return ctrl.Result{}, nil
	default:
		return r.syncCreate(ctx, &snap)
	}
}

func (r *SnapshotReconciler) SetupWithManager(manager ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(manager).
		For(&sandboxv1alpha1.SandboxSnapshot{}).
		Complete(r)
}

// syncCreate: parent Running → Worker CreateSnapshot → status Ready.
func (r *SnapshotReconciler) syncCreate(ctx context.Context, snap *sandboxv1alpha1.SandboxSnapshot) (ctrl.Result, error) {
	var parent sandboxv1alpha1.Sandbox
	if err := r.Get(ctx, types.NamespacedName{Namespace: snap.Namespace, Name: snap.Spec.SourceSandbox}, &parent); err != nil {
		if apierrors.IsNotFound(err) {
			return r.fail(ctx, snap, "ParentNotFound", err.Error())
		}
		return ctrl.Result{}, err
	}
	if parent.Status.Phase != sandboxv1alpha1.SandboxPhaseRunning || parent.Status.Assignment == nil {
		setSnapshotCondition(snap, sandboxv1alpha1.ConditionReady, metav1.ConditionFalse, "ParentNotReady", "source Sandbox is not Running")
		snap.Status.Phase = sandboxv1alpha1.SandboxSnapshotPhasePending
		if err := r.syncStatus(ctx, snap); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: snapshotRetryInterval}, nil
	}
	if parent.Spec.PoolRef != snap.Spec.Pool {
		return r.fail(ctx, snap, "PoolMismatch", fmt.Sprintf("parent poolRef %q != snapshot pool %q", parent.Spec.PoolRef, snap.Spec.Pool))
	}

	var pool sandboxv1alpha1.SandboxPool
	if err := r.Get(ctx, types.NamespacedName{Namespace: snap.Namespace, Name: snap.Spec.Pool}, &pool); err != nil {
		return ctrl.Result{}, fmt.Errorf("get SandboxPool: %w", err)
	}
	storage, err := snapshotStorageConfig(ctx, r.Client, &pool)
	if err != nil {
		return r.fail(ctx, snap, "StorageConfig", err.Error())
	}

	endpoint, err := r.resolveWorkerEndpoint(ctx, snap.Namespace, parent.Status.Assignment.Worker)
	if err != nil {
		setSnapshotCondition(snap, sandboxv1alpha1.ConditionReady, metav1.ConditionFalse, "WorkerUnavailable", err.Error())
		if statusErr := r.syncStatus(ctx, snap); statusErr != nil {
			return ctrl.Result{}, statusErr
		}
		return ctrl.Result{RequeueAfter: snapshotRetryInterval}, nil
	}

	storagePath := snap.Status.StoragePath
	if storagePath == "" {
		storagePath = fmt.Sprintf("snapshots/%s/%s/", parent.Name, snap.Name)
	}
	handler := poolRuntime(&pool)
	container := sandboxv1alpha1.ContainerSpec{}
	if parent.Spec.Container != nil {
		container = *parent.Spec.Container
	}

	result, err := r.WorkerClient.CreateSnapshot(ctx, endpoint, worker.CreateSnapshotRequest{
		SlotID:      parent.Status.Assignment.SlotID,
		Identity:    sandboxIdentity(&parent),
		StoragePath: storagePath,
		Storage:     storage,
		Runtime:     handler,
		Pool:        snap.Spec.Pool,
		Container:   container,
	})
	if err != nil {
		// Upload may have left orphans; ask Worker to wipe this path.
		_ = r.WorkerClient.DeleteSnapshotObjects(ctx, endpoint, worker.DeleteSnapshotObjectsRequest{
			StoragePath: storagePath,
			Storage:     storage,
		})
		return r.fail(ctx, snap, "CreateFailed", err.Error())
	}

	snap.Status.Phase = sandboxv1alpha1.SandboxSnapshotPhaseReady
	snap.Status.StoragePath = result.StoragePath
	snap.Status.SnapshotFiles = append([]string(nil), result.SnapshotFiles...)
	snap.Status.Runtime = result.Runtime
	snap.Status.FormatVersion = result.FormatVersion
	snap.Status.SizeBytes = result.SizeBytes
	snap.Status.SourceWorker = parent.Status.Assignment.Worker
	snap.Status.Message = ""
	setSnapshotCondition(snap, sandboxv1alpha1.ConditionReady, metav1.ConditionTrue, "Ready", "snapshot uploaded to object storage")
	if err := r.syncStatus(ctx, snap); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

// syncDelete refuses while any Sandbox still has fromSnapshot=<this>, else deletes objects.
func (r *SnapshotReconciler) syncDelete(ctx context.Context, snap *sandboxv1alpha1.SandboxSnapshot) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(snap, sandboxv1alpha1.SandboxSnapshotFinalizer) {
		return ctrl.Result{}, nil
	}

	var sandboxes sandboxv1alpha1.SandboxList
	if err := r.List(ctx, &sandboxes, client.InNamespace(snap.Namespace)); err != nil {
		return ctrl.Result{}, err
	}
	for _, sb := range sandboxes.Items {
		if sb.Spec.FromSnapshot == snap.Name {
			snap.Status.Message = fmt.Sprintf("still referenced by Sandbox %q", sb.Name)
			setSnapshotCondition(snap, sandboxv1alpha1.ConditionReady, metav1.ConditionFalse, "InUse", snap.Status.Message)
			_ = r.syncStatus(ctx, snap)
			return ctrl.Result{RequeueAfter: snapshotRetryInterval}, nil
		}
	}

	if snap.Status.StoragePath != "" {
		var pool sandboxv1alpha1.SandboxPool
		if err := r.Get(ctx, types.NamespacedName{Namespace: snap.Namespace, Name: snap.Spec.Pool}, &pool); err != nil {
			if !apierrors.IsNotFound(err) {
				return ctrl.Result{}, err
			}
		} else if storage, err := snapshotStorageConfig(ctx, r.Client, &pool); err == nil {
			endpoint, epErr := r.pickWorkerEndpoint(ctx, snap.Namespace, &pool, snap.Status.SourceWorker)
			if epErr == nil {
				_ = r.WorkerClient.DeleteSnapshotObjects(ctx, endpoint, worker.DeleteSnapshotObjectsRequest{
					StoragePath:   snap.Status.StoragePath,
					SnapshotFiles: snap.Status.SnapshotFiles,
					Storage:       storage,
				})
			}
		}
	}

	controllerutil.RemoveFinalizer(snap, sandboxv1alpha1.SandboxSnapshotFinalizer)
	if err := r.Update(ctx, snap); err != nil {
		return ctrl.Result{}, fmt.Errorf("remove SandboxSnapshot finalizer: %w", err)
	}
	return ctrl.Result{}, nil
}

func (r *SnapshotReconciler) pickWorkerEndpoint(ctx context.Context, namespace string, pool *sandboxv1alpha1.SandboxPool, preferredWorker string) (string, error) {
	if preferredWorker != "" {
		if endpoint, err := r.resolveWorkerEndpoint(ctx, namespace, preferredWorker); err == nil {
			return endpoint, nil
		}
	}
	var pods corev1.PodList
	if err := r.List(ctx, &pods, client.InNamespace(namespace), client.MatchingLabels{
		labelManaged: "true",
		labelPool:    pool.Name,
	}); err != nil {
		return "", err
	}
	for i := range pods.Items {
		endpoint, err := r.EndpointResolver.Endpoint(&pods.Items[i])
		if err == nil && endpoint != "" {
			return endpoint, nil
		}
	}
	return "", fmt.Errorf("no Worker available to delete snapshot objects")
}

func (r *SnapshotReconciler) resolveWorkerEndpoint(ctx context.Context, namespace, name string) (string, error) {
	var pod corev1.Pod
	if err := r.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, &pod); err != nil {
		return "", fmt.Errorf("get Worker Pod %q: %w", name, err)
	}
	return r.EndpointResolver.Endpoint(&pod)
}

func (r *SnapshotReconciler) fail(ctx context.Context, snap *sandboxv1alpha1.SandboxSnapshot, reason, message string) (ctrl.Result, error) {
	snap.Status.Phase = sandboxv1alpha1.SandboxSnapshotPhaseFailed
	snap.Status.Message = message
	setSnapshotCondition(snap, sandboxv1alpha1.ConditionReady, metav1.ConditionFalse, reason, message)
	if err := r.syncStatus(ctx, snap); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

func (r *SnapshotReconciler) syncStatus(ctx context.Context, snap *sandboxv1alpha1.SandboxSnapshot) error {
	snap.Status.ObservedGeneration = snap.Generation
	if err := r.Status().Update(ctx, snap); err != nil {
		return fmt.Errorf("update SandboxSnapshot status: %w", err)
	}
	return nil
}

func setSnapshotCondition(snap *sandboxv1alpha1.SandboxSnapshot, conditionType string, status metav1.ConditionStatus, reason, message string) {
	meta.SetStatusCondition(&snap.Status.Conditions, metav1.Condition{
		Type:               conditionType,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: snap.Generation,
		LastTransitionTime: metav1.Now(),
	})
}
