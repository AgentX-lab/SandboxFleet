package controller

import (
	"context"
	"fmt"
	"reflect"
	"sort"
	"strings"
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

	if err := r.syncService(ctx, &pool); err != nil {
		return ctrl.Result{}, err
	}

	var pending []string
	for _, template := range pool.Spec.WorkerTemplates {
		reasons, err := r.syncTemplate(ctx, &pool, template)
		if err != nil {
			return ctrl.Result{}, err
		}
		pending = append(pending, reasons...)
	}

	if err := r.syncStatus(ctx, &pool, pending); err != nil {
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

func (r *PoolReconciler) syncService(ctx context.Context, pool *sandboxv1alpha1.SandboxPool) error {
	port := r.WorkerPort
	if port == 0 {
		port = 8090
	}
	desired := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      workerServiceName(pool.Name),
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

// syncTemplate makes one WorkerTemplate match Spec: Slot layout + Worker replicas.
func (r *PoolReconciler) syncTemplate(
	ctx context.Context,
	pool *sandboxv1alpha1.SandboxPool,
	template sandboxv1alpha1.WorkerTemplate,
) ([]string, error) {
	profileResources := make(slot.ProfileResources, len(pool.Spec.SlotProfiles))
	for _, profile := range pool.Spec.SlotProfiles {
		profileResources[profile.Name] = profile.Resources
	}
	currentSlots := appliedConfigs(pool, template.Name, profileResources)
	counts, order, err := slotCountsFromTemplate(template, profileResources)
	if err != nil {
		return nil, err
	}

	occupied, err := r.occupiedSlots(ctx, pool, template.Name)
	if err != nil {
		return nil, err
	}

	result := slot.PlanTopology(currentSlots, counts, profileResources, occupied, order)
	nextSlots := result.Slots
	var pending []string
	if result.Blocked {
		pending = append(pending, fmt.Sprintf("template %s: %s", template.Name, result.Reason))
	}

	// Adding Slots requires free resource headroom on existing Worker Pods.
	if slot.CountNewSlots(currentSlots, nextSlots) > 0 {
		canFit, err := r.workersCanFit(ctx, pool, template.Name, slot.SumResources(nextSlots))
		if err != nil {
			return nil, err
		}
		if !canFit {
			nextSlots = slot.KeepExistingSlots(currentSlots, nextSlots)
			pending = append(pending, fmt.Sprintf(
				"template %s: workers lack resource headroom for more slots; increase replicas",
				template.Name,
			))
		}
	}

	if pushPending, err := r.syncTopology(ctx, pool, template.Name, nextSlots); err != nil {
		return nil, err
	} else if pushPending != "" {
		pending = append(pending, pushPending)
	}
	workersBusy, err := r.syncWorkers(ctx, pool, template, nextSlots)
	if err != nil {
		return nil, err
	}
	if workersBusy {
		pending = append(pending, fmt.Sprintf(
			"template %s: cannot remove Workers that still have busy slots",
			template.Name,
		))
	}
	return pending, nil
}

// syncTopology pushes Slot configs to Ready Workers, then records them in Status.
// Returns a pending reason when some Ready Workers could not be updated.
func (r *PoolReconciler) syncTopology(
	ctx context.Context,
	pool *sandboxv1alpha1.SandboxPool,
	templateName string,
	configs []slot.Config,
) (string, error) {
	var pods corev1.PodList
	if err := r.List(ctx, &pods,
		client.InNamespace(pool.Namespace),
		client.MatchingLabels{labelManaged: "true", labelPool: pool.Name, labelTemplate: templateName},
	); err != nil {
		return "", fmt.Errorf("list Worker Pods: %w", err)
	}

	var failed []string
	for i := range pods.Items {
		pod := &pods.Items[i]
		if !podReady(pod) {
			continue
		}
		endpoint, err := r.EndpointResolver.Endpoint(pod)
		if err != nil {
			failed = append(failed, pod.Name)
			continue
		}
		if err := r.WorkerClient.ApplyTopology(ctx, endpoint, configs); err != nil {
			failed = append(failed, pod.Name)
		}
	}
	if len(failed) > 0 {
		return fmt.Sprintf("template %s: failed to push topology to workers %s", templateName, strings.Join(failed, ",")), nil
	}
	setAppliedSlots(pool, templateName, configs)
	return "", nil
}

// syncWorkers creates/updates the StatefulSet. Returns true if scale-down was
// skipped because target Workers still have busy Slots.
func (r *PoolReconciler) syncWorkers(
	ctx context.Context,
	pool *sandboxv1alpha1.SandboxPool,
	template sandboxv1alpha1.WorkerTemplate,
	configs []slot.Config,
) (workersStillBusy bool, err error) {
	desired := r.WorkloadBuilder.Build(pool, template, configs)
	if err := ctrl.SetControllerReference(pool, desired, r.Scheme); err != nil {
		return false, fmt.Errorf("set Worker StatefulSet owner: %w", err)
	}

	var current appsv1.StatefulSet
	key := types.NamespacedName{Namespace: pool.Namespace, Name: desired.Name}
	if err := r.Get(ctx, key, &current); apierrors.IsNotFound(err) {
		if err := r.Create(ctx, desired); err != nil {
			return false, fmt.Errorf("create Worker StatefulSet: %w", err)
		}
		return false, nil
	} else if err != nil {
		return false, fmt.Errorf("get Worker StatefulSet: %w", err)
	}

	replicas := desired.Spec.Replicas
	if current.Spec.Replicas != nil && replicas != nil && *replicas < *current.Spec.Replicas {
		busy, err := r.workersStillBusy(ctx, pool, template.Name, *replicas, *current.Spec.Replicas)
		if err != nil {
			return false, err
		}
		if busy {
			replicas = current.Spec.Replicas
			workersStillBusy = true
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
			return false, fmt.Errorf("update Worker StatefulSet: %w", err)
		}
	}
	return workersStillBusy, nil
}

func (r *PoolReconciler) workersStillBusy(
	ctx context.Context,
	pool *sandboxv1alpha1.SandboxPool,
	templateName string,
	keep int32,
	have int32,
) (bool, error) {
	for ordinal := keep; ordinal < have; ordinal++ {
		name := fmt.Sprintf("%s-%d", workerSetName(pool.Name, templateName), ordinal)
		var pod corev1.Pod
		if err := r.Get(ctx, types.NamespacedName{Namespace: pool.Namespace, Name: name}, &pod); apierrors.IsNotFound(err) {
			continue
		} else if err != nil {
			return false, fmt.Errorf("get Worker %q: %w", name, err)
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

// appliedConfigs returns Status.AppliedSlots for a Template, with resources from profileResources.
func appliedConfigs(pool *sandboxv1alpha1.SandboxPool, templateName string, profileResources slot.ProfileResources) []slot.Config {
	for _, template := range pool.Status.Templates {
		if template.Name != templateName {
			continue
		}
		configs := make([]slot.Config, 0, len(template.AppliedSlots))
		for _, applied := range template.AppliedSlots {
			cfg := slot.Config{ID: applied.ID, Profile: applied.Profile}
			if resources, ok := profileResources[applied.Profile]; ok {
				cfg.Resources = resources
			}
			configs = append(configs, cfg)
		}
		return configs
	}
	return nil
}

func setAppliedSlots(pool *sandboxv1alpha1.SandboxPool, templateName string, configs []slot.Config) {
	applied := make([]sandboxv1alpha1.AppliedSlot, 0, len(configs))
	for _, cfg := range configs {
		applied = append(applied, sandboxv1alpha1.AppliedSlot{ID: cfg.ID, Profile: cfg.Profile})
	}
	for i := range pool.Status.Templates {
		if pool.Status.Templates[i].Name == templateName {
			pool.Status.Templates[i].AppliedSlots = applied
			return
		}
	}
	pool.Status.Templates = append(pool.Status.Templates, sandboxv1alpha1.WorkerTemplateStatus{
		Name:         templateName,
		AppliedSlots: applied,
	})
}

func (r *PoolReconciler) occupiedSlots(
	ctx context.Context,
	pool *sandboxv1alpha1.SandboxPool,
	templateName string,
) (map[int32]bool, error) {
	var pods corev1.PodList
	if err := r.List(ctx, &pods,
		client.InNamespace(pool.Namespace),
		client.MatchingLabels{labelManaged: "true", labelPool: pool.Name, labelTemplate: templateName},
	); err != nil {
		return nil, fmt.Errorf("list Worker Pods: %w", err)
	}
	occupied := map[int32]bool{}
	for i := range pods.Items {
		pod := &pods.Items[i]
		endpoint, err := r.EndpointResolver.Endpoint(pod)
		if err != nil {
			continue
		}
		slots, err := r.WorkerClient.ListSlots(ctx, endpoint)
		if err != nil {
			continue
		}
		for _, slotInfo := range slots {
			if slotInfo.State != slot.StateFree {
				occupied[slotInfo.ID] = true
			}
		}
	}
	return occupied, nil
}

func (r *PoolReconciler) workersCanFit(
	ctx context.Context,
	pool *sandboxv1alpha1.SandboxPool,
	templateName string,
	need corev1.ResourceRequirements,
) (bool, error) {
	var pods corev1.PodList
	if err := r.List(ctx, &pods,
		client.InNamespace(pool.Namespace),
		client.MatchingLabels{labelManaged: "true", labelPool: pool.Name, labelTemplate: templateName},
	); err != nil {
		return false, fmt.Errorf("list Worker Pods: %w", err)
	}
	if len(pods.Items) == 0 {
		return true, nil
	}
	for i := range pods.Items {
		pod := &pods.Items[i]
		if len(pod.Spec.Containers) == 0 {
			return false, nil
		}
		if !slot.ResourcesEnough(pod.Spec.Containers[0].Resources, need) {
			return false, nil
		}
	}
	return true, nil
}

// syncStatus pushes Worker inventory into the Scheduler and Pool Status.
func (r *PoolReconciler) syncStatus(ctx context.Context, pool *sandboxv1alpha1.SandboxPool, pending []string) error {
	var pods corev1.PodList
	if err := r.List(ctx, &pods,
		client.InNamespace(pool.Namespace),
		client.MatchingLabels{labelManaged: "true", labelPool: pool.Name},
	); err != nil {
		return fmt.Errorf("list Worker Pods: %w", err)
	}

	var readyWorkers, usedSlots, availableSlots int32
	profileStats := make(map[string]*sandboxv1alpha1.SlotProfileStatus)
	for _, profile := range pool.Spec.SlotProfiles {
		profileStats[profile.Name] = &sandboxv1alpha1.SlotProfileStatus{Name: profile.Name}
	}
	templateReady := make(map[string]int32)
	templateTotal := make(map[string]int32)
	seen := make(map[string]struct{}, len(pods.Items))

	for i := range pods.Items {
		pod := &pods.Items[i]
		seen[pod.Name] = struct{}{}
		templateName := pod.Labels[labelTemplate]
		templateTotal[templateName]++
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
			stats := profileStats[slotInfo.Profile]
			if stats == nil && slotInfo.Profile != "" {
				stats = &sandboxv1alpha1.SlotProfileStatus{Name: slotInfo.Profile}
				profileStats[slotInfo.Profile] = stats
			}
			if slotInfo.State == slot.StateFree {
				availableSlots++
				if stats != nil {
					stats.Available++
					stats.Total++
				}
			} else {
				usedSlots++
				if stats != nil {
					stats.Used++
					stats.Total++
				}
			}
		}
		r.Scheduler.UpdateWorker(scheduler.WorkerState{
			Key:          workerKey,
			Healthy:      true,
			LastObserved: time.Now(),
			Slots:        slotMap,
		})
		readyWorkers++
		templateReady[templateName]++
	}

	for _, template := range pool.Spec.WorkerTemplates {
		known := templateTotal[template.Name]
		if template.Replicas > known {
			known = template.Replicas
		}
		for ordinal := int32(0); ordinal < known; ordinal++ {
			name := fmt.Sprintf("%s-%d", workerSetName(pool.Name, template.Name), ordinal)
			if _, found := seen[name]; !found {
				r.Scheduler.RemoveWorker(scheduler.WorkerKey{Namespace: pool.Namespace, Pool: pool.Name, Name: name})
			}
		}
	}

	previous := pool.Status.DeepCopy()
	appliedByTemplate := make(map[string][]sandboxv1alpha1.AppliedSlot, len(pool.Status.Templates))
	for _, template := range pool.Status.Templates {
		if len(template.AppliedSlots) == 0 {
			continue
		}
		appliedByTemplate[template.Name] = append([]sandboxv1alpha1.AppliedSlot(nil), template.AppliedSlots...)
	}

	pool.Status.ObservedGeneration = pool.Generation
	pool.Status.CurrentWorkers = int32(len(pods.Items))
	pool.Status.ReadyWorkers = readyWorkers
	pool.Status.UsedSlots = usedSlots
	pool.Status.AvailableSlots = availableSlots
	pool.Status.Templates = make([]sandboxv1alpha1.WorkerTemplateStatus, 0, len(pool.Spec.WorkerTemplates))
	for _, template := range pool.Spec.WorkerTemplates {
		pool.Status.Templates = append(pool.Status.Templates, sandboxv1alpha1.WorkerTemplateStatus{
			Name:          template.Name,
			Replicas:      templateTotal[template.Name],
			ReadyReplicas: templateReady[template.Name],
			AppliedSlots:  appliedByTemplate[template.Name],
		})
	}
	pool.Status.Profiles = make([]sandboxv1alpha1.SlotProfileStatus, 0, len(pool.Spec.SlotProfiles))
	names := make([]string, 0, len(pool.Spec.SlotProfiles))
	for _, profile := range pool.Spec.SlotProfiles {
		names = append(names, profile.Name)
	}
	sort.Strings(names)
	for _, name := range names {
		if profileStats[name] == nil {
			pool.Status.Profiles = append(pool.Status.Profiles, sandboxv1alpha1.SlotProfileStatus{Name: name})
			continue
		}
		pool.Status.Profiles = append(pool.Status.Profiles, *profileStats[name])
	}
	setPoolConditions(pool, readyWorkers, pending)
	if reflect.DeepEqual(previous, &pool.Status) {
		return nil
	}
	if err := r.Status().Update(ctx, pool); err != nil {
		return fmt.Errorf("update SandboxPool status: %w", err)
	}
	return nil
}

func slotCountsFromTemplate(
	template sandboxv1alpha1.WorkerTemplate,
	profileResources slot.ProfileResources,
) (slot.ProfileCounts, []string, error) {
	counts := slot.ProfileCounts{}
	order := make([]string, 0, len(template.Slots))
	for _, group := range template.Slots {
		if _, ok := profileResources[group.Profile]; !ok {
			return nil, nil, fmt.Errorf("template %q references unknown profile %q", template.Name, group.Profile)
		}
		if _, exists := counts[group.Profile]; exists {
			return nil, nil, fmt.Errorf("template %q repeats profile %q", template.Name, group.Profile)
		}
		counts[group.Profile] = group.Count
		order = append(order, group.Profile)
	}
	return counts, order, nil
}

func hasSlotProfile(spec sandboxv1alpha1.SandboxPoolSpec, name string) bool {
	for _, profile := range spec.SlotProfiles {
		if profile.Name == name {
			return true
		}
	}
	return false
}

func desiredWorkerCount(spec sandboxv1alpha1.SandboxPoolSpec) int32 {
	var total int32
	for _, template := range spec.WorkerTemplates {
		total += template.Replicas
	}
	return total
}

func setPoolConditions(pool *sandboxv1alpha1.SandboxPool, readyWorkers int32, pending []string) {
	wantWorkers := desiredWorkerCount(pool.Spec)

	workersReady := readyWorkers == wantWorkers && wantWorkers > 0
	workersReadyReason := "WorkersNotReady"
	if workersReady {
		workersReadyReason = "WorkersReady"
	}
	meta.SetStatusCondition(&pool.Status.Conditions, metav1.Condition{
		Type:               sandboxv1alpha1.ConditionWorkersReady,
		Status:             conditionStatus(workersReady),
		ObservedGeneration: pool.Generation,
		Reason:             workersReadyReason,
		Message:            fmt.Sprintf("%d of %d Workers are ready", readyWorkers, wantWorkers),
	})

	poolReady := wantWorkers > 0 && readyWorkers > 0
	poolReadyReason := "NoReadyWorkers"
	if poolReady {
		poolReadyReason = "WorkerAvailable"
	}
	meta.SetStatusCondition(&pool.Status.Conditions, metav1.Condition{
		Type:               sandboxv1alpha1.ConditionReady,
		Status:             conditionStatus(poolReady),
		ObservedGeneration: pool.Generation,
		Reason:             poolReadyReason,
		Message:            fmt.Sprintf("%d Workers can accept Sandbox operations", readyWorkers),
	})

	updating := len(pending) > 0
	updatingReason := "TopologySettled"
	updatingMessage := "pool topology matches the desired replicas and slot counts"
	if updating {
		updatingReason = "TopologyPending"
		updatingMessage = strings.Join(pending, "; ")
	}
	meta.SetStatusCondition(&pool.Status.Conditions, metav1.Condition{
		Type:               sandboxv1alpha1.ConditionUpdating,
		Status:             conditionStatus(updating),
		ObservedGeneration: pool.Generation,
		Reason:             updatingReason,
		Message:            updatingMessage,
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
