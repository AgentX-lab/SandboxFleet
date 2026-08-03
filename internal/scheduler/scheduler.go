package scheduler

import (
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
}

func New() Scheduler {
	return &memoryScheduler{
		workers:     make(map[WorkerKey]*WorkerState),
		assignments: make(map[types.UID]Assignment),
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

	for _, key := range s.sortedWorkers(req.Namespace, req.Pool) {
		worker := s.workers[key]
		if !worker.Healthy {
			continue
		}
		for _, slotID := range sortedSlotIDs(worker.Slots) {
			slotInfo := worker.Slots[slotID]
			if slotInfo.State != slot.StateFree {
				continue
			}

			slotInfo.State = slot.StateReserved
			slotInfo.SandboxUID = req.SandboxUID
			worker.Slots[slotID] = slotInfo

			assignment := Assignment{
				SandboxUID: req.SandboxUID,
				Namespace:  req.Namespace,
				Name:       req.Name,
				Worker:     key,
				SlotID:     slotID,
			}
			s.assignments[req.SandboxUID] = assignment
			return assignment, nil
		}
	}

	return Assignment{}, ErrNoCapacity
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

	worker, found := s.workers[assignment.Worker]
	if !found {
		s.assignments[assignment.SandboxUID] = assignment
		return nil
	}

	slotInfo, found := worker.Slots[assignment.SlotID]
	if !found {
		return fmt.Errorf("%w: slot %d does not exist", ErrAssignmentConflict, assignment.SlotID)
	}
	if slotInfo.State != slot.StateFree && slotInfo.SandboxUID != assignment.SandboxUID {
		return fmt.Errorf("%w: slot %d belongs to sandbox %q", ErrAssignmentConflict, assignment.SlotID, slotInfo.SandboxUID)
	}

	slotInfo.State = slot.StateReserved
	slotInfo.SandboxUID = assignment.SandboxUID
	worker.Slots[assignment.SlotID] = slotInfo
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
	if worker, workerFound := s.workers[assignment.Worker]; workerFound {
		if slotInfo, slotFound := worker.Slots[assignment.SlotID]; slotFound && slotInfo.SandboxUID == sandboxUID {
			slotInfo.State = slot.StateFree
			slotInfo.SandboxUID = ""
			worker.Slots[assignment.SlotID] = slotInfo
		}
	}
	delete(s.assignments, sandboxUID)
	return nil
}

func (s *memoryScheduler) Get(sandboxUID types.UID) (Assignment, bool) {
	s.lock.RLock()
	defer s.lock.RUnlock()
	assignment, found := s.assignments[sandboxUID]
	return assignment, found
}

func (s *memoryScheduler) sortedWorkers(namespace, pool string) []WorkerKey {
	keys := make([]WorkerKey, 0, len(s.workers))
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

func cloneSlots(source map[int32]slot.Info) map[int32]slot.Info {
	result := make(map[int32]slot.Info, len(source))
	for id, info := range source {
		result[id] = info
	}
	return result
}

func sortedSlotIDs(slots map[int32]slot.Info) []int32 {
	ids := make([]int32, 0, len(slots))
	for id := range slots {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		return ids[i] < ids[j]
	})
	return ids
}
