package cri

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"time"

	sandboxruntime "github.com/AgentNaut/SandboxFleet/internal/runtime"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1"
)

const (
	labelSandboxUID       = "sandboxfleet.io/sandbox-uid"
	labelSandboxNamespace = "sandboxfleet.io/sandbox-namespace"
	labelSandboxName      = "sandboxfleet.io/sandbox-name"
	labelWorker           = "sandboxfleet.io/worker"
	labelSlotID           = "sandboxfleet.io/slot-id"
)

type Config struct {
	Endpoint       string
	WorkerName     string
	RuntimeHandler string
	Resources      corev1.ResourceRequirements
}

type Runtime struct {
	connection *grpc.ClientConn
	runtime    runtimeapi.RuntimeServiceClient
	images     runtimeapi.ImageServiceClient
	config     Config
}

func New(config Config) (*Runtime, error) {
	if config.Endpoint == "" {
		return nil, errors.New("CRI endpoint is required")
	}
	connection, err := grpc.NewClient(
		"passthrough:///cri",
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", config.Endpoint)
		}),
	)
	if err != nil {
		return nil, fmt.Errorf("connect to CRI endpoint: %w", err)
	}
	return &Runtime{
		connection: connection,
		runtime:    runtimeapi.NewRuntimeServiceClient(connection),
		images:     runtimeapi.NewImageServiceClient(connection),
		config:     config,
	}, nil
}

func (r *Runtime) Close() error {
	return r.connection.Close()
}

func (r *Runtime) Create(ctx context.Context, req sandboxruntime.CreateRequest) (sandboxruntime.ID, error) {
	image, err := r.images.PullImage(ctx, &runtimeapi.PullImageRequest{
		Image: &runtimeapi.ImageSpec{Image: req.Container.Image},
	})
	if err != nil {
		return sandboxruntime.ID{}, fmt.Errorf("pull image %q: %w", req.Container.Image, err)
	}

	labels := map[string]string{
		labelSandboxUID:       string(req.Identity.UID),
		labelSandboxNamespace: req.Identity.Namespace,
		labelSandboxName:      req.Identity.Name,
		labelWorker:           r.config.WorkerName,
		labelSlotID:           strconv.FormatInt(int64(req.SlotID), 10),
	}
	podConfig := &runtimeapi.PodSandboxConfig{
		Metadata: &runtimeapi.PodSandboxMetadata{
			Name:      req.Identity.Name,
			Namespace: req.Identity.Namespace,
			Uid:       string(req.Identity.UID),
		},
		Labels: labels,
		Linux:  &runtimeapi.LinuxPodSandboxConfig{},
	}
	pod, err := r.runtime.RunPodSandbox(ctx, &runtimeapi.RunPodSandboxRequest{
		Config:         podConfig,
		RuntimeHandler: r.config.RuntimeHandler,
	})
	if err != nil {
		return sandboxruntime.ID{}, fmt.Errorf("create pod sandbox: %w", err)
	}

	env := make([]*runtimeapi.KeyValue, 0, len(req.Container.Env))
	for _, variable := range req.Container.Env {
		env = append(env, &runtimeapi.KeyValue{Key: variable.Name, Value: []byte(variable.Value)})
	}
	imageReference := image.ImageRef
	if imageReference == "" {
		imageReference = req.Container.Image
	}
	_, err = r.runtime.CreateContainer(ctx, &runtimeapi.CreateContainerRequest{
		PodSandboxId:  pod.PodSandboxId,
		SandboxConfig: podConfig,
		Config: &runtimeapi.ContainerConfig{
			Metadata: &runtimeapi.ContainerMetadata{Name: req.Identity.Name},
			Image:    &runtimeapi.ImageSpec{Image: imageReference},
			Command:  req.Container.Command,
			Args:     req.Container.Args,
			Envs:     env,
			Labels:   labels,
			Linux: &runtimeapi.LinuxContainerConfig{
				Resources: linuxResources(r.config.Resources),
			},
		},
	})
	if err != nil {
		_, _ = r.runtime.StopPodSandbox(ctx, &runtimeapi.StopPodSandboxRequest{PodSandboxId: pod.PodSandboxId})
		_, _ = r.runtime.RemovePodSandbox(ctx, &runtimeapi.RemovePodSandboxRequest{PodSandboxId: pod.PodSandboxId})
		return sandboxruntime.ID{}, fmt.Errorf("create container: %w", err)
	}
	return sandboxruntime.ID{Value: pod.PodSandboxId}, nil
}

