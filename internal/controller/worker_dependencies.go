package controller

import (
	"context"
	"fmt"
	"net"
	"strconv"

	sandboxv1alpha1 "github.com/AgentNaut/SandboxFleet/api/v1alpha1"
	"github.com/AgentNaut/SandboxFleet/internal/slot"
	"github.com/AgentNaut/SandboxFleet/internal/worker"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
)

const (
	labelManaged = "sandboxfleet.io/managed"
	labelPool    = "sandboxfleet.io/pool"
)

type WorkerClient interface {
	Health(ctx context.Context, endpoint string) error
	ListSlots(ctx context.Context, endpoint string) ([]slot.Info, error)
	ReserveSlot(ctx context.Context, endpoint string, ref worker.SandboxSlotRef) error
	StartSandbox(ctx context.Context, endpoint string, req worker.StartSandboxRequest) error
	StopSandbox(ctx context.Context, endpoint string, ref worker.SandboxSlotRef) error
	ReleaseSlot(ctx context.Context, endpoint string, ref worker.SandboxSlotRef) error
}

type WorkerEndpointResolver interface {
	Endpoint(pod *corev1.Pod) (string, error)
}

type PodIPResolver struct {
	Port int32
}

func (r PodIPResolver) Endpoint(pod *corev1.Pod) (string, error) {
	if pod.Status.PodIP == "" {
		return "", fmt.Errorf("worker pod %q has no IP", pod.Name)
	}
	return "http://" + net.JoinHostPort(pod.Status.PodIP, strconv.Itoa(int(r.Port))), nil
}

type WorkerWorkloadBuilder interface {
	Build(pool *sandboxv1alpha1.SandboxPool) *appsv1.StatefulSet
}

// WorkerProfile is the Pod shape for Workers of one runtime backend.
// Runtime-specific binaries belong in the image, not in controller logic.
type WorkerProfile struct {
	Image      string
	Privileged bool
}

// StatefulSetBuilder creates Worker workloads from a Pool and opaque profiles.
// Image selection is operator configuration; handler names are passed through.
type StatefulSetBuilder struct {
	Port int32
	// DefaultImage is used when Profiles has no entry for the Pool backend.
	DefaultImage string
	// Profiles maps runtime backend (for example "cri") to a Worker image/shape.
	Profiles map[string]WorkerProfile
}

func (b StatefulSetBuilder) Build(pool *sandboxv1alpha1.SandboxPool) *appsv1.StatefulSet {
	labels := map[string]string{
		labelManaged: "true",
		labelPool:    pool.Name,
	}
	profile := b.profileFor(pool)
	handler := ""
	if pool.Spec.Runtime.CRI != nil {
		handler = pool.Spec.Runtime.CRI.RuntimeHandler
	}

	podSpec := corev1.PodSpec{
		Containers: []corev1.Container{{
			Name:            "worker",
			Image:           profile.Image,
			ImagePullPolicy: corev1.PullIfNotPresent,
			Args: []string{
				"--name=$(WORKER_NAME)",
				"--namespace=$(WORKER_NAMESPACE)",
				"--pool=" + pool.Name,
				fmt.Sprintf("--slots=%d", pool.Spec.SlotsPerWorker),
				"--runtime-handler=" + handler,
				fmt.Sprintf("--listen=:%d", b.Port),
				"--slot-cpu-request=" + resourceValue(pool.Spec.SlotResources.Requests, corev1.ResourceCPU),
				"--slot-cpu-limit=" + resourceValue(pool.Spec.SlotResources.Limits, corev1.ResourceCPU),
				"--slot-memory-request=" + resourceValue(pool.Spec.SlotResources.Requests, corev1.ResourceMemory),
				"--slot-memory-limit=" + resourceValue(pool.Spec.SlotResources.Limits, corev1.ResourceMemory),
			},
			Env: []corev1.EnvVar{
				{
					Name: "WORKER_NAME",
					ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{
						FieldPath: "metadata.name",
					}},
				},
				{
					Name: "WORKER_NAMESPACE",
					ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{
						FieldPath: "metadata.namespace",
					}},
				},
			},
			Ports:     []corev1.ContainerPort{{Name: "http", ContainerPort: b.Port}},
			Resources: scaleResources(pool.Spec.SlotResources, pool.Spec.SlotsPerWorker),
			ReadinessProbe: &corev1.Probe{
				ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{
					Path: "/healthz",
					Port: intstr.FromString("http"),
				}},
			},
			SecurityContext: &corev1.SecurityContext{Privileged: ptr.To(profile.Privileged)},
		}},
	}

	// Nested containerd needs writable root/state directories. This is a CRI backend
	// concern, not specific to any runtime handler.
	if pool.Spec.Runtime.Backend == sandboxv1alpha1.RuntimeBackendCRI {
		podSpec.Volumes = []corev1.Volume{
			{Name: "containerd-root", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
			{Name: "containerd-state", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
		}
		podSpec.Containers[0].VolumeMounts = []corev1.VolumeMount{
			{Name: "containerd-root", MountPath: "/var/lib/containerd"},
			{Name: "containerd-state", MountPath: "/run/containerd"},
		}
	}

	return &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      workerSetName(pool.Name),
			Namespace: pool.Namespace,
		},
		Spec: appsv1.StatefulSetSpec{
			ServiceName: workerSetName(pool.Name),
			Replicas:    ptr.To(pool.Spec.Workers),
			Selector:    &metav1.LabelSelector{MatchLabels: labels},
			UpdateStrategy: appsv1.StatefulSetUpdateStrategy{
				Type: appsv1.OnDeleteStatefulSetStrategyType,
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec:       podSpec,
			},
		},
	}
}

func (b StatefulSetBuilder) profileFor(pool *sandboxv1alpha1.SandboxPool) WorkerProfile {
	backend := string(pool.Spec.Runtime.Backend)
	if override, ok := b.Profiles[backend]; ok {
		image := override.Image
		if image == "" {
			image = b.DefaultImage
		}
		return WorkerProfile{Image: image, Privileged: override.Privileged}
	}
	// CRI Workers typically run a local containerd; privilege is a backend concern,
	// not a specific handler (runsc/runc/...) concern.
	return WorkerProfile{
		Image:      b.DefaultImage,
		Privileged: backend == string(sandboxv1alpha1.RuntimeBackendCRI),
	}
}

func workerSetName(poolName string) string {
	return poolName + "-worker"
}

func resourceValue(resources corev1.ResourceList, name corev1.ResourceName) string {
	value, found := resources[name]
	if !found {
		return ""
	}
	return value.String()
}

func scaleResources(perSlot corev1.ResourceRequirements, slots int32) corev1.ResourceRequirements {
	return corev1.ResourceRequirements{
		Requests: scaleResourceList(perSlot.Requests, slots),
		Limits:   scaleResourceList(perSlot.Limits, slots),
	}
}

func scaleResourceList(perSlot corev1.ResourceList, slots int32) corev1.ResourceList {
	if len(perSlot) == 0 {
		return nil
	}
	result := make(corev1.ResourceList, len(perSlot))
	for name, quantity := range perSlot {
		scaled := quantity.DeepCopy()
		scaled.Mul(int64(slots))
		result[name] = scaled
	}
	return result
}
