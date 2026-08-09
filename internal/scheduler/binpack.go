package scheduler

import "context"

// BinPackStrategy packs onto Workers that already hold sandboxes before
// opening a new Worker. Among empty Workers it uses stable name/slot order
// so capacity drains predictably (e.g. primary before secondary by name).
type BinPackStrategy struct{}

func (BinPackStrategy) Name() string { return "binpack" }

func (BinPackStrategy) Select(_ context.Context, _ AssignRequest, candidates []Candidate) (Candidate, error) {
	if len(candidates) == 0 {
		return Candidate{}, ErrNoCapacity
	}
	best := candidates[0]
	for _, c := range candidates[1:] {
		if binPackBetter(c, best) {
			best = c
		}
	}
	return best, nil
}

// binPackBetter reports whether a should be preferred over b.
func binPackBetter(a, b Candidate) bool {
	aBusy := a.BusySlots > 0
	bBusy := b.BusySlots > 0
	if aBusy != bBusy {
		// Continue packing onto an already-used Worker.
		return aBusy
	}
	if aBusy && a.FreeSlots != b.FreeSlots {
		// Among in-use Workers, prefer the tighter fit.
		return a.FreeSlots < b.FreeSlots
	}
	if a.Worker.Name != b.Worker.Name {
		return a.Worker.Name < b.Worker.Name
	}
	return a.SlotID < b.SlotID
}
