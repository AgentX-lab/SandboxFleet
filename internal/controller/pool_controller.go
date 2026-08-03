package controller

import (
	"context"
	"fmt"
	"reflect"
	"time"

	sandboxv1alpha1 "github.com/AgentNaut/SandboxFleet/api/v1alpha1"
	"github.com/AgentNaut/SandboxFleet/internal/scheduler"
	"github.com/AgentNaut/SandboxFleet/internal/slot"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const poolPollInterval = 5 * time.Second

type PoolReconciler struct {
	client.Client
	Scheme           *runtime.Scheme
	Scheduler        scheduler.Scheduler
	WorkerClient     WorkerClient
	EndpointResolver WorkerEndpointResolver
	WorkloadBuilder  WorkerWorkloadBuilder
	WorkerPort       int32
}

func (r *PoolReconciler) Reconcile(ctx context.Context, request ctrl.Request) (ctrl.Result, error) {
	var pool sandboxv1alpha1.SandboxPool
	if err := r.Get(ctx, request.NamespacedName, &pool); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if err := r.reconcileService(ctx, &pool); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.reconcileStatefulSet(ctx, &pool); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.refreshWorkers(ctx, &pool); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: poolPollInterval}, nil
}

func (r *PoolReconciler) SetupWithManager(manager ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(manager).
		For(&sandboxv1alpha1.SandboxPool{}).
		Owns(&appsv1.StatefulSet{}).
		Owns(&corev1.Service{}).
		Complete(r)
}

func (r *PoolReconciler) reconcileService(ctx context.Context, pool *sandboxv1alpha1.SandboxPool) error {
	port := r.WorkerPort
	if port == 0 {
		port = 8090
	}
	desired := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      workerSetName(pool.Name),
			Namespace: pool.Namespace,
		},
		Spec: corev1.ServiceSpec{
			ClusterIP: corev1.ClusterIPNone,
			Selector: map[string]string{
				labelManaged: "true",
				labelPool:    pool.Name,
			},
			Ports: []corev1.ServicePort{{
				Name: "http",
				Port: port,
			}},
		},
	}
	if err := ctrl.SetControllerReference(pool, desired, r.Scheme); err != nil {
		return fmt.Errorf("set Worker Service owner: %w", err)
	}

	var current corev1.Service
	key := types.NamespacedName{Namespace: desired.Namespace, Name: desired.Name}
	if err := r.Get(ctx, key, &current); apierrors.IsNotFound(err) {
		if err := r.Create(ctx, desired); err != nil {
			return fmt.Errorf("create Worker Service: %w", err)
		}
		return nil
	} else if err != nil {
		return fmt.Errorf("get Worker Service: %w", err)
	}
	return nil
}

func (r *PoolReconciler) reconcileStatefulSet(ctx context.Context, pool *sandboxv1alpha1.SandboxPool) error {
	desired := r.WorkloadBuilder.Build(pool)
	if err := ctrl.SetControllerReference(pool, desired, r.Scheme); err != nil {
		return fmt.Errorf("set Worker StatefulSet owner: %w", err)
	}

	var current appsv1.StatefulSet
	key := types.NamespacedName{Namespace: pool.Namespace, Name: desired.Name}
	if err := r.Get(ctx, key, &current); apierrors.IsNotFound(err) {
		if err := r.Create(ctx, desired); err != nil {
			return fmt.Errorf("create Worker StatefulSet: %w", err)
		}
		return nil
	} else if err != nil {
		return fmt.Errorf("get Worker StatefulSet: %w", err)
	}

	replicas := desired.Spec.Replicas
	if current.Spec.Replicas != nil && replicas != nil && *replicas < *current.Spec.Replicas {
		blocked, err := r.scaleDownBlocked(ctx, pool, *replicas, *current.Spec.Replicas)
		if err != nil {
			return err
		}
		if blocked {
			replicas = current.Spec.Replicas
		}
	}

	changed := false
	if !reflect.DeepEqual(current.Spec.Template, desired.Spec.Template) {
		current.Spec.Template = desired.Spec.Template
		changed = true
	}
	if current.Spec.Replicas == nil || replicas == nil || *current.Spec.Replicas != *replicas {
		current.Spec.Replicas = replicas
		changed = true
	}
	if changed {
		if err := r.Update(ctx, &current); err != nil {
			return fmt.Errorf("update Worker StatefulSet: %w", err)
		}
	}
	return nil
}

func (r *PoolReconciler) scaleDownBlocked(
	ctx context.Context,
	pool *sandboxv1alpha1.SandboxPool,
	desiredReplicas int32,
	currentReplicas int32,
) (bool, error) {
	for ordinal := desiredReplicas; ordinal < currentReplicas; ordinal++ {
		name := fmt.Sprintf("%s-%d", workerSetName(pool.Name), ordinal)
		var pod corev1.Pod
		if err := r.Get(ctx, types.NamespacedName{Namespace: pool.Namespace, Name: name}, &pod); apierrors.IsNotFound(err) {
			continue
		} else if err != nil {
			return false, fmt.Errorf("get scale-down Worker %q: %w", name, err)
		}
		endpoint, err := r.EndpointResolver.Endpoint(&pod)
		if err != nil {
			return true, nil
		}
		slots, err := r.WorkerClient.ListSlots(ctx, endpoint)
		if err != nil {
			return true, nil
		}
		for _, slotInfo := range slots {
			if slotInfo.State != slot.StateFree {
				return true, nil
			}
		}
	}
	return false, nil
}

