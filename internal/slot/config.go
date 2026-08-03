package slot

import (
	"fmt"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
)

// Config is one Slot on a Worker: stable ID, profile name, fixed resources.
type Config struct {
	ID        int32                       `json:"id"`
	Profile   string                      `json:"profile"`
	Resources corev1.ResourceRequirements `json:"resources"`
}

// ProfileCounts is profile name -> how many Slots that profile should have.
type ProfileCounts map[string]int32

// ProfileResources is profile name -> fixed resources for that profile.
type ProfileResources map[string]corev1.ResourceRequirements

// TopologyPlan is the Slot layout after trying to match desired counts.
type TopologyPlan struct {
	Slots   []Config
	Blocked bool
	Reason  string
}

// PlanTopology adjusts Slot counts per profile.
//
// Rules (simple):
//   - Keep existing Slot IDs when possible.
//   - Scale down: drop highest IDs of that profile first; skip busy ones (Blocked).
//   - Scale up: reuse free ID holes, then maxID+1.
//   - profileOrder controls which profile gets new IDs first (template slot order).
//
// Callers must check Pod resource headroom before applying any newly added Slots.
func PlanTopology(
	current []Config,
	counts ProfileCounts,
	profiles ProfileResources,
	busy map[int32]bool,
	profileOrder []string,
) TopologyPlan {
	byProfile := groupByProfile(current)
	usedIDs := map[int32]struct{}{}
	maxID := int32(-1)
	for _, cfg := range current {
		usedIDs[cfg.ID] = struct{}{}
		if cfg.ID > maxID {
			maxID = cfg.ID
		}
	}

	// Walk profiles in template order, then any leftovers.
	names := orderedProfiles(profileOrder, counts, byProfile)

	next := make([]Config, 0, len(current))
	var reasons []string

	for _, profile := range names {
		desired := counts[profile]
		existing := byProfile[profile]
		resources, known := profiles[profile]

		if !known && desired > 0 {
			reasons = append(reasons, fmt.Sprintf("unknown slot profile %q", profile))
			next = append(next, existing...)
			continue
		}

		// Too many: keep the lowest IDs, try to drop the rest if free.
		if int32(len(existing)) > desired {
			keep := existing[:desired]
			for _, cfg := range existing[desired:] {
				if busy[cfg.ID] {
					reasons = append(reasons, fmt.Sprintf("slot %d (%s) is busy", cfg.ID, profile))
					keep = append(keep, cfg)
					continue
				}
				delete(usedIDs, cfg.ID)
			}
			sort.Slice(keep, func(i, j int) bool { return keep[i].ID < keep[j].ID })
			next = append(next, keep...)
			continue
		}

		// Keep what we have, then add missing.
		next = append(next, existing...)
		for missing := desired - int32(len(existing)); missing > 0; missing-- {
			id := nextFreeID(usedIDs, &maxID)
			usedIDs[id] = struct{}{}
			next = append(next, Config{
				ID:        id,
				Profile:   profile,
				Resources: cloneResources(resources),
			})
		}
	}

	sort.Slice(next, func(i, j int) bool { return next[i].ID < next[j].ID })
	result := TopologyPlan{Slots: next, Blocked: len(reasons) > 0}
	if result.Blocked {
		result.Reason = strings.Join(reasons, "; ")
	}
	return result
}

// KeepExistingSlots keeps only Configs whose IDs already exist in current.
// Used when Workers do not have enough resources for scale-up.
func KeepExistingSlots(current, proposed []Config) []Config {
	existing := make(map[int32]struct{}, len(current))
	for _, cfg := range current {
		existing[cfg.ID] = struct{}{}
	}
	out := make([]Config, 0, len(proposed))
	for _, cfg := range proposed {
		if _, ok := existing[cfg.ID]; ok {
			out = append(out, cfg)
		}
	}
	return out
}

// CountNewSlots returns how many Configs in proposed are new IDs.
func CountNewSlots(current, proposed []Config) int {
	existing := make(map[int32]struct{}, len(current))
	for _, cfg := range current {
		existing[cfg.ID] = struct{}{}
	}
	n := 0
	for _, cfg := range proposed {
		if _, ok := existing[cfg.ID]; !ok {
			n++
		}
	}
	return n
}

// SumResources adds up all Slot resource requirements.
func SumResources(configs []Config) corev1.ResourceRequirements {
	var sum corev1.ResourceRequirements
	for _, cfg := range configs {
		sum.Requests = addResourceList(sum.Requests, cfg.Resources.Requests)
		sum.Limits = addResourceList(sum.Limits, cfg.Resources.Limits)
	}
	return sum
}

// ResourcesEnough is true when have >= need for every resource name.
func ResourcesEnough(have, need corev1.ResourceRequirements) bool {
	return resourceListCovers(have.Requests, need.Requests) && resourceListCovers(have.Limits, need.Limits)
}

func groupByProfile(current []Config) map[string][]Config {
	byProfile := make(map[string][]Config)
	for _, cfg := range current {
		byProfile[cfg.Profile] = append(byProfile[cfg.Profile], cfg)
	}
	for profile, configs := range byProfile {
		sort.Slice(configs, func(i, j int) bool { return configs[i].ID < configs[j].ID })
		byProfile[profile] = configs
	}
	return byProfile
}

func orderedProfiles(order []string, counts ProfileCounts, byProfile map[string][]Config) []string {
	seen := map[string]struct{}{}
	names := make([]string, 0, len(order)+len(counts)+len(byProfile))
	for _, profile := range order {
		if _, ok := seen[profile]; ok {
			continue
		}
		seen[profile] = struct{}{}
		names = append(names, profile)
	}
	extras := make([]string, 0)
	for profile := range counts {
		if _, ok := seen[profile]; !ok {
			extras = append(extras, profile)
		}
	}
	for profile := range byProfile {
		if _, ok := seen[profile]; !ok {
			extras = append(extras, profile)
		}
	}
	sort.Strings(extras)
	return append(names, extras...)
}

func nextFreeID(used map[int32]struct{}, maxID *int32) int32 {
	for id := int32(0); id <= *maxID; id++ {
		if _, found := used[id]; !found {
			return id
		}
	}
	*maxID++
	return *maxID
}

func resourceListCovers(have, need corev1.ResourceList) bool {
	for name, required := range need {
		available, ok := have[name]
		if !ok {
			if required.IsZero() {
				continue
			}
			return false
		}
		if available.Cmp(required) < 0 {
			return false
		}
	}
	return true
}

func addResourceList(base, add corev1.ResourceList) corev1.ResourceList {
	if len(add) == 0 {
		return base
	}
	result := corev1.ResourceList{}
	for name, quantity := range base {
		result[name] = quantity.DeepCopy()
	}
	for name, quantity := range add {
		current, ok := result[name]
		if !ok {
			result[name] = quantity.DeepCopy()
			continue
		}
		current.Add(quantity)
		result[name] = current
	}
	return result
}

func cloneResources(in corev1.ResourceRequirements) corev1.ResourceRequirements {
	return corev1.ResourceRequirements{
		Requests: cloneResourceList(in.Requests),
		Limits:   cloneResourceList(in.Limits),
	}
}

func cloneResourceList(in corev1.ResourceList) corev1.ResourceList {
	if len(in) == 0 {
		return nil
	}
	out := make(corev1.ResourceList, len(in))
	for name, quantity := range in {
		out[name] = quantity.DeepCopy()
	}
	return out
}
