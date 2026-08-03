package worker

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	sandboxruntime "github.com/AgentNaut/SandboxFleet/internal/runtime"
	"github.com/AgentNaut/SandboxFleet/internal/slot"
)

type SlotManager struct {
	config  Config
	runtime sandboxruntime.Runtime
	slots   map[int32]*managedSlot
}

type managedSlot struct {
	lock       sync.Mutex
	id         int32
	state      slot.State
	sandbox    SandboxIdentity
	runtimeRef *sandboxruntime.ID
}

func NewSlotManager(config Config, runtime sandboxruntime.Runtime) *SlotManager {
	slots := make(map[int32]*managedSlot, config.Slots)
	for id := int32(0); id < config.Slots; id++ {
		slots[id] = &managedSlot{id: id, state: slot.StateFree}
	}
	return &SlotManager{config: config, runtime: runtime, slots: slots}
}

func (m *SlotManager) ReserveSlot(_ context.Context, ref SandboxSlotRef) error {
	if err := validateIdentity(ref.Identity); err != nil {
		return err
	}
	current, err := m.getSlot(ref.SlotID)
	if err != nil {
		return err
	}

	current.lock.Lock()
	defer current.lock.Unlock()

	if current.state == slot.StateFree {
		current.state = slot.StateReserved
		current.sandbox = ref.Identity
		return nil
	}
	if current.sandbox.UID == ref.Identity.UID {
		return nil
	}
	return fmt.Errorf("%w: slot %d is owned by sandbox %q", ErrSlotConflict, ref.SlotID, current.sandbox.UID)
}

func (m *SlotManager) StartSandbox(ctx context.Context, req StartSandboxRequest) error {
	if err := validateIdentity(req.Identity); err != nil {
		return err
	}
	current, err := m.getSlot(req.SlotID)
	if err != nil {
		return err
	}

	current.lock.Lock()
	defer current.lock.Unlock()

	if current.sandbox.UID != req.Identity.UID {
		return fmt.Errorf("%w: slot %d is not reserved by sandbox %q", ErrSlotConflict, req.SlotID, req.Identity.UID)
	}
	if current.state == slot.StateRunning && current.runtimeRef != nil {
		status, statusErr := m.runtime.Status(ctx, *current.runtimeRef)
		if statusErr == nil && status.State == sandboxruntime.StateRunning {
			return nil
		}
		if statusErr != nil && !errors.Is(statusErr, sandboxruntime.ErrNotFound) {
			return fmt.Errorf("check running runtime: %w", statusErr)
		}
		if err := m.runtime.Delete(ctx, *current.runtimeRef); err != nil {
			current.state = slot.StateFailed
			return fmt.Errorf("delete stale runtime: %w", err)
		}
		current.runtimeRef = nil
		current.state = slot.StateReserved
	} else if current.state == slot.StateRunning {
		current.state = slot.StateReserved
	}
	if current.state == slot.StateStarting && current.runtimeRef != nil {
		if err := m.runtime.Start(ctx, *current.runtimeRef); err != nil {
			cleanupErr := m.runtime.Delete(ctx, *current.runtimeRef)
			current.runtimeRef = nil
			current.state = slot.StateReserved
			if cleanupErr != nil {
				current.state = slot.StateFailed
				return fmt.Errorf("resume runtime start: %v; clean failed runtime: %w", err, cleanupErr)
			}
			return fmt.Errorf("resume runtime start: %w", err)
		}
		current.state = slot.StateRunning
		return nil
	}
	if current.state != slot.StateReserved {
		return fmt.Errorf("slot %d cannot start from state %q", req.SlotID, current.state)
	}

	container, err := containerConfig(req)
	if err != nil {
		return err
	}

	current.state = slot.StateStarting
	runtimeID, err := m.runtime.Create(ctx, sandboxruntime.CreateRequest{
		Identity: sandboxruntime.SandboxIdentity{
			Namespace: req.Identity.Namespace,
			Name:      req.Identity.Name,
			UID:       req.Identity.UID,
		},
		SlotID:    req.SlotID,
		Container: container,
	})
	if err != nil {
		current.state = slot.StateReserved
		return fmt.Errorf("create runtime: %w", err)
	}
	current.runtimeRef = &runtimeID

	if err := m.runtime.Start(ctx, runtimeID); err != nil {
		cleanupErr := m.runtime.Delete(ctx, runtimeID)
		current.runtimeRef = nil
		current.state = slot.StateReserved
		if cleanupErr != nil {
			current.state = slot.StateFailed
			return fmt.Errorf("start runtime: %v; clean failed runtime: %w", err, cleanupErr)
		}
		return fmt.Errorf("start runtime: %w", err)
	}

	current.state = slot.StateRunning
	return nil
}

