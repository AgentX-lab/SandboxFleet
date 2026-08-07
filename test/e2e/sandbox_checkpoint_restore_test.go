//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"testing"
	"time"

	sandboxv1alpha1 "github.com/AgentNaut/SandboxFleet/api/v1alpha1"
	sandboxfleet "github.com/AgentNaut/SandboxFleet/clients/go/sandboxfleet"
	"github.com/AgentNaut/SandboxFleet/test/e2e/framework"
)

// TestSandboxCheckpointRestore is the baseline for memory snapshotting.
// File name sorts before sandbox_fork_test.go so go test runs this first:
// if CreateSnapshot / fromSnapshot is broken, Fork will fail the same way.
//
//  1. MinIO + 1-worker pool with 1 slot (keeps CI memory pressure low)
//  2. Parent Running with python /readyz; write file; egress
//  3. CreateSnapshot → Ready in object storage
//  4. Parent still Running after snapshot (leave-running), then delete to free the slot
//  5. CreateSandboxFromSnapshot → child Ready + readyz; same file + egress
//  6. Cleanup child, snapshot
func TestSandboxCheckpointRestore(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()

	tc := framework.New(t)
	ns := fmt.Sprintf("sandboxfleet-snap-%d", time.Now().UnixNano())
	poolName := "snap-pool"

	tc.CreateNamespace(ctx, ns)
	t.Logf("created namespace %s", ns)

	tc.EnsureMinIO(ctx, ns)
	tc.CreatePoolFrom(ctx, "pool_snapshot.yaml", ns, poolName)
	tc.WaitPoolReady(ctx, ns, poolName)
	t.Logf("SandboxPool %s/%s Ready", ns, poolName)

	parent, err := tc.SDK.CreateSandbox(ctx, sandboxfleet.CreateOptions{
		Namespace:   ns,
		Name:        "snap-parent",
		PoolRef:     poolName,
		SlotProfile: "small",
		Container:   forkE2EContainer(),
	})
	if err != nil {
		t.Fatalf("CreateSandbox parent: %v", err)
	}
	parentSession, err := tc.SDK.OpenSandboxReady(ctx, parent.Namespace, parent.Name)
	if err != nil {
		t.Fatalf("OpenSandboxReady parent: %v", err)
	}
	defer parentSession.Close()
	waitGuestReadyz(t, ctx, parentSession)

	fileName := "snap-note.txt"
	fileBody := []byte("sandboxfleet-checkpoint-restore-e2e")
	if err := parentSession.WriteSandboxFile(ctx, fileName, fileBody); err != nil {
		t.Fatalf("WriteSandboxFile parent: %v", err)
	}
	assertGuestEgressPython(t, ctx, parentSession)

	snap, err := tc.SDK.CreateSnapshot(ctx, sandboxfleet.SnapshotOptions{
		Namespace:     ns,
		Name:          "snap-cr",
		SourceSandbox: parent.Name,
		Pool:          poolName,
	})
	if err != nil {
		t.Fatalf("CreateSnapshot: %v", err)
	}
	snap, err = tc.SDK.WaitSnapshotReady(ctx, snap.Namespace, snap.Name)
	if err != nil {
		t.Fatalf("WaitSnapshotReady: %v", err)
	}
	if snap.Status.StoragePath == "" {
		t.Fatalf("snapshot missing storagePath: %#v", snap)
	}
	t.Logf("snapshot Ready path=%s files=%v", snap.Status.StoragePath, snap.Status.SnapshotFiles)
	if n := tc.MinIOObjectCount(ctx, ns, snap.Status.StoragePath); n < 2 {
		t.Fatalf("MinIO objects under %q = %d, want >= 2", snap.Status.StoragePath, n)
	}

	parentAgain, err := tc.SDK.GetSandbox(ctx, ns, parent.Name)
	if err != nil {
		t.Fatalf("GetSandbox parent after snapshot: %v", err)
	}
	if parentAgain.Status.Phase != sandboxv1alpha1.SandboxPhaseRunning {
		t.Fatalf("parent phase=%q after snapshot, want Running", parentAgain.Status.Phase)
	}
	assertGuestEgressPython(t, ctx, parentSession)

	// Free the single slot before restore (parent + child concurrent VMs OOM kata workers).
	parentSession.Close()
	if err := tc.SDK.DeleteSandbox(ctx, ns, parent.Name); err != nil {
		t.Fatalf("DeleteSandbox parent before restore: %v", err)
	}
	if err := tc.SDK.WaitSandboxDeleted(ctx, ns, parent.Name); err != nil {
		t.Fatalf("WaitSandboxDeleted parent before restore: %v", err)
	}

	childCR, err := tc.SDK.CreateSandboxFromSnapshot(ctx, sandboxfleet.CreateOptions{
		Namespace:   ns,
		Name:        "snap-child",
		PoolRef:     poolName,
		SlotProfile: "small",
	}, snap.Name)
	if err != nil {
		t.Fatalf("CreateSandboxFromSnapshot: %v", err)
	}
	childSession, err := tc.SDK.OpenSandboxReady(ctx, childCR.Namespace, childCR.Name)
	if err != nil {
		t.Fatalf("OpenSandboxReady child: %v", err)
	}
	defer childSession.Close()
	waitGuestReadyz(t, ctx, childSession)

	got, err := childSession.ReadSandboxFile(ctx, fileName)
	if err != nil {
		t.Fatalf("ReadSandboxFile child: %v", err)
	}
	if string(got) != string(fileBody) {
		t.Fatalf("child file = %q, want %q", got, fileBody)
	}
	assertGuestEgressPython(t, ctx, childSession)
	t.Logf("checkpoint/restore child file+egress ok")

	childSession.Close()
	if err := tc.SDK.DeleteSandbox(ctx, ns, childSession.Name()); err != nil {
		t.Fatalf("DeleteSandbox child: %v", err)
	}
	if err := tc.SDK.WaitSandboxDeleted(ctx, ns, childSession.Name()); err != nil {
		t.Fatalf("WaitSandboxDeleted child: %v", err)
	}

	if err := tc.SDK.DeleteSnapshot(ctx, ns, snap.Name); err != nil {
		t.Fatalf("DeleteSnapshot: %v", err)
	}
	if err := tc.SDK.WaitSnapshotDeleted(ctx, ns, snap.Name); err != nil {
		t.Fatalf("WaitSnapshotDeleted: %v", err)
	}
	if n := tc.MinIOObjectCount(ctx, ns, snap.Status.StoragePath); n != 0 {
		t.Fatalf("MinIO objects under %q after delete = %d, want 0", snap.Status.StoragePath, n)
	}
}