func (r *Runtime) Start(ctx context.Context, id sandboxruntime.ID) error {
	containerIDs, err := r.containerIDs(ctx, id)
	if err != nil {
		return err
	}
	for _, containerID := range containerIDs {
		if _, err := r.runtime.StartContainer(ctx, &runtimeapi.StartContainerRequest{ContainerId: containerID}); err != nil {
			return fmt.Errorf("start container: %w", err)
		}
	}
	return nil
}

func (r *Runtime) Stop(ctx context.Context, id sandboxruntime.ID) error {
	containerIDs, err := r.containerIDs(ctx, id)
	if err != nil && !isNotFound(err) {
		return err
	}
	for _, containerID := range containerIDs {
		if _, err := r.runtime.StopContainer(ctx, &runtimeapi.StopContainerRequest{
			ContainerId: containerID,
			Timeout:     int64((10 * time.Second).Seconds()),
		}); err != nil && !isNotFound(err) {
			return fmt.Errorf("stop container: %w", err)
		}
	}
	return nil
}

func (r *Runtime) Delete(ctx context.Context, id sandboxruntime.ID) error {
	containerIDs, err := r.containerIDs(ctx, id)
	if err != nil && !isNotFound(err) {
		return err
	}
	for _, containerID := range containerIDs {
		_, stopErr := r.runtime.StopContainer(ctx, &runtimeapi.StopContainerRequest{ContainerId: containerID, Timeout: 10})
		if stopErr != nil && !isNotFound(stopErr) {
			return fmt.Errorf("stop container before removal: %w", stopErr)
		}
		if _, err := r.runtime.RemoveContainer(ctx, &runtimeapi.RemoveContainerRequest{ContainerId: containerID}); err != nil && !isNotFound(err) {
			return fmt.Errorf("remove container: %w", err)
		}
	}
	if _, err := r.runtime.StopPodSandbox(ctx, &runtimeapi.StopPodSandboxRequest{PodSandboxId: id.Value}); err != nil && !isNotFound(err) {
		return fmt.Errorf("stop pod sandbox: %w", err)
	}
	if _, err := r.runtime.RemovePodSandbox(ctx, &runtimeapi.RemovePodSandboxRequest{PodSandboxId: id.Value}); err != nil && !isNotFound(err) {
		return fmt.Errorf("remove pod sandbox: %w", err)
	}
	return nil
}

func (r *Runtime) Status(ctx context.Context, id sandboxruntime.ID) (sandboxruntime.Status, error) {
	containerIDs, err := r.containerIDs(ctx, id)
	if err != nil {
		return sandboxruntime.Status{}, err
	}
	response, err := r.runtime.ContainerStatus(ctx, &runtimeapi.ContainerStatusRequest{ContainerId: containerIDs[0]})
	if err != nil {
		return sandboxruntime.Status{}, fmt.Errorf("get container status: %w", err)
	}
	return containerStatus(response.Status), nil
}

func (r *Runtime) List(ctx context.Context) ([]sandboxruntime.Info, error) {
	response, err := r.runtime.ListPodSandbox(ctx, &runtimeapi.ListPodSandboxRequest{
		Filter: &runtimeapi.PodSandboxFilter{LabelSelector: map[string]string{labelWorker: r.config.WorkerName}},
	})
	if err != nil {
		return nil, fmt.Errorf("list pod sandboxes: %w", err)
	}

	result := make([]sandboxruntime.Info, 0, len(response.Items))
	for _, item := range response.Items {
		slotID, err := strconv.ParseInt(item.Labels[labelSlotID], 10, 32)
		if err != nil {
			return nil, fmt.Errorf("runtime %q has invalid slot label: %w", item.Id, err)
		}
		info := sandboxruntime.Info{
			ID: sandboxruntime.ID{Value: item.Id},
			Identity: sandboxruntime.SandboxIdentity{
				Namespace: item.Labels[labelSandboxNamespace],
				Name:      item.Labels[labelSandboxName],
				UID:       types.UID(item.Labels[labelSandboxUID]),
			},
			SlotID: int32(slotID),
			Status: sandboxruntime.Status{State: sandboxruntime.StateCreated},
		}
		if currentStatus, statusErr := r.Status(ctx, info.ID); statusErr == nil {
			info.Status = currentStatus
		}
		result = append(result, info)
	}
	return result, nil
}

