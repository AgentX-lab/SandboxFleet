package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	sandboxv1alpha1 "github.com/AgentNaut/SandboxFleet/api/v1alpha1"
	"github.com/AgentNaut/SandboxFleet/internal/runtime/cri"
	"github.com/AgentNaut/SandboxFleet/internal/worker"
	"github.com/AgentNaut/SandboxFleet/internal/worker/httpapi"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

func main() {
	var (
		name               = flag.String("name", "", "Stable Worker name.")
		namespace          = flag.String("namespace", "", "Worker namespace.")
		pool               = flag.String("pool", "", "SandboxPool name.")
		slots              = flag.Int("slots", 0, "Number of local Slots.")
		runtimeHandler     = flag.String("runtime-handler", "", "CRI runtime handler name configured in local containerd.")
		containerdEndpoint = flag.String("containerd-endpoint", "/run/containerd/containerd.sock", "containerd CRI socket.")
		listenAddress      = flag.String("listen", ":8090", "Worker HTTP API address.")
		cpuRequest         = flag.String("slot-cpu-request", "", "CPU request for each Slot.")
		cpuLimit           = flag.String("slot-cpu-limit", "", "CPU limit for each Slot.")
		memoryRequest      = flag.String("slot-memory-request", "", "Memory request for each Slot.")
		memoryLimit        = flag.String("slot-memory-limit", "", "Memory limit for each Slot.")
	)
	flag.Parse()

	if *name == "" || *namespace == "" || *pool == "" || *slots <= 0 {
		log.Fatal("name, namespace, pool, and a positive slots value are required")
	}
	if *runtimeHandler == "" {
		log.Fatal("runtime-handler is required")
	}
	resources, err := slotResources(*cpuRequest, *cpuLimit, *memoryRequest, *memoryLimit)
	if err != nil {
		log.Fatalf("invalid Slot resources: %v", err)
	}

	runtimeAdapter, err := cri.New(cri.Config{
		Endpoint:       *containerdEndpoint,
		WorkerName:     *name,
		RuntimeHandler: *runtimeHandler,
		Resources:      resources,
	})
	if err != nil {
		log.Fatalf("create CRI runtime: %v", err)
	}
	defer runtimeAdapter.Close()

	manager := worker.NewSlotManager(worker.Config{
		Name:          *name,
		Namespace:     *namespace,
		Pool:          *pool,
		Slots:         int32(*slots),
		SlotResources: resources,
		Runtime: sandboxv1alpha1.RuntimeConfig{
			Backend: sandboxv1alpha1.RuntimeBackendCRI,
			CRI:     &sandboxv1alpha1.CRIRuntimeConfig{RuntimeHandler: *runtimeHandler},
		},
	}, runtimeAdapter)
	if err := manager.Recover(context.Background()); err != nil {
		log.Fatalf("recover Slot state: %v", err)
	}

	server := &http.Server{
		Addr:              *listenAddress,
		Handler:           httpapi.NewServer(manager),
		ReadHeaderTimeout: 5 * time.Second,
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			log.Printf("shut down Worker API: %v", err)
		}
	}()

	log.Printf("Worker %s listening on %s", *name, *listenAddress)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("serve Worker API: %v", err)
	}
}

func slotResources(cpuRequest, cpuLimit, memoryRequest, memoryLimit string) (corev1.ResourceRequirements, error) {
	result := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{},
		Limits:   corev1.ResourceList{},
	}
	values := []struct {
		target corev1.ResourceList
		name   corev1.ResourceName
		value  string
	}{
		{result.Requests, corev1.ResourceCPU, cpuRequest},
		{result.Limits, corev1.ResourceCPU, cpuLimit},
		{result.Requests, corev1.ResourceMemory, memoryRequest},
		{result.Limits, corev1.ResourceMemory, memoryLimit},
	}
	for _, item := range values {
		if item.value == "" {
			continue
		}
		quantity, err := resource.ParseQuantity(item.value)
		if err != nil {
			return corev1.ResourceRequirements{}, fmt.Errorf("%s %q: %w", item.name, item.value, err)
		}
		item.target[item.name] = quantity
	}
	return result, nil
}
