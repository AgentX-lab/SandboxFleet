package controller

import (
	"strings"
	"testing"

	sandboxv1alpha1 "github.com/AgentNaut/SandboxFleet/api/v1alpha1"
	"github.com/AgentNaut/SandboxFleet/internal/slot"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func testPool() *sandboxv1alpha1.SandboxPool {
	return &sandboxv1alpha1.SandboxPool{
		ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: "test"},
		Spec: sandboxv1alpha1.SandboxPoolSpec{
			Runtime: sandboxv1alpha1.RuntimeConfig{
				Backend: sandboxv1alpha1.RuntimeBackendCRI,
				CRI:     &sandboxv1alpha1.CRIRuntimeConfig{RuntimeHandler: "test-handler"},
			},
			SlotProfiles: []sandboxv1alpha1.SlotProfile{{
				Name: "default",
				Resources: corev1.ResourceRequirements{
					Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("256Mi")},
				},
			}},
			WorkerTemplates: []sandboxv1alpha1.WorkerTemplate{{
				Name:     "mixed",
				Replicas: 2,
				Slots:    []sandboxv1alpha1.SlotGroup{{Profile: "default", Count: 4}},
			}},
		},
	}
}

func TestStatefulSetBuilderUsesStableWorkerNames(t *testing.T) {
	pool := testPool()
	template := pool.Spec.WorkerTemplates[0]
	specs := make([]slot.Config, 0, 4)
	for id := int32(0); id < 4; id++ {
		specs = append(specs, slot.Config{
			ID:        id,
			Profile:   "default",
			Resources: pool.Spec.SlotProfiles[0].Resources,
		})
	}

	workload := (StatefulSetBuilder{DefaultImage: "worker:test", Port: 8090}).Build(pool, template, specs)
	if workload.Name != "default-mixed-worker" {
		t.Fatalf("StatefulSet name = %q, want default-mixed-worker", workload.Name)
	}
	if workload.Spec.Replicas == nil || *workload.Spec.Replicas != 2 {
		t.Fatalf("StatefulSet replicas = %v, want 2", workload.Spec.Replicas)
	}
	container := workload.Spec.Template.Spec.Containers[0]
	if container.Image != "worker:test" {
		t.Fatalf("Worker image = %q, want worker:test", container.Image)
	}
	if got := container.Resources.Limits[corev1.ResourceMemory]; got.Cmp(resource.MustParse("1Gi")) != 0 {
		t.Fatalf("Worker memory limit = %s, want 1Gi", got.String())
	}
	if container.SecurityContext == nil || container.SecurityContext.Privileged == nil || !*container.SecurityContext.Privileged {
		t.Fatal("CRI Worker profile should default to privileged")
	}
	foundHandler := false
	for _, arg := range container.Args {
		if arg == "--runtime-handler=test-handler" {
			foundHandler = true
		}
		if strings.Contains(arg, "--slots-file=") {
			t.Fatalf("Worker should not mount slots file, got arg %q", arg)
		}
	}
	if !foundHandler {
		t.Fatalf("Worker args missing opaque runtime handler, got %v", container.Args)
	}
	if len(workload.Spec.Template.Spec.Volumes) != 2 {
		t.Fatalf("CRI Worker should mount containerd volumes only, got %d", len(workload.Spec.Template.Spec.Volumes))
	}
	if len(container.VolumeMounts) != 2 {
		t.Fatalf("CRI Worker volume mounts = %d, want 2", len(container.VolumeMounts))
	}
}

func TestStatefulSetBuilderUsesBackendProfile(t *testing.T) {
	pool := testPool()
	pool.ObjectMeta.Name = "demo"
	template := pool.Spec.WorkerTemplates[0]
	template.Replicas = 1
	template.Slots = []sandboxv1alpha1.SlotGroup{{Profile: "default", Count: 1}}
	specs := []slot.Config{{ID: 0, Profile: "default"}}

	workload := (StatefulSetBuilder{
		DefaultImage: "worker:default",
		Port:         8090,
		Profiles: map[string]WorkerProfile{
			"cri": {Image: "worker:cri", Privileged: true},
		},
	}).Build(pool, template, specs)

	container := workload.Spec.Template.Spec.Containers[0]
	if container.Image != "worker:cri" {
		t.Fatalf("Worker image = %q, want worker:cri", container.Image)
	}
	if !strings.Contains(strings.Join(container.Args, " "), "--runtime-handler=test-handler") {
		t.Fatalf("handler should pass through unchanged, got %v", container.Args)
	}
}

func TestStatefulSetBuilderMountsHostDevices(t *testing.T) {
	pool := testPool()
	pool.Spec.Runtime.CRI.RuntimeHandler = "kata"
	pool.Spec.Runtime.CRI.HostDevices = []string{"/dev/kvm"}
	template := pool.Spec.WorkerTemplates[0]
	template.Replicas = 1
	template.Slots = []sandboxv1alpha1.SlotGroup{{Profile: "default", Count: 1}}
	specs := []slot.Config{{ID: 0, Profile: "default"}}

	workload := (StatefulSetBuilder{DefaultImage: "worker:kata", Port: 8090}).Build(pool, template, specs)
	volumes := workload.Spec.Template.Spec.Volumes
	if len(volumes) != 3 {
		t.Fatalf("Worker volumes = %d, want 3 (containerd root/state + host device)", len(volumes))
	}
	found := false
	for _, volume := range volumes {
		if volume.Name != "host-device-0" {
			continue
		}
		found = true
		if volume.HostPath == nil || volume.HostPath.Path != "/dev/kvm" {
			t.Fatalf("host device path = %#v, want /dev/kvm", volume.HostPath)
		}
		if volume.HostPath.Type == nil || *volume.HostPath.Type != corev1.HostPathCharDev {
			t.Fatalf("host device type = %v, want CharDevice", volume.HostPath.Type)
		}
	}
	if !found {
		t.Fatal("missing host-device-0 volume")
	}
	mounts := workload.Spec.Template.Spec.Containers[0].VolumeMounts
	foundMount := false
	for _, mount := range mounts {
		if mount.Name == "host-device-0" && mount.MountPath == "/dev/kvm" {
			foundMount = true
		}
	}
	if !foundMount {
		t.Fatalf("missing /dev/kvm mount, got %#v", mounts)
	}
}

func TestStatefulSetBuilderIgnoresHandlerNameWithoutHostDevices(t *testing.T) {
	pool := testPool()
	pool.Spec.Runtime.CRI.RuntimeHandler = "kata"
	template := pool.Spec.WorkerTemplates[0]
	template.Replicas = 1
	template.Slots = []sandboxv1alpha1.SlotGroup{{Profile: "default", Count: 1}}
	specs := []slot.Config{{ID: 0, Profile: "default"}}

	workload := (StatefulSetBuilder{DefaultImage: "worker:kata", Port: 8090}).Build(pool, template, specs)
	if len(workload.Spec.Template.Spec.Volumes) != 2 {
		t.Fatalf("handler name alone must not mount devices, volumes=%d", len(workload.Spec.Template.Spec.Volumes))
	}
}
