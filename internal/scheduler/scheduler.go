package scheduler

import (
	"context"
	"fmt"
	"sort"
	"sync"

	"github.com/AgentNaut/SandboxFleet/internal/slot"
	"k8s.io/apimachinery/pkg/types"
)

type memoryScheduler struct {
	lock        sync.RWMutex
	workers     map[WorkerKey]*WorkerState
	assignments map[types.UID]Assignment
	strategy    Strategy
}

func New(strategy Strategy) Scheduler {
	if strategy == nil {
		strategy = BinPackStrategy{}
	}
	return &memoryScheduler{
		workers:     make(map[WorkerKey]*WorkerState),
		assignments: make(map[types.UID]Assignment),
		strategy:    strategy,
	}
}

func (s *memoryScheduler) UpdateWorker(state WorkerState) {
	s.lock.Lock()
	defer s.lock.Unlock()

	copy := state
	copy.Slots = cloneSlots(state.Slots)

	for sandboxUID, assignment := range s.assignments {
		if assignment.Worker != state.Key {
			continue
		}
		slotInfo, found := copy.Slots[assignment.SlotID]
		if found && slotInfo.State == slot.StateFree {
			slotInfo.State = slot.StateReserved
			slotInfo.SandboxUID = sandboxUID
			copy.Slots[assignment.SlotID] = slotInfo
		}
	}
	s.workers[state.Key] = &copy
}

func (s *memoryScheduler) RemoveWorker(key WorkerKey) {
	s.lock.Lock()
	defer s.lock.Unlock()
	delete(s.workers, key)
}

func (s *memoryScheduler) Assign(req AssignRequest) (Assignment, error) {
	s.lock.Lock()
	defer s.lock.Unlock()

	if assignment, found := s.assignments[req.SandboxUID]; found {
		return assignment, nil
	}
	if req.SlotProfile == "" {
		return Assignment{}, ErrInvalidSlotProfile
	}

	candidates := make([]Candidate, 0)
	for _, key := range s.sortedWorkers(req.Namespace, req.Pool) {
		worker := s.workers[key]
		if !worker.Healthy {
			continue
		}
		freeMatching, busy := countWorkerSlots(worker.Slots, req.SlotProfile)
		for _, slotID := range sortedSlotIDs(worker.Slots) {
			slotInfo := worker.Slots[slotID]
			if slotInfo.State != slot.StateFree {
				continue
			}
			if slotInfo.Profile != req.SlotProfile {
				continue
			}
			candidates = append(candidates, Candidate{
				Worker:    key,
				SlotID:    slotID,
				Profile:   req.SlotProfile,
				FreeSlots: freeMatching,
				BusySlots: busy,
			})
		}
	}
	if len(candidates) == 0 {
		return Assignment{}, ErrNoCapacity
	}

	selected, err := s.strategy.Select(context.Background(), req, candidates)
	if err != nil {
		return Assignment{}, err
	}

	worker := s.workers[selected.Worker]
	slotInfo := worker.Slots[selected.SlotID]
	slotInfo.State = slot.StateReserved
	slotInfo.SandboxUID = req.SandboxUID
	worker.Slots[selected.SlotID] = slotInfo

	assignment := Assignment{
		SandboxUID:  req.SandboxUID,
		Namespace:   req.Namespace,
		Name:        req.Name,
		Worker:      selected.Worker,
		SlotID:      selected.SlotID,
		SlotProfile: req.SlotProfile,
	}
	s.assignments[req.SandboxUID] = assignment
	return assignment, nil
}

func (s *memoryScheduler) Restore(assignment Assignment) error {
	s.lock.Lock()
	defer s.lock.Unlock()

	if current, found := s.assignments[assignment.SandboxUID]; found {
		if current == assignment {
			return nil
		}
		return fmt.Errorf("%w: sandbox %q already has another assignment", ErrAssignmentConflict, assignment.SandboxUID)
	}

	if worker, found := s.workers[assignment.Worker]; found {
		slotInfo, slotFound := worker.Slots[assignment.SlotID]
		if slotFound {
			if slotInfo.State != slot.StateFree && slotInfo.SandboxUID != assignment.SandboxUID {
				return fmt.Errorf("%w: slot %d on %q is owned by %q", ErrAssignmentConflict, assignment.SlotID, assignment.Worker.Name, slotInfo.SandboxUID)
			}
			slotInfo.State = slot.StateReserved
			slotInfo.SandboxUID = assignment.SandboxUID
			if assignment.SlotProfile != "" {
				slotInfo.Profile = assignment.SlotProfile
			}
			worker.Slots[assignment.SlotID] = slotInfo
		}
	}
	s.assignments[assignment.SandboxUID] = assignment
	return nil
}

func (s *memoryScheduler) Release(sandboxUID types.UID) error {
	s.lock.Lock()
	defer s.lock.Unlock()

	assignment, found := s.assignments[sandboxUID]
	if !found {
		return nil
	}
	delete(s.assignments, sandboxUID)

	worker, found := s.workers[assignment.Worker]
	if !found {
		return nil
	}
	slotInfo, found := worker.Slots[assignment.SlotID]
	if !found {
		return nil
	}
	if slotInfo.SandboxUID != sandboxUID {
		return nil
	}
	slotInfo.State = slot.StateFree
	slotInfo.SandboxUID = ""
	worker.Slots[assignment.SlotID] = slotInfo
	return nil
}

func (s *memoryScheduler) Get(sandboxUID types.UID) (Assignment, bool) {
	s.lock.RLock()
	defer s.lock.RUnlock()
	assignment, found := s.assignments[sandboxUID]
	return assignment, found
}

func (s *memoryScheduler) sortedWorkers(namespace, pool string) []WorkerKey {
	keys := make([]WorkerKey, 0)
	for key := range s.workers {
		if key.Namespace == namespace && key.Pool == pool {
			keys = append(keys, key)
		}
	}
	sort.Slice(keys, func(i, j int) bool {
		return keys[i].Name < keys[j].Name
	})
	return keys
}

func sortedSlotIDs(slots map[int32]slot.Info) []int32 {
	ids := make([]int32, 0, len(slots))
	for id := range slots {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

func countWorkerSlots(slots map[int32]slot.Info, profile string) (freeMatching, busy int) {
	for _, info := range slots {
		if info.State != slot.StateFree {
			busy++
			continue
		}
		if info.Profile == profile {
			freeMatching++
		}
	}
	return freeMatching, busy
}

func cloneSlots(in map[int32]slot.Info) map[int32]slot.Info {
	out := make(map[int32]slot.Info, len(in))
	for id, info := range in {
		out[id] = info
	}
	return out
}
