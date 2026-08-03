package sandboxfleet

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	sandboxv1alpha1 "github.com/AgentNaut/SandboxFleet/api/v1alpha1"
	"github.com/AgentNaut/SandboxFleet/internal/worker/httpapi"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	defaultPollInterval = 200 * time.Millisecond
	defaultWorkerPort   = int32(8090)
)

type Client interface {
	CreateSandbox(ctx context.Context, opts CreateOptions) (*sandboxv1alpha1.Sandbox, error)
	GetSandbox(ctx context.Context, namespace, name string) (*sandboxv1alpha1.Sandbox, error)
	WaitSandboxReady(ctx context.Context, namespace, name string) (*sandboxv1alpha1.Sandbox, error)
	DeleteSandbox(ctx context.Context, namespace, name string) error
	WaitSandboxDeleted(ctx context.Context, namespace, name string) error
	OpenSandbox(ctx context.Context, namespace, name string) (*Sandbox, error)
	OpenSandboxReady(ctx context.Context, namespace, name string) (*Sandbox, error)
}

type CreateOptions struct {
	Namespace   string
	Name        string
	PoolRef     string
	SlotProfile string
	Container   sandboxv1alpha1.ContainerSpec
}

type ExecOptions struct {
	Command []string
	// Timeout is the command deadline. Zero uses the Worker default.
	Timeout time.Duration
}

type ExecResult struct {
	ExitCode int32
	Stdout   string
	Stderr   string
}

type SandboxFileEntry struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

type SandboxFailedError struct {
	Namespace string
	Name      string
	Message   string
}

func (e *SandboxFailedError) Error() string {
	return fmt.Sprintf("Sandbox %s/%s failed: %s", e.Namespace, e.Name, e.Message)
}

type ClientOption func(*sdkClient)

// WithWorkerReachability overrides how Worker HTTP endpoints are resolved.
func WithWorkerReachability(reach WorkerReachability) ClientOption {
	return func(c *sdkClient) {
		if reach != nil {
			c.reach = reach
		}
	}
}

type sdkClient struct {
	kubernetes   client.Client
	worker       *httpapi.Client
	reach        WorkerReachability
	workerPort   int32
	pollInterval time.Duration
}

func New(config *rest.Config, opts ...ClientOption) (Client, error) {
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		return nil, fmt.Errorf("register Kubernetes scheme: %w", err)
	}
	if err := sandboxv1alpha1.AddToScheme(scheme); err != nil {
		return nil, fmt.Errorf("register SandboxFleet API: %w", err)
	}
	kubernetesClient, err := client.New(config, client.Options{Scheme: scheme})
	if err != nil {
		return nil, fmt.Errorf("create Kubernetes client: %w", err)
	}
	return NewWithClient(kubernetesClient, opts...), nil
}

func NewWithClient(kubernetesClient client.Client, opts ...ClientOption) Client {
	c := &sdkClient{
		kubernetes:   kubernetesClient,
		worker:       httpapi.NewClient(&http.Client{Timeout: 60 * time.Second}),
		reach:        &directPodIPReachability{client: kubernetesClient},
		workerPort:   defaultWorkerPort,
		pollInterval: defaultPollInterval,
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

func (c *sdkClient) CreateSandbox(ctx context.Context, opts CreateOptions) (*sandboxv1alpha1.Sandbox, error) {
	if opts.Namespace == "" || opts.Name == "" || opts.PoolRef == "" || opts.SlotProfile == "" || opts.Container.Image == "" {
		return nil, errors.New("namespace, name, poolRef, slotProfile, and container image are required")
	}
	sandbox := &sandboxv1alpha1.Sandbox{
		TypeMeta: metav1.TypeMeta{
			APIVersion: sandboxv1alpha1.GroupVersion.String(),
			Kind:       "Sandbox",
		},
		ObjectMeta: metav1.ObjectMeta{Namespace: opts.Namespace, Name: opts.Name},
		Spec: sandboxv1alpha1.SandboxSpec{
			PoolRef:     opts.PoolRef,
			SlotProfile: opts.SlotProfile,
			Container:   opts.Container,
		},
	}
	if err := c.kubernetes.Create(ctx, sandbox); err != nil {
		return nil, err
	}
	return sandbox, nil
}

func (c *sdkClient) GetSandbox(ctx context.Context, namespace, name string) (*sandboxv1alpha1.Sandbox, error) {
	var sandbox sandboxv1alpha1.Sandbox
	if err := c.kubernetes.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, &sandbox); err != nil {
		return nil, err
	}
	return &sandbox, nil
}

func (c *sdkClient) WaitSandboxReady(ctx context.Context, namespace, name string) (*sandboxv1alpha1.Sandbox, error) {
	for {
		sandbox, err := c.GetSandbox(ctx, namespace, name)
		if err != nil {
			return nil, err
		}
		if sandbox.Status.Phase == sandboxv1alpha1.SandboxPhaseFailed {
			return nil, &SandboxFailedError{Namespace: namespace, Name: name, Message: conditionMessage(sandbox.Status.Conditions)}
		}
		ready := meta.FindStatusCondition(sandbox.Status.Conditions, sandboxv1alpha1.ConditionReady)
		if ready != nil && ready.Status == metav1.ConditionTrue {
			return sandbox, nil
		}
		if err := wait(ctx, c.pollInterval); err != nil {
			return nil, err
		}
	}
}

func (c *sdkClient) DeleteSandbox(ctx context.Context, namespace, name string) error {
	sandbox, err := c.GetSandbox(ctx, namespace, name)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	return c.kubernetes.Delete(ctx, sandbox)
}

func (c *sdkClient) WaitSandboxDeleted(ctx context.Context, namespace, name string) error {
	for {
		_, err := c.GetSandbox(ctx, namespace, name)
		if apierrors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if err := wait(ctx, c.pollInterval); err != nil {
			return err
		}
	}
}

func wait(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func conditionMessage(conditions []metav1.Condition) string {
	ready := meta.FindStatusCondition(conditions, sandboxv1alpha1.ConditionReady)
	if ready != nil && ready.Message != "" {
		return ready.Message
	}
	return "the controller could not make progress"
}
