package scheduler

import "context"

// StableStrategy always selects the first filtered candidate.
// Candidates are already ordered by Worker name and Slot ID.
type StableStrategy struct{}

func (StableStrategy) Name() string { return "stable" }

func (StableStrategy) Select(_ context.Context, _ AssignRequest, candidates []Candidate) (Candidate, error) {
	if len(candidates) == 0 {
		return Candidate{}, ErrNoCapacity
	}
	return candidates[0], nil
}
