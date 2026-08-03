package cri

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

func TestLinuxResources(t *testing.T) {
	resources := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU: resource.MustParse("250m"),
		},
		Limits: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("500m"),
			corev1.ResourceMemory: resource.MustParse("512Mi"),
		},
	}

	got := linuxResources(resources)
	if got.CpuShares != 256 {
		t.Fatalf("CpuShares = %d, want 256", got.CpuShares)
	}
	if got.CpuPeriod != 100_000 || got.CpuQuota != 50_000 {
		t.Fatalf("CPU quota = %d/%d, want 50000/100000", got.CpuQuota, got.CpuPeriod)
	}
	if got.MemoryLimitInBytes != 512*1024*1024 {
		t.Fatalf("MemoryLimitInBytes = %d, want 512Mi", got.MemoryLimitInBytes)
	}
}
