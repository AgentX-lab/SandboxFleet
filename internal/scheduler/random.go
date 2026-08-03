package scheduler

import (
	"context"
	"math/rand"
	"sync"
	"time"
)

// RandomStrategy picks one candidate at random.
type RandomStrategy struct {
	rand *rand.Rand
	mu   sync.Mutex
}

func NewRandomStrategy(source rand.Source) *RandomStrategy {
	if source == nil {
		source = rand.NewSource(time.Now().UnixNano())
	}
	return &RandomStrategy{rand: rand.New(source)}
}

func (s *RandomStrategy) Name() string { return "random" }

func (s *RandomStrategy) Select(_ context.Context, _ AssignRequest, candidates []Candidate) (Candidate, error) {
	if len(candidates) == 0 {
		return Candidate{}, ErrNoCapacity
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return candidates[s.rand.Intn(len(candidates))], nil
}
