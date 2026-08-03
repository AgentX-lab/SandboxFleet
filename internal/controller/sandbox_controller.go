package controller

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"time"

	sandboxv1alpha1 "github.com/AgentNaut/SandboxFleet/api/v1alpha1"
	"github.com/AgentNaut/SandboxFleet/internal/scheduler"
	"github.com/AgentNaut/SandboxFleet/internal/worker"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

const sandboxRetryInterval = 2 * time.Second

type SandboxReconciler struct {
	client.Client
	Scheme           *runtime.Scheme
	Scheduler        scheduler.Scheduler
	WorkerClient     WorkerClient
	EndpointResolver WorkerEndpointResolver
}

func (r *SandboxReconciler) Reconcile(ctx context.Context, request ctrl.Request) (ctrl.Result, error) {
	var sandbox sandboxv1alpha1.Sandbox
	if err := r.Get(ctx, request.NamespacedName, &sandbox); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !sandbox.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, &sandbox)
	}
	if !controllerutil.ContainsFinalizer(&sandbox, sandboxv1alpha1.SandboxFinalizer) {
		controllerutil.AddFinalizer(&sandbox, sandboxv1alpha1.SandboxFinalizer)
		if err := r.Update(ctx, &sandbox); err != nil {
			return ctrl.Result{}, fmt.Errorf("add Sandbox finalizer: %w", err)
		}
		return ctrl.Result{}, nil
	}

	var pool sandboxv1alpha1.SandboxPool
	if err := r.Get(ctx, types.NamespacedName{Namespace: sandbox.Namespace, Name: sandbox.Spec.PoolRef}, &pool); err != nil {
		return ctrl.Result{}, fmt.Errorf("get SandboxPool %q: %w", sandbox.Spec.PoolRef, err)
	}
	if !conditionTrue(pool.Status.Conditions, sandboxv1alpha1.ConditionReady) {
		setSandboxCondition(&sandbox, sandboxv1alpha1.ConditionScheduled, metav1.ConditionFalse, "PoolNotReady", "the SandboxPool has no ready Worker")
		sandbox.Status.Phase = sandboxv1alpha1.SandboxPhasePending
		if err := r.updateStatus(ctx, &sandbox); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: sandboxRetryInterval}, nil
	}

	if sandbox.Status.Assignment == nil {
		assignment, err := r.Scheduler.Assign(scheduler.AssignRequest{
			SandboxUID: sandbox.UID,
			Namespace:  sandbox.Namespace,
			Name:       sandbox.Name,
			Pool:       sandbox.Spec.PoolRef,
		})
		if errors.Is(err, scheduler.ErrNoCapacity) {
			setSandboxCondition(&sandbox, sandboxv1alpha1.ConditionScheduled, metav1.ConditionFalse, "NoCapacity", "the SandboxPool has no available Slot")
			sandbox.Status.Phase = sandboxv1alpha1.SandboxPhasePending
			if statusErr := r.updateStatus(ctx, &sandbox); statusErr != nil {
				return ctrl.Result{}, statusErr
			}
			return ctrl.Result{RequeueAfter: sandboxRetryInterval}, nil
		}
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("assign Sandbox: %w", err)
		}

		sandbox.Status.Assignment = &sandboxv1alpha1.Assignment{
			Worker: assignment.Worker.Name,
			SlotID: assignment.SlotID,
		}
		sandbox.Status.Phase = sandboxv1alpha1.SandboxPhaseStarting
		setSandboxCondition(&sandbox, sandboxv1alpha1.ConditionScheduled, metav1.ConditionTrue, "Assigned", "a Worker and Slot were assigned")
		setSandboxCondition(&sandbox, sandboxv1alpha1.ConditionReady, metav1.ConditionFalse, "Starting", "the Sandbox is starting")
		if err := r.updateStatus(ctx, &sandbox); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	if err := r.Scheduler.Restore(r.schedulerAssignment(&sandbox)); err != nil {
		return ctrl.Result{}, fmt.Errorf("restore Sandbox assignment: %w", err)
	}
	endpoint, err := r.workerEndpoint(ctx, sandbox.Namespace, sandbox.Status.Assignment.Worker)
	if err != nil {
		setSandboxCondition(&sandbox, sandboxv1alpha1.ConditionReady, metav1.ConditionFalse, "WorkerUnavailable", err.Error())
		if statusErr := r.updateStatus(ctx, &sandbox); statusErr != nil {
			return ctrl.Result{}, statusErr
		}
		return ctrl.Result{RequeueAfter: sandboxRetryInterval}, nil
	}

	identity := sandboxIdentity(&sandbox)
	ref := worker.SandboxSlotRef{SlotID: sandbox.Status.Assignment.SlotID, Identity: identity}
	if err := r.WorkerClient.ReserveSlot(ctx, endpoint, ref); err != nil {
		return r.workerOperationFailed(ctx, &sandbox, "ReserveFailed", err)
	}
	if err := r.WorkerClient.StartSandbox(ctx, endpoint, worker.StartSandboxRequest{
		SlotID:    ref.SlotID,
		Identity:  identity,
		Container: sandbox.Spec.Container,
	}); err != nil {
		return r.workerOperationFailed(ctx, &sandbox, "StartFailed", err)
	}

	sandbox.Status.Phase = sandboxv1alpha1.SandboxPhaseRunning
	setSandboxCondition(&sandbox, sandboxv1alpha1.ConditionReady, metav1.ConditionTrue, "Running", "the Sandbox is running")
	if err := r.updateStatus(ctx, &sandbox); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: poolPollInterval}, nil
}

