package main

import (
	"context"
	"flag"
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
)

func main() {
	var (
		name               = flag.String("name", "", "Stable Worker name.")
		namespace          = flag.String("namespace", "", "Worker namespace.")
		pool               = flag.String("pool", "", "SandboxPool name.")
		runtimeHandler     = flag.String("runtime-handler", "", "CRI runtime handler name configured in local containerd.")
		containerdEndpoint = flag.String("containerd-endpoint", "/run/containerd/containerd.sock", "containerd CRI socket.")
		listenAddress      = flag.String("listen", ":8090", "Worker HTTP API address.")
	)
	flag.Parse()

	if *name == "" || *namespace == "" || *pool == "" {
		log.Fatal("name, namespace, and pool are required")
	}
	if *runtimeHandler == "" {
		log.Fatal("runtime-handler is required")
	}

	runtimeAdapter, err := cri.New(cri.Config{
		Endpoint:       *containerdEndpoint,
		WorkerName:     *name,
		RuntimeHandler: *runtimeHandler,
	})
	if err != nil {
		log.Fatalf("create CRI runtime: %v", err)
	}
	defer runtimeAdapter.Close()

	// Topology starts empty; Pool controller pushes Slot Specs via PUT /v1/topology.
	manager := worker.NewSlotManager(worker.Config{
		Name:      *name,
		Namespace: *namespace,
		Pool:      *pool,
		Runtime: sandboxv1alpha1.RuntimeConfig{
			Backend: sandboxv1alpha1.RuntimeBackendCRI,
			CRI:     &sandboxv1alpha1.CRIRuntimeConfig{RuntimeHandler: *runtimeHandler},
		},
	}, runtimeAdapter)
	if err := manager.Recover(context.Background()); err != nil {
		log.Fatalf("recover Slot state: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	server := &http.Server{
		Addr:              *listenAddress,
		Handler:           httpapi.NewServer(manager),
		ReadHeaderTimeout: 5 * time.Second,
	}
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