func (m *SlotManager) StopSandbox(ctx context.Context, ref SandboxSlotRef) error {
	if err := validateIdentity(ref.Identity); err != nil {
		return err
	}
	current, err := m.getSlot(ref.SlotID)
	if err != nil {
		return err
	}

	current.lock.Lock()
	defer current.lock.Unlock()

	if current.state == slot.StateFree {
		return nil
	}
	if current.sandbox.UID != ref.Identity.UID {
		return fmt.Errorf("%w: slot %d is owned by sandbox %q", ErrSlotConflict, ref.SlotID, current.sandbox.UID)
	}
	if current.state == slot.StateStopping || current.state == slot.StateCleaning {
		return nil
	}

	current.state = slot.StateStopping
	if current.runtimeRef == nil {
		return nil
	}
	if err := m.runtime.Stop(ctx, *current.runtimeRef); err != nil {
		current.state = slot.StateFailed
		return fmt.Errorf("stop runtime: %w", err)
	}
	return nil
}

func (m *SlotManager) ReleaseSlot(ctx context.Context, ref SandboxSlotRef) error {
	if err := validateIdentity(ref.Identity); err != nil {
		return err
	}
	current, err := m.getSlot(ref.SlotID)
	if err != nil {
		return err
	}

	current.lock.Lock()
	defer current.lock.Unlock()

	if current.state == slot.StateFree {
		return nil
	}
	if current.sandbox.UID != ref.Identity.UID {
		return fmt.Errorf("%w: slot %d is owned by sandbox %q", ErrSlotConflict, ref.SlotID, current.sandbox.UID)
	}

	current.state = slot.StateCleaning
	if current.runtimeRef != nil {
		if err := m.runtime.Delete(ctx, *current.runtimeRef); err != nil {
			current.state = slot.StateFailed
			return fmt.Errorf("delete runtime: %w", err)
		}
	}

	current.state = slot.StateFree
	current.sandbox = SandboxIdentity{}
	current.runtimeRef = nil
	return nil
}

func (m *SlotManager) GetSandbox(_ context.Context, ref SandboxSlotRef) (SandboxInfo, error) {
	if err := validateIdentity(ref.Identity); err != nil {
		return SandboxInfo{}, err
	}
	current, err := m.getSlot(ref.SlotID)
	if err != nil {
		return SandboxInfo{}, err
	}

	current.lock.Lock()
	defer current.lock.Unlock()
	if current.state == slot.StateFree || current.sandbox.UID != ref.Identity.UID {
		return SandboxInfo{}, ErrSandboxNotFound
	}
	return SandboxInfo{Identity: current.sandbox, SlotID: current.id, State: current.state}, nil
}