func (r *SandboxReconciler) SetupWithManager(manager ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(manager).
		For(&sandboxv1alpha1.Sandbox{}).
		Complete(r)
}

func (r *SandboxReconciler) reconcileDelete(ctx context.Context, sandbox *sandboxv1alpha1.Sandbox) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(sandbox, sandboxv1alpha1.SandboxFinalizer) {
		return ctrl.Result{}, nil
	}
	if sandbox.Status.Assignment != nil {
		sandbox.Status.Phase = sandboxv1alpha1.SandboxPhaseStopping
		if err := r.updateStatus(ctx, sandbox); err != nil {
			return ctrl.Result{}, err
		}

		endpoint, err := r.workerEndpoint(ctx, sandbox.Namespace, sandbox.Status.Assignment.Worker)
		if err != nil {
			return ctrl.Result{RequeueAfter: sandboxRetryInterval}, nil
		}
		ref := worker.SandboxSlotRef{
			SlotID:   sandbox.Status.Assignment.SlotID,
			Identity: sandboxIdentity(sandbox),
		}
		if err := r.WorkerClient.StopSandbox(ctx, endpoint, ref); err != nil {
			return ctrl.Result{}, fmt.Errorf("stop Sandbox: %w", err)
		}
		if err := r.WorkerClient.ReleaseSlot(ctx, endpoint, ref); err != nil {
			return ctrl.Result{}, fmt.Errorf("release Sandbox Slot: %w", err)
		}
		if err := r.Scheduler.Release(sandbox.UID); err != nil {
			return ctrl.Result{}, fmt.Errorf("release Scheduler assignment: %w", err)
		}
	} else if err := r.Scheduler.Release(sandbox.UID); err != nil {
		return ctrl.Result{}, fmt.Errorf("release pending Scheduler assignment: %w", err)
	}

	controllerutil.RemoveFinalizer(sandbox, sandboxv1alpha1.SandboxFinalizer)
	if err := r.Update(ctx, sandbox); err != nil {
		return ctrl.Result{}, fmt.Errorf("remove Sandbox finalizer: %w", err)
	}
	return ctrl.Result{}, nil
}

func (r *SandboxReconciler) schedulerAssignment(sandbox *sandboxv1alpha1.Sandbox) scheduler.Assignment {
	return scheduler.Assignment{
		SandboxUID: sandbox.UID,
		Namespace:  sandbox.Namespace,
		Name:       sandbox.Name,
		Worker: scheduler.WorkerKey{
			Namespace: sandbox.Namespace,
			Pool:      sandbox.Spec.PoolRef,
			Name:      sandbox.Status.Assignment.Worker,
		},
		SlotID: sandbox.Status.Assignment.SlotID,
	}
}

func (r *SandboxReconciler) workerEndpoint(ctx context.Context, namespace, name string) (string, error) {
	var pod corev1.Pod
	if err := r.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, &pod); err != nil {
		return "", fmt.Errorf("get Worker Pod %q: %w", name, err)
	}
	return r.EndpointResolver.Endpoint(&pod)
}

func (r *SandboxReconciler) workerOperationFailed(
	ctx context.Context,
	sandbox *sandboxv1alpha1.Sandbox,
	reason string,
	operationErr error,
) (ctrl.Result, error) {
	setSandboxCondition(sandbox, sandboxv1alpha1.ConditionReady, metav1.ConditionFalse, reason, operationErr.Error())
	if retryable, ok := operationErr.(interface{ Retryable() bool }); ok && !retryable.Retryable() {
		sandbox.Status.Phase = sandboxv1alpha1.SandboxPhaseFailed
		if err := r.updateStatus(ctx, sandbox); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}
	if err := r.updateStatus(ctx, sandbox); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: sandboxRetryInterval}, nil
}

func (r *SandboxReconciler) updateStatus(ctx context.Context, sandbox *sandboxv1alpha1.Sandbox) error {
	sandbox.Status.ObservedGeneration = sandbox.Generation
	desired := *sandbox.Status.DeepCopy()

	var current sandboxv1alpha1.Sandbox
	if err := r.Get(ctx, types.NamespacedName{Namespace: sandbox.Namespace, Name: sandbox.Name}, &current); err != nil {
		return fmt.Errorf("get current Sandbox before status update: %w", err)
	}
	if reflect.DeepEqual(current.Status, desired) {
		sandbox.ObjectMeta = current.ObjectMeta
		sandbox.Status = current.Status
		return nil
	}
	current.Status = desired
	if err := r.Status().Update(ctx, &current); err != nil {
		return fmt.Errorf("update Sandbox status: %w", err)
	}
	sandbox.ObjectMeta = current.ObjectMeta
	sandbox.Status = current.Status
	return nil
}

func setSandboxCondition(sandbox *sandboxv1alpha1.Sandbox, conditionType string, status metav1.ConditionStatus, reason, message string) {
	meta.SetStatusCondition(&sandbox.Status.Conditions, metav1.Condition{
		Type:               conditionType,
		Status:             status,
		ObservedGeneration: sandbox.Generation,
		Reason:             reason,
		Message:            message,
	})
}

func conditionTrue(conditions []metav1.Condition, conditionType string) bool {
	condition := meta.FindStatusCondition(conditions, conditionType)
	return condition != nil && condition.Status == metav1.ConditionTrue
}

func sandboxIdentity(sandbox *sandboxv1alpha1.Sandbox) worker.SandboxIdentity {
	return worker.SandboxIdentity{Namespace: sandbox.Namespace, Name: sandbox.Name, UID: sandbox.UID}
}
