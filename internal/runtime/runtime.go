package runtime

import (
	"context"
	"errors"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
)

var ErrNotFound = errors.New("runtime object not found")

type SandboxIdentity struct {
	Namespace string
	Name      string
	UID       types.UID
}

type EnvironmentVariable struct {
	Name  string
	Value string
}

type ContainerConfig struct {
	Image   string
	Command []string
	Args    []string
	Env     []EnvironmentVariable
}

type CreateRequest struct {
	Identity  SandboxIdentity
	SlotID    int32
	Resources corev1.ResourceRequirements
	Container ContainerConfig
}

type ID struct {
	Value string
}

type State string

const (
	StateCreated State = "Created"
	StateRunning State = "Running"
	StateStopped State = "Stopped"
	StateUnknown State = "Unknown"
)

type Status struct {
	State    State
	ExitCode int32
	Message  string
}

type Info struct {
	ID       ID
	Identity SandboxIdentity
	SlotID   int32
	Status   Status
}

type Runtime interface {
	Create(ctx context.Context, req CreateRequest) (ID, error)
	Start(ctx context.Context, id ID) error
	Stop(ctx context.Context, id ID) error
	Delete(ctx context.Context, id ID) error
	Status(ctx context.Context, id ID) (Status, error)
	List(ctx context.Context) ([]Info, error)
	Exec(ctx context.Context, id ID, req ExecRequest) (ExecResult, error)
	ReadFile(ctx context.Context, id ID, absPath string) ([]byte, error)
	WriteFile(ctx context.Context, id ID, absPath string, content []byte) error
	ListFiles(ctx context.Context, id ID, absPath string) ([]FileEntry, error)
	FileExists(ctx context.Context, id ID, absPath string) (bool, error)
}

type ExecRequest struct {
	Command []string
	// Timeout is the exec deadline. Zero means the runtime default.
	Timeout time.Duration
}

type ExecResult struct {
	ExitCode int32
	Stdout   string
	Stderr   string
}
