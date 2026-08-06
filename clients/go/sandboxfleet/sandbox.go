package sandboxfleet

import (
	"context"
	"errors"
	"fmt"

	sandboxv1alpha1 "github.com/AgentNaut/SandboxFleet/api/v1alpha1"
	"github.com/AgentNaut/SandboxFleet/internal/workerapi"
)

// Sandbox is an open session to a Running Sandbox, similar to agent-sandbox's handle.
// Connectivity is established in OpenSandbox and reused by Exec/file methods.
type Sandbox struct {
	client   *sdkClient
	object   *sandboxv1alpha1.Sandbox
	endpoint string
	cleanup  func()
}

func (s *Sandbox) Namespace() string { return s.object.Namespace }
func (s *Sandbox) Name() string      { return s.object.Name }

// Object returns a copy of the Sandbox CR observed at Open time.
func (s *Sandbox) Object() *sandboxv1alpha1.Sandbox {
	return s.object.DeepCopy()
}

// Close releases session resources (for example a port-forward). It does not
// delete the Sandbox CR.
func (s *Sandbox) Close() {
	if s == nil {
		return
	}
	if s.cleanup != nil {
		s.cleanup()
		s.cleanup = nil
	}
}

func (s *Sandbox) Exec(ctx context.Context, opts ExecOptions) (*ExecResult, error) {
	if s == nil || s.client == nil {
		return nil, errors.New("sandbox session is closed")
	}
	if len(opts.Command) == 0 {
		return nil, errors.New("command is required")
	}
	req := workerapi.ExecSandboxRequest{
		SlotID:   s.object.Status.Assignment.SlotID,
		Identity: workerapi.SandboxIdentity{Namespace: s.object.Namespace, Name: s.object.Name, UID: s.object.UID},
		Command:  append([]string(nil), opts.Command...),
	}
	if opts.Timeout > 0 {
		req.TimeoutSeconds = int64(opts.Timeout.Seconds())
	}
	result, err := s.client.worker.ExecSandbox(ctx, s.endpoint, req)
	if err != nil {
		return nil, fmt.Errorf("exec on Worker: %w", err)
	}
	return &ExecResult{ExitCode: result.ExitCode, Stdout: result.Stdout, Stderr: result.Stderr}, nil
}

func (s *Sandbox) WriteSandboxFile(ctx context.Context, path string, content []byte) error {
	if s == nil || s.client == nil {
		return errors.New("sandbox session is closed")
	}
	req := workerapi.SandboxFileRequest{
		SlotID:   s.object.Status.Assignment.SlotID,
		Identity: workerapi.SandboxIdentity{Namespace: s.object.Namespace, Name: s.object.Name, UID: s.object.UID},
		Path:     path,
	}
	if err := s.client.worker.WriteSandboxFile(ctx, s.endpoint, req, content); err != nil {
		return fmt.Errorf("write on Worker: %w", err)
	}
	return nil
}

func (s *Sandbox) ReadSandboxFile(ctx context.Context, path string) ([]byte, error) {
	if s == nil || s.client == nil {
		return nil, errors.New("sandbox session is closed")
	}
	req := workerapi.SandboxFileRequest{
		SlotID:   s.object.Status.Assignment.SlotID,
		Identity: workerapi.SandboxIdentity{Namespace: s.object.Namespace, Name: s.object.Name, UID: s.object.UID},
		Path:     path,
	}
	data, err := s.client.worker.ReadSandboxFile(ctx, s.endpoint, req)
	if err != nil {
		return nil, fmt.Errorf("read on Worker: %w", err)
	}
	return data, nil
}

func (s *Sandbox) ListSandboxFiles(ctx context.Context, path string) ([]SandboxFileEntry, error) {
	if s == nil || s.client == nil {
		return nil, errors.New("sandbox session is closed")
	}
	req := workerapi.SandboxFileRequest{
		SlotID:   s.object.Status.Assignment.SlotID,
		Identity: workerapi.SandboxIdentity{Namespace: s.object.Namespace, Name: s.object.Name, UID: s.object.UID},
		Path:     path,
	}
	entries, err := s.client.worker.ListSandboxFiles(ctx, s.endpoint, req)
	if err != nil {
		return nil, fmt.Errorf("list on Worker: %w", err)
	}
	result := make([]SandboxFileEntry, 0, len(entries))
	for _, entry := range entries {
		result = append(result, SandboxFileEntry{Name: entry.Name, Type: entry.Type})
	}
	return result, nil
}

func (s *Sandbox) ExistsSandboxFile(ctx context.Context, path string) (bool, error) {
	if s == nil || s.client == nil {
		return false, errors.New("sandbox session is closed")
	}
	req := workerapi.SandboxFileRequest{
		SlotID:   s.object.Status.Assignment.SlotID,
		Identity: workerapi.SandboxIdentity{Namespace: s.object.Namespace, Name: s.object.Name, UID: s.object.UID},
		Path:     path,
	}
	exists, err := s.client.worker.ExistsSandboxFile(ctx, s.endpoint, req)
	if err != nil {
		return false, fmt.Errorf("exists on Worker: %w", err)
	}
	return exists, nil
}

func (c *sdkClient) OpenSandbox(ctx context.Context, namespace, name string) (*Sandbox, error) {
	sandbox, err := c.GetSandbox(ctx, namespace, name)
	if err != nil {
		return nil, err
	}
	if sandbox.Status.Assignment == nil {
		return nil, fmt.Errorf("Sandbox %s/%s has no assignment", namespace, name)
	}
	if sandbox.Status.Phase != sandboxv1alpha1.SandboxPhaseRunning {
		return nil, fmt.Errorf("Sandbox %s/%s is not Running (phase=%s)", namespace, name, sandbox.Status.Phase)
	}
	endpoint, cleanup, err := c.reach.ReachWorker(ctx, sandbox.Namespace, sandbox.Status.Assignment.Worker, c.workerPort)
	if err != nil {
		return nil, err
	}
	if cleanup == nil {
		cleanup = func() {}
	}
	return &Sandbox{
		client:   c,
		object:   sandbox,
		endpoint: endpoint,
		cleanup:  cleanup,
	}, nil
}

// OpenSandboxReady waits until Ready, then opens a session.
func (c *sdkClient) OpenSandboxReady(ctx context.Context, namespace, name string) (*Sandbox, error) {
	if _, err := c.WaitSandboxReady(ctx, namespace, name); err != nil {
		return nil, err
	}
	return c.OpenSandbox(ctx, namespace, name)
}
