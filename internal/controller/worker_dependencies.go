package controller

import (
	"context"
	"fmt"
	"net"
	"path"
	"strconv"
	"strings"

	sandboxv1alpha1 "github.com/AgentNaut/SandboxFleet/api/v1alpha1"
	"github.com/AgentNaut/SandboxFleet/internal/slot"
	"github.com/AgentNaut/SandboxFleet/internal/workerapi"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
)

const (
	labelManaged  = "sandboxfleet.io/managed"
	labelPool     = "sandboxfleet.io/pool"
	labelTemplate = "sandboxfleet.io/template"
)

type WorkerClient interface {
	Health(ctx context.Context, endpoint string) error
	ListSlots(ctx context.Context, endpoint string) ([]slot.Info, error)
	ApplyTopology(ctx context.Context, endpoint string, configs []slot.Config) error
	ReserveSlot(ctx context.Context, endpoint string, ref workerapi.SandboxSlotRef) error
	StartSandbox(ctx context.Context, endpoint string, req workerapi.StartSandboxRequest) error
	StopSandbox(ctx context.Context, endpoint string, ref workerapi.SandboxSlotRef) error
	ReleaseSlot(ctx context.Context, endpoint string, ref workerapi.SandboxSlotRef) error
	CreateSnapshot(ctx context.Context, endpoint string, req workerapi.CreateSnapshotRequest) (workerapi.CreateSnapshotResult, error)
	RestoreFromSnapshot(ctx context.Context, endpoint string, req workerapi.RestoreFromSnapshotRequest) error
	DeleteSnapshotObjects(ctx context.Context, endpoint string, req workerapi.DeleteSnapshotObjectsRequest) error
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
	Build(pool *sandboxv1alpha1.SandboxPool, template sandboxv1alpha1.WorkerTemplate, configs []slot.Config) *appsv1.StatefulSet
}

// WorkerProfile is the Pod shape for Workers of one runtime backend.
type WorkerProfile struct {
	Image      string
	Privileged bool
}

// StatefulSetBuilder creates Worker workloads from a Pool Template and Slot configs.
type StatefulSetBuilder struct {
	Port         int32
	DefaultImage string
	Profiles     map[string]WorkerProfile
}

func (b StatefulSetBuilder) Build(
	pool *sandboxv1alpha1.SandboxPool,
	template sandboxv1alpha1.WorkerTemplate,
	configs []slot.Config,
) *appsv1.StatefulSet {
	labels := workerLabels(pool.Name, template.Name)
	profile := b.profileFor(pool)
	handler := ""
	snapshotterKind := ""
	var hostDevices []string
	if pool.Spec.Runtime.CRI != nil {
		handler = pool.Spec.Runtime.CRI.RuntimeHandler
		snapshotterKind = string(pool.Spec.Runtime.CRI.Snapshotter)
		hostDevices = pool.Spec.Runtime.CRI.HostDevices
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
				"--runtime-handler=" + handler,
				"--snapshotter=" + snapshotterKind,
				fmt.Sprintf("--listen=:%d", b.Port),
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
			Resources: slot.SumResources(configs),
			ReadinessProbe: &corev1.Probe{
				ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{
					Path: "/healthz",
					Port: intstr.FromString("http"),
				}},
			},
			SecurityContext: &corev1.SecurityContext{Privileged: ptr.To(profile.Privileged)},
		}},
	}

	if pool.Spec.Runtime.Backend == sandboxv1alpha1.RuntimeBackendCRI {
		// EmptyDirs are node-backed (not the container overlay rootfs). Overlay
		// upper/work for gVisor restore must live here — same reason substrate
		// puts bundles on /var/lib/ateom-gvisor hostPath.
		podSpec.Volumes = []corev1.Volume{
			{Name: "containerd-root", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
			{Name: "containerd-state", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
			{Name: "sandboxfleet-data", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
		}
		podSpec.Containers[0].VolumeMounts = []corev1.VolumeMount{
			{Name: "containerd-root", MountPath: "/var/lib/containerd"},
			{Name: "containerd-state", MountPath: "/run/containerd"},
			{Name: "sandboxfleet-data", MountPath: "/var/lib/sandboxfleet"},
		}
		mountHostDevices(&podSpec, hostDevices)
	}

	return &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      workerSetName(pool.Name, template.Name),
			Namespace: pool.Namespace,
			Labels:    labels,
		},
		Spec: appsv1.StatefulSetSpec{
			ServiceName: workerServiceName(pool.Name),
			Replicas:    ptr.To(template.Replicas),
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
	return WorkerProfile{
		Image:      b.DefaultImage,
		Privileged: backend == string(sandboxv1alpha1.RuntimeBackendCRI),
	}
}

// mountHostDevices appends hostPath volumes for each declared device path.
func mountHostDevices(podSpec *corev1.PodSpec, devices []string) {
	for i, devicePath := range devices {
		devicePath = path.Clean(devicePath)
		if devicePath == "." || devicePath == "/" || !strings.HasPrefix(devicePath, "/") {
			continue
		}
		name := fmt.Sprintf("host-device-%d", i)
		hostPathType := corev1.HostPathCharDev
		podSpec.Volumes = append(podSpec.Volumes, corev1.Volume{
			Name: name,
			VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{
				Path: devicePath,
				Type: &hostPathType,
			}},
		})
		podSpec.Containers[0].VolumeMounts = append(podSpec.Containers[0].VolumeMounts, corev1.VolumeMount{
			Name:      name,
			MountPath: devicePath,
		})
	}
}

func workerLabels(poolName, templateName string) map[string]string {
	return map[string]string{
		labelManaged:  "true",
		labelPool:     poolName,
		labelTemplate: templateName,
	}
}

func workerServiceName(poolName string) string {
	return poolName + "-worker"
}

func workerSetName(poolName, templateName string) string {
	return poolName + "-" + templateName + "-worker"
}