func (r *Runtime) Exec(ctx context.Context, id sandboxruntime.ID, req sandboxruntime.ExecRequest) (sandboxruntime.ExecResult, error) {
	if len(req.Command) == 0 {
		return sandboxruntime.ExecResult{}, errors.New("exec command is required")
	}
	containerIDs, err := r.containerIDs(ctx, id)
	if err != nil {
		return sandboxruntime.ExecResult{}, err
	}
	timeout := int64(req.Timeout.Seconds())
	if req.Timeout <= 0 {
		timeout = 30
	}
	response, err := r.runtime.ExecSync(ctx, &runtimeapi.ExecSyncRequest{
		ContainerId: containerIDs[0],
		Cmd:         req.Command,
		Timeout:     timeout,
	})
	if err != nil {
		return sandboxruntime.ExecResult{}, fmt.Errorf("exec in container: %w", err)
	}
	return sandboxruntime.ExecResult{
		ExitCode: response.ExitCode,
		Stdout:   string(response.Stdout),
		Stderr:   string(response.Stderr),
	}, nil
}

func (r *Runtime) containerIDs(ctx context.Context, id sandboxruntime.ID) ([]string, error) {
	response, err := r.runtime.ListContainers(ctx, &runtimeapi.ListContainersRequest{
		Filter: &runtimeapi.ContainerFilter{PodSandboxId: id.Value},
	})
	if err != nil {
		return nil, fmt.Errorf("list containers: %w", err)
	}
	if len(response.Containers) == 0 {
		return nil, fmt.Errorf("%w: sandbox container", sandboxruntime.ErrNotFound)
	}
	result := make([]string, 0, len(response.Containers))
	for _, container := range response.Containers {
		result = append(result, container.Id)
	}
	return result, nil
}

func linuxResources(resources corev1.ResourceRequirements) *runtimeapi.LinuxContainerResources {
	result := &runtimeapi.LinuxContainerResources{}
	if cpu, found := resources.Requests[corev1.ResourceCPU]; found {
		shares := cpu.MilliValue() * 1024 / 1000
		if shares < 2 {
			shares = 2
		}
		result.CpuShares = shares
	}
	if cpu, found := resources.Limits[corev1.ResourceCPU]; found {
		result.CpuPeriod = 100_000
		result.CpuQuota = cpu.MilliValue() * result.CpuPeriod / 1000
	}
	if memory, found := resources.Limits[corev1.ResourceMemory]; found {
		result.MemoryLimitInBytes = memory.Value()
	}
	return result
}

func containerStatus(current *runtimeapi.ContainerStatus) sandboxruntime.Status {
	if current == nil {
		return sandboxruntime.Status{State: sandboxruntime.StateUnknown}
	}
	result := sandboxruntime.Status{ExitCode: current.ExitCode, Message: current.Message}
	switch current.State {
	case runtimeapi.ContainerState_CONTAINER_CREATED:
		result.State = sandboxruntime.StateCreated
	case runtimeapi.ContainerState_CONTAINER_RUNNING:
		result.State = sandboxruntime.StateRunning
	case runtimeapi.ContainerState_CONTAINER_EXITED:
		result.State = sandboxruntime.StateStopped
	default:
		result.State = sandboxruntime.StateUnknown
	}
	return result
}

func isNotFound(err error) bool {
	return errors.Is(err, sandboxruntime.ErrNotFound) || status.Code(err) == codes.NotFound
}
