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
	"github.com/AgentNaut/SandboxFleet/internal/snapshotter"
	"github.com/AgentNaut/SandboxFleet/internal/worker"
	"github.com/AgentNaut/SandboxFleet/internal/worker/httpapi"
)

func main() {
	var (
		name               = flag.String("name", "", "Stable Worker name.")
		namespace          = flag.String("namespace", "", "Worker namespace.")
		pool               = flag.String("pool", "", "SandboxPool name.")
		runtimeHandler     = flag.String("runtime-handler", "", "CRI runtime handler name configured in local containerd.")
		snapshotterKind    = flag.String("snapshotter", "", "Memory snapshot adapter: gvisor or kata.")
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
	kind := sandboxv1alpha1.SnapshotterKind(*snapshotterKind)
	snap, err := snapshotter.New(kind)
	if err != nil {
		log.Fatal(err)
	}
	if err := snapshotter.SetupCgroupDelegation(); err != nil {
		log.Printf("cgroup delegation: %v (continuing)", err)
	}

	runtimeAdapter, err := cri.New(cri.Config{
		Endpoint:       *containerdEndpoint,
		WorkerName:     *name,
		RuntimeHandler: *runtimeHandler,
		PreDelete:      criPreDelete(snap),
	})
	if err != nil {
		log.Fatalf("create CRI runtime: %v", err)
	}
	defer runtimeAdapter.Close()

	// Snapshot adapters are keyed by the opaque CRI runtimeHandler so restore
	// requests that carry the same handler string resolve to this Worker's adapter.
	snapshots := snapshotter.NewRegistry(map[string]snapshotter.Snapshotter{
		*runtimeHandler: snap,
	})

	// Topology starts empty; Pool controller pushes Slot Specs via PUT /v1/topology.
	manager := worker.NewSlotManager(worker.Config{
		Name:      *name,
		Namespace: *namespace,
		Pool:      *pool,
		Runtime: sandboxv1alpha1.RuntimeConfig{
			Backend: sandboxv1alpha1.RuntimeBackendCRI,
			CRI: &sandboxv1alpha1.CRIRuntimeConfig{
				RuntimeHandler: *runtimeHandler,
				Snapshotter:    kind,
			},
		},
	}, runtimeAdapter, snapshots)
	// Brief retry covers the window where entrypoint saw containerd ready but
	// CRI is still finishing plugin init (common on kata cold start / restart).
	var recoverErr error
	for attempt := 0; attempt < 50; attempt++ {
		recoverErr = manager.Recover(context.Background())
		if recoverErr == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if recoverErr != nil {
		log.Fatalf("recover Slot state: %v", recoverErr)
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

	log.Printf("Worker %s listening on %s (snapshotter=%s)", *name, *listenAddress, kind)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("serve Worker API: %v", err)
	}
}

// criPreDelete wires optional snapshotter cleanup into CRI without importing
// snapshotter details into the cri package (type-assert at the composition root).
func criPreDelete(snap snapshotter.Snapshotter) func(context.Context, string) {
	cleaner, ok := snap.(snapshotter.CRICleanup)
	if !ok {
		return nil
	}
	return cleaner.BestEffortCleanupCRI
}