func (r *PoolReconciler) refreshWorkers(ctx context.Context, pool *sandboxv1alpha1.SandboxPool) error {
	var pods corev1.PodList
	if err := r.List(ctx, &pods,
		client.InNamespace(pool.Namespace),
		client.MatchingLabels{labelManaged: "true", labelPool: pool.Name},
	); err != nil {
		return fmt.Errorf("list Worker Pods: %w", err)
	}

	var readyWorkers, usedSlots, availableSlots int32
	seen := make(map[string]struct{}, len(pods.Items))
	for i := range pods.Items {
		pod := &pods.Items[i]
		seen[pod.Name] = struct{}{}
		workerKey := scheduler.WorkerKey{Namespace: pool.Namespace, Pool: pool.Name, Name: pod.Name}
		if !podReady(pod) {
			r.Scheduler.RemoveWorker(workerKey)
			continue
		}
		endpoint, err := r.EndpointResolver.Endpoint(pod)
		if err != nil || r.WorkerClient.Health(ctx, endpoint) != nil {
			r.Scheduler.RemoveWorker(workerKey)
			continue
		}
		slots, err := r.WorkerClient.ListSlots(ctx, endpoint)
		if err != nil {
			r.Scheduler.RemoveWorker(workerKey)
			continue
		}

		slotMap := make(map[int32]slot.Info, len(slots))
		for _, slotInfo := range slots {
			slotMap[slotInfo.ID] = slotInfo
			if slotInfo.State == slot.StateFree {
				availableSlots++
			} else {
				usedSlots++
			}
		}
		r.Scheduler.UpdateWorker(scheduler.WorkerState{
			Key:          workerKey,
			Healthy:      true,
			LastObserved: time.Now(),
			Slots:        slotMap,
		})
		readyWorkers++
	}
	knownWorkers := pool.Status.CurrentWorkers
	if pool.Spec.Workers > knownWorkers {
		knownWorkers = pool.Spec.Workers
	}
	for ordinal := int32(0); ordinal < knownWorkers; ordinal++ {
		name := fmt.Sprintf("%s-%d", workerSetName(pool.Name), ordinal)
		if _, found := seen[name]; !found {
			r.Scheduler.RemoveWorker(scheduler.WorkerKey{Namespace: pool.Namespace, Pool: pool.Name, Name: name})
		}
	}

	previous := pool.Status.DeepCopy()
	pool.Status.ObservedGeneration = pool.Generation
	pool.Status.CurrentWorkers = int32(len(pods.Items))
	pool.Status.ReadyWorkers = readyWorkers
	pool.Status.UsedSlots = usedSlots
	pool.Status.AvailableSlots = availableSlots
	setPoolConditions(pool, readyWorkers)
	if reflect.DeepEqual(previous, &pool.Status) {
		return nil
	}
	if err := r.Status().Update(ctx, pool); err != nil {
		return fmt.Errorf("update SandboxPool status: %w", err)
	}
	return nil
}

func setPoolConditions(pool *sandboxv1alpha1.SandboxPool, readyWorkers int32) {
	expectedReady := readyWorkers == pool.Spec.Workers
	meta.SetStatusCondition(&pool.Status.Conditions, metav1.Condition{
		Type:               sandboxv1alpha1.ConditionWorkersReady,
		Status:             conditionStatus(expectedReady),
		ObservedGeneration: pool.Generation,
		Reason:             map[bool]string{true: "WorkersReady", false: "WorkersNotReady"}[expectedReady],
		Message:            fmt.Sprintf("%d of %d Workers are ready", readyWorkers, pool.Spec.Workers),
	})
	poolReady := pool.Spec.Workers > 0 && readyWorkers > 0
	meta.SetStatusCondition(&pool.Status.Conditions, metav1.Condition{
		Type:               sandboxv1alpha1.ConditionReady,
		Status:             conditionStatus(poolReady),
		ObservedGeneration: pool.Generation,
		Reason:             map[bool]string{true: "WorkerAvailable", false: "NoReadyWorkers"}[poolReady],
		Message:            fmt.Sprintf("%d Workers can accept Sandbox operations", readyWorkers),
	})
}

func conditionStatus(value bool) metav1.ConditionStatus {
	if value {
		return metav1.ConditionTrue
	}
	return metav1.ConditionFalse
}

func podReady(pod *corev1.Pod) bool {
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodReady {
			return condition.Status == corev1.ConditionTrue
		}
	}
	return false
}