func (m *SlotManager) ExecSandbox(ctx context.Context, req ExecSandboxRequest) (ExecSandboxResult, error) {
	if err := validateIdentity(req.Identity); err != nil {
		return ExecSandboxResult{}, err
	}
	if len(req.Command) == 0 {
		return ExecSandboxResult{}, fmt.Errorf("%w: command is required", ErrInvalidRequest)
	}
	current, err := m.getSlot(req.SlotID)
	if err != nil {
		return ExecSandboxResult{}, err
	}

	current.lock.Lock()
	defer current.lock.Unlock()

	if current.state == slot.StateFree || current.sandbox.UID != req.Identity.UID {
		return ExecSandboxResult{}, ErrSandboxNotFound
	}
	if current.state != slot.StateRunning || current.runtimeRef == nil {
		return ExecSandboxResult{}, fmt.Errorf("%w: sandbox is not running", ErrInvalidRequest)
	}

	timeout := time.Duration(0)
	if req.TimeoutSeconds > 0 {
		timeout = time.Duration(req.TimeoutSeconds) * time.Second
	}
	result, err := m.runtime.Exec(ctx, *current.runtimeRef, sandboxruntime.ExecRequest{
		Command: append([]string(nil), req.Command...),
		Timeout: timeout,
	})
	if err != nil {
		return ExecSandboxResult{}, fmt.Errorf("exec sandbox: %w", err)
	}
	return ExecSandboxResult{
		ExitCode: result.ExitCode,
		Stdout:   result.Stdout,
		Stderr:   result.Stderr,
	}, nil
}

func (m *SlotManager) ListSlots(_ context.Context) []slot.Info {
	ids := make([]int32, 0, len(m.slots))
	for id := range m.slots {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })

	result := make([]slot.Info, 0, len(ids))
	for _, id := range ids {
		current := m.slots[id]
		current.lock.Lock()
		result = append(result, slot.Info{ID: id, State: current.state, SandboxUID: current.sandbox.UID})
		current.lock.Unlock()
	}
	return result
}

func (m *SlotManager) Recover(ctx context.Context) error {
	objects, err := m.runtime.List(ctx)
	if err != nil {
		return fmt.Errorf("list runtime objects: %w", err)
	}

	for _, object := range objects {
		current, found := m.slots[object.SlotID]
		if !found {
			return fmt.Errorf("runtime %q references unknown slot %d", object.ID.Value, object.SlotID)
		}

		current.lock.Lock()
		if current.state != slot.StateFree {
			current.state = slot.StateFailed
			current.lock.Unlock()
			return fmt.Errorf("multiple runtime objects reference slot %d", object.SlotID)
		}
		current.sandbox = SandboxIdentity{
			Namespace: object.Identity.Namespace,
			Name:      object.Identity.Name,
			UID:       object.Identity.UID,
		}
		runtimeID := object.ID
		current.runtimeRef = &runtimeID
		current.state = runtimeSlotState(object.Status.State)
		current.lock.Unlock()
	}
	return nil
}

func (m *SlotManager) getSlot(id int32) (*managedSlot, error) {
	current, found := m.slots[id]
	if !found {
		return nil, fmt.Errorf("%w: %d", ErrSlotNotFound, id)
	}
	return current, nil
}

func containerConfig(req StartSandboxRequest) (sandboxruntime.ContainerConfig, error) {
	env := make([]sandboxruntime.EnvironmentVariable, 0, len(req.Container.Env))
	for _, variable := range req.Container.Env {
		if variable.ValueFrom != nil {
			return sandboxruntime.ContainerConfig{}, fmt.Errorf("%w: environment variable %q uses unsupported valueFrom", ErrInvalidRequest, variable.Name)
		}
		env = append(env, sandboxruntime.EnvironmentVariable{Name: variable.Name, Value: variable.Value})
	}
	return sandboxruntime.ContainerConfig{
		Image:   req.Container.Image,
		Command: append([]string(nil), req.Container.Command...),
		Args:    append([]string(nil), req.Container.Args...),
		Env:     env,
	}, nil
}

func runtimeSlotState(state sandboxruntime.State) slot.State {
	switch state {
	case sandboxruntime.StateCreated:
		return slot.StateStarting
	case sandboxruntime.StateRunning:
		return slot.StateRunning
	case sandboxruntime.StateStopped:
		return slot.StateStopping
	default:
		return slot.StateFailed
	}
}

func validateIdentity(identity SandboxIdentity) error {
	if identity.Namespace == "" || identity.Name == "" || identity.UID == "" {
		return fmt.Errorf("%w: namespace, name, and UID are required", ErrInvalidRequest)
	}
	return nil
}
