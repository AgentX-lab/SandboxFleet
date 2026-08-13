//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	sandboxv1alpha1 "github.com/AgentNaut/SandboxFleet/api/v1alpha1"
	sandboxfleet "github.com/AgentNaut/SandboxFleet/clients/go/sandboxfleet"
	"github.com/AgentNaut/SandboxFleet/test/e2e/framework"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

// TestSandboxFork:
// Runs after TestSandboxCheckpointRestore (see sandbox_checkpoint_restore_test.go).
//  1. MinIO + Pool (primary 2 slots, secondary 1) with snapshotStorage
//  2. Wait ReadyWorkers + AvailableSlots so scheduling sees all slots
//  3. Parent Running with python /readyz, write file, assert egress
//  4. Fork(1): child Ready + readyz, same file + egress
//  5. Nested Fork(1): grandchild forced onto secondary; readyz + file + egress
//  6. Delete root snapshot while child exists → CR stays (finalizer / InUse)
//  7. Delete grandchild/child then snapshots → CRs gone and MinIO prefixes empty
//
// Kata note: Fork's CreateSnapshot tears the source VMM down (TearsDownOnSave),
// so parent/child are not re-probed for Running/egress after they have been
// snapshotted — same as TestSandboxCheckpointRestore.
func TestSandboxFork(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	tc := framework.New(t)
	ns := fmt.Sprintf("sandboxfleet-fork-%d", time.Now().UnixNano())
	poolName := "fork-pool"

	tc.CreateNamespace(ctx, ns)
	t.Logf("created namespace %s", ns)

	tc.EnsureMinIO(ctx, ns)
	tc.CreatePoolFrom(ctx, "pool_fork.yaml", ns, poolName)
	tc.WaitReadyWorkers(ctx, ns, poolName, 2)
	const forkPoolSlots int32 = 3
	tc.WaitAvailableSlots(ctx, ns, poolName, forkPoolSlots)
	t.Logf("SandboxPool %s/%s Ready (2 workers, %d slots)", ns, poolName, forkPoolSlots)

	parent, err := tc.SDK.CreateSandbox(ctx, sandboxfleet.CreateOptions{
		Namespace:   ns,
		Name:        "fork-parent",
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

	fileName := "fork-note.txt"
	fileBody := []byte("sandboxfleet-fork-e2e")
	writeAndVerifySandboxFile(t, ctx, parentSession, fileName, fileBody)
	assertGuestEgressPython(t, ctx, parentSession)

	parentWorker := sandboxAssignedWorker(t, tc, ns, parent.Name)
	t.Logf("parent on worker %s", parentWorker)
	logHostSharedHasFile(t, ctx, ns, parentWorker, fileName)

	forked, err := tc.SDK.Fork(ctx, sandboxfleet.ForkOptions{
		ParentNamespace: ns,
		ParentName:      parent.Name,
		Count:           1,
		SnapshotName:    "fork-snap",
		ChildNames:      []string{"fork-parent-fork-0"},
	})
	if err != nil {
		t.Fatalf("Fork: %v", err)
	}
	if forked.Snapshot == nil || forked.Snapshot.Status.StoragePath == "" {
		t.Fatalf("Fork snapshot missing storagePath: %#v", forked.Snapshot)
	}
	storagePath := forked.Snapshot.Status.StoragePath
	t.Logf("snapshot Ready path=%s files=%v", storagePath, forked.Snapshot.Status.SnapshotFiles)
	if n := tc.MinIOObjectCount(ctx, ns, storagePath); n < 2 {
		t.Fatalf("MinIO objects under %q = %d, want >= 2 (manifest + zstd)", storagePath, n)
	}
	markerInSnap := logSnapshotMemoryRangesMarker(t, ctx, tc, ns, storagePath, fileBody)

	// Self-managed kata tears the Fork source VMM down as part of checkpoint.
	if !framework.SnapshotTearsDownSource() {
		parentAgain, err := tc.SDK.GetSandbox(ctx, ns, parent.Name)
		if err != nil {
			t.Fatalf("GetSandbox parent: %v", err)
		}
		if parentAgain.Status.Phase != sandboxv1alpha1.SandboxPhaseRunning {
			t.Fatalf("parent phase=%q, want Running", parentAgain.Status.Phase)
		}
		assertGuestEgressPython(t, ctx, parentSession)
	}

	if len(forked.Children) != 1 {
		t.Fatalf("Fork children = %d, want 1", len(forked.Children))
	}
	child := forked.Children[0]
	childWorker := sandboxAssignedWorker(t, tc, ns, child.Name)
	t.Logf("child %s on worker %s", child.Name, childWorker)
	childSession, err := tc.SDK.OpenSandboxReady(ctx, child.Namespace, child.Name)
	if err != nil {
		t.Fatalf("OpenSandboxReady %s: %v", child.Name, err)
	}
	waitGuestReadyz(t, ctx, childSession)
	got, err := childSession.ReadSandboxFile(ctx, fileName)
	if err != nil {
		t.Fatalf("ReadSandboxFile %s: %v", child.Name, err)
	}
	if string(got) != string(fileBody) {
		t.Fatalf("child %s file = %q, want %q (diag: markerInMinIOMemoryRanges=%v; write+readback on parent already passed)", child.Name, got, fileBody, markerInSnap)
	}
	assertGuestEgressPython(t, ctx, childSession)
	t.Logf("child %s file+egress ok", child.Name)

	// Nested fork from child: primary (2 slots) still holds parent+child assignment
	// → grandchild lands on secondary. Kata has already torn the parent VMM down,
	// but the slot stays occupied until DeleteSandbox.
	nested, err := tc.SDK.Fork(ctx, sandboxfleet.ForkOptions{
		ParentNamespace: ns,
		ParentName:      childSession.Name(),
		Count:           1,
		SnapshotName:    "fork-snap-nested",
		ChildNames:      []string{"fork-grandchild-0"},
	})
	if err != nil {
		t.Fatalf("nested Fork: %v", err)
	}
	if nested.Snapshot == nil || nested.Snapshot.Status.StoragePath == "" {
		t.Fatalf("nested snapshot missing storagePath: %#v", nested.Snapshot)
	}
	nestedPath := nested.Snapshot.Status.StoragePath
	t.Logf("nested snapshot Ready path=%s", nestedPath)
	if n := tc.MinIOObjectCount(ctx, ns, nestedPath); n < 2 {
		t.Fatalf("MinIO objects under nested %q = %d, want >= 2", nestedPath, n)
	}

	grandchild, err := tc.SDK.OpenSandboxReady(ctx, ns, "fork-grandchild-0")
	if err != nil {
		t.Fatalf("OpenSandboxReady grandchild: %v", err)
	}
	waitGuestReadyz(t, ctx, grandchild)
	grandchildWorker := sandboxAssignedWorker(t, tc, ns, "fork-grandchild-0")
	if grandchildWorker == childWorker {
		t.Fatalf("nested fork grandchild on worker %q, want cross-Worker (not %q)", grandchildWorker, childWorker)
	}
	t.Logf("grandchild on worker %s (child on %s)", grandchildWorker, childWorker)
	got, err = grandchild.ReadSandboxFile(ctx, fileName)
	if err != nil {
		t.Fatalf("ReadSandboxFile grandchild: %v", err)
	}
	if string(got) != string(fileBody) {
		nestedMarker := logSnapshotMemoryRangesMarker(t, ctx, tc, ns, nestedPath, fileBody)
		t.Fatalf("grandchild file = %q, want %q (diag: nestedMarkerInMinIO=%v firstSnapMarker=%v)", got, fileBody, nestedMarker, markerInSnap)
	}
	assertGuestEgressPython(t, ctx, grandchild)
	t.Logf("grandchild file+egress ok")

	// Nested Fork checkpoints the child; only snapshotters that leave it running
	// are probed again here (kata tears it down).
	if !framework.SnapshotTearsDownSource() {
		childObj, err := tc.SDK.GetSandbox(ctx, ns, childSession.Name())
		if err != nil {
			t.Fatalf("GetSandbox child: %v", err)
		}
		if childObj.Status.Phase != sandboxv1alpha1.SandboxPhaseRunning {
			t.Fatalf("child phase=%q, want Running", childObj.Status.Phase)
		}
		assertGuestEgressPython(t, ctx, childSession)
	}

	if err := tc.SDK.DeleteSnapshot(ctx, ns, forked.Snapshot.Name); err != nil {
		t.Fatalf("DeleteSnapshot (in use): %v", err)
	}
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		snap, err := tc.SDK.GetSnapshot(ctx, ns, forked.Snapshot.Name)
		if apierrors.IsNotFound(err) {
			t.Fatal("snapshot deleted while children still reference fromSnapshot")
		}
		if err != nil {
			t.Fatalf("GetSnapshot after in-use delete: %v", err)
		}
		if snap.DeletionTimestamp != nil && strings.Contains(snap.Status.Message, "referenced") {
			t.Logf("snapshot correctly blocked: %s", snap.Status.Message)
			break
		}
		time.Sleep(time.Second)
	}
	if _, err := tc.SDK.GetSnapshot(ctx, ns, forked.Snapshot.Name); apierrors.IsNotFound(err) {
		t.Fatal("snapshot vanished while still in use")
	}

	grandchild.Close()
	if err := tc.SDK.DeleteSandbox(ctx, ns, grandchild.Name()); err != nil {
		t.Fatalf("DeleteSandbox grandchild: %v", err)
	}
	if err := tc.SDK.WaitSandboxDeleted(ctx, ns, grandchild.Name()); err != nil {
		t.Fatalf("WaitSandboxDeleted grandchild: %v", err)
	}
	_ = tc.SDK.DeleteSnapshot(ctx, ns, nested.Snapshot.Name)
	if err := tc.SDK.WaitSnapshotDeleted(ctx, ns, nested.Snapshot.Name); err != nil {
		t.Fatalf("WaitSnapshotDeleted nested: %v", err)
	}
	if n := tc.MinIOObjectCount(ctx, ns, nestedPath); n != 0 {
		t.Fatalf("MinIO objects under nested %q after delete = %d, want 0", nestedPath, n)
	}

	childSession.Close()
	if err := tc.SDK.DeleteSandbox(ctx, childSession.Namespace(), childSession.Name()); err != nil {
		t.Fatalf("DeleteSandbox %s: %v", childSession.Name(), err)
	}
	if err := tc.SDK.WaitSandboxDeleted(ctx, childSession.Namespace(), childSession.Name()); err != nil {
		t.Fatalf("WaitSandboxDeleted %s: %v", childSession.Name(), err)
	}

	if err := tc.SDK.WaitSnapshotDeleted(ctx, ns, forked.Snapshot.Name); err != nil {
		_ = tc.SDK.DeleteSnapshot(ctx, ns, forked.Snapshot.Name)
		if err := tc.SDK.WaitSnapshotDeleted(ctx, ns, forked.Snapshot.Name); err != nil {
			t.Fatalf("WaitSnapshotDeleted: %v", err)
		}
	}
	if n := tc.MinIOObjectCount(ctx, ns, storagePath); n != 0 {
		t.Fatalf("MinIO objects under %q after delete = %d, want 0", storagePath, n)
	}
	t.Logf("snapshot objects cleaned from MinIO")

	parentSession.Close()
	if err := tc.SDK.DeleteSandbox(ctx, ns, parent.Name); err != nil {
		t.Fatalf("DeleteSandbox parent: %v", err)
	}
	if err := tc.SDK.WaitSandboxDeleted(ctx, ns, parent.Name); err != nil {
		t.Fatalf("WaitSandboxDeleted parent: %v", err)
	}
}

func sandboxAssignedWorker(t *testing.T, tc *framework.Context, namespace, name string) string {
	t.Helper()
	sb, err := tc.SDK.GetSandbox(context.Background(), namespace, name)
	if err != nil {
		t.Fatalf("GetSandbox %s for worker: %v", name, err)
	}
	if sb.Status.Assignment == nil || sb.Status.Assignment.Worker == "" {
		t.Fatalf("sandbox %s missing assignment worker", name)
	}
	return sb.Status.Assignment.Worker
}
