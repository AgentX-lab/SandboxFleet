package slot

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

func TestPlanTopologyRemovesHighestFreeIDs(t *testing.T) {
	current := []Config{
		{ID: 0, Profile: "small"},
		{ID: 1, Profile: "small"},
		{ID: 2, Profile: "large"},
	}
	result := PlanTopology(current, ProfileCounts{"small": 1, "large": 1}, ProfileResources{
		"small": {},
		"large": {},
	}, nil, []string{"small", "large"})
	if result.Blocked {
		t.Fatalf("unexpected block: %s", result.Reason)
	}
	if len(result.Slots) != 2 || result.Slots[0].ID != 0 || result.Slots[1].ID != 2 {
		t.Fatalf("slots = %#v, want IDs 0 and 2", result.Slots)
	}
}

func TestPlanTopologyFillsHoles(t *testing.T) {
	current := []Config{
		{ID: 0, Profile: "small"},
		{ID: 2, Profile: "large"},
	}
	resources := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m")},
	}
	result := PlanTopology(current, ProfileCounts{"small": 2, "large": 1}, ProfileResources{
		"small": resources,
		"large": {},
	}, nil, []string{"small", "large"})
	if result.Blocked {
		t.Fatalf("unexpected block: %s", result.Reason)
	}
	if len(result.Slots) != 3 || result.Slots[1].ID != 1 || result.Slots[1].Profile != "small" {
		t.Fatalf("slots = %#v, want hole ID 1 reused for small", result.Slots)
	}
}

func TestPlanTopologyBlocksBusyRemoval(t *testing.T) {
	current := []Config{
		{ID: 0, Profile: "small"},
		{ID: 1, Profile: "small"},
	}
	result := PlanTopology(current, ProfileCounts{"small": 1}, ProfileResources{"small": {}}, map[int32]bool{1: true}, []string{"small"})
	if !result.Blocked {
		t.Fatal("expected busy removal to block")
	}
	if len(result.Slots) != 2 {
		t.Fatalf("slots = %#v, want both slots retained", result.Slots)
	}
}

func TestPlanTopologyCreatesFromEmptyInOrder(t *testing.T) {
	result := PlanTopology(nil, ProfileCounts{"small": 2, "large": 1}, ProfileResources{
		"small": {},
		"large": {},
	}, nil, []string{"small", "large"})
	if result.Blocked {
		t.Fatalf("unexpected block: %s", result.Reason)
	}
	if len(result.Slots) != 3 {
		t.Fatalf("slots = %#v, want 3", result.Slots)
	}
	if result.Slots[0].Profile != "small" || result.Slots[1].Profile != "small" || result.Slots[2].Profile != "large" {
		t.Fatalf("slots = %#v, want small,small,large", result.Slots)
	}
}

func TestKeepExistingSlots(t *testing.T) {
	current := []Config{{ID: 0, Profile: "small"}, {ID: 2, Profile: "large"}}
	proposed := []Config{{ID: 0, Profile: "small"}, {ID: 1, Profile: "small"}, {ID: 2, Profile: "large"}}
	if CountNewSlots(current, proposed) != 1 {
		t.Fatalf("CountNewSlots = %d, want 1", CountNewSlots(current, proposed))
	}
	got := KeepExistingSlots(current, proposed)
	if len(got) != 2 || got[0].ID != 0 || got[1].ID != 2 {
		t.Fatalf("KeepExistingSlots = %#v", got)
	}
}

func TestResourcesEnough(t *testing.T) {
	have := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("200m")},
		Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("512Mi")},
	}
	need := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("200m")},
		Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("256Mi")},
	}
	if !ResourcesEnough(have, need) {
		t.Fatal("expected resources to be enough")
	}
	need.Requests[corev1.ResourceCPU] = resource.MustParse("300m")
	if ResourcesEnough(have, need) {
		t.Fatal("expected resources not to be enough")
	}
}
