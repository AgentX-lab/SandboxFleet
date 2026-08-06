package controller

import (
	"context"
	"fmt"

	sandboxv1alpha1 "github.com/AgentNaut/SandboxFleet/api/v1alpha1"
	"github.com/AgentNaut/SandboxFleet/internal/worker"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
)

// startFromSnapshot restores a child Sandbox from a Ready SandboxSnapshot.
//
//  1. Load the SandboxSnapshot named by spec.fromSnapshot
//  2. Require phase=Ready and a storagePath (bytes already in object storage)
//  3. Load Pool credentials
//  4. Ask the Worker to download + LoadSnapshot into this Slot
func (r *SandboxReconciler) startFromSnapshot(
	ctx context.Context,
	sandbox *sandboxv1alpha1.Sandbox,
	endpoint string,
	ref worker.SandboxSlotRef,
) error {
	var snap sandboxv1alpha1.SandboxSnapshot
	if err := r.Get(ctx, types.NamespacedName{Namespace: sandbox.Namespace, Name: sandbox.Spec.FromSnapshot}, &snap); err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("fromSnapshot %q not found", sandbox.Spec.FromSnapshot)
		}
		return fmt.Errorf("get SandboxSnapshot %q: %w", sandbox.Spec.FromSnapshot, err)
	}
	if snap.Status.Phase != sandboxv1alpha1.SandboxSnapshotPhaseReady {
		return fmt.Errorf("SandboxSnapshot %q is %q, want Ready", snap.Name, snap.Status.Phase)
	}
	if snap.Status.StoragePath == "" {
		return fmt.Errorf("SandboxSnapshot %q has empty storagePath", snap.Name)
	}
	if snap.Spec.Pool != sandbox.Spec.PoolRef {
		return fmt.Errorf("SandboxSnapshot %q pool %q does not match Sandbox poolRef %q", snap.Name, snap.Spec.Pool, sandbox.Spec.PoolRef)
	}

	var pool sandboxv1alpha1.SandboxPool
	if err := r.Get(ctx, types.NamespacedName{Namespace: sandbox.Namespace, Name: sandbox.Spec.PoolRef}, &pool); err != nil {
		return fmt.Errorf("get SandboxPool %q: %w", sandbox.Spec.PoolRef, err)
	}
	storage, err := snapshotStorageConfig(ctx, r.Client, &pool)
	if err != nil {
		return err
	}

	handler := snap.Status.Runtime
	if handler == "" {
		handler = poolRuntime(&pool)
	}
	return r.WorkerClient.RestoreFromSnapshot(ctx, endpoint, worker.RestoreFromSnapshotRequest{
		SlotID:      ref.SlotID,
		Identity:    ref.Identity,
		StoragePath: snap.Status.StoragePath,
		Storage:     storage,
		Runtime:     handler,
	})
}
