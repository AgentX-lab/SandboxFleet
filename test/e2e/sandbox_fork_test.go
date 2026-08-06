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
//  1. MinIO + Pool (4 slots) with snapshotStorage
//  2. Parent Running, write file, assert egress
//  3. Fork(2): snapshot Ready, two children Running with same file + egress
//  4. Nested Fork(1) from child-0: grandchild Ready, file + egress; child still Running
//  5. Parent still Running
//  6. Delete root snapshot while children exist → CR stays (finalizer / InUse)
//  7. Delete grandchild/children then snapshots → CRs gone and MinIO prefixes empty
func TestSandboxFork(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Minute)
	defer cancel()

	tc := framework.New(t)
	ns := fmt.Sprintf("sandboxfleet-fork-%d", time.Now().UnixNano())
	poolName := "fork-pool"

	tc.CreateNamespace(ctx, ns)
	t.Logf("created namespace %s", ns)

	tc.EnsureMinIO(ctx, ns)
	tc.CreatePoolFrom(ctx, "pool_fork.yaml", ns, poolName)
	tc.WaitPoolReady(ctx, ns, poolName)
	t.Logf("SandboxPool %s/%s Ready", ns, poolName)

	container := sandboxv1alpha1.ContainerSpec{
		Image:   "registry.k8s.io/e2e-test-images/busybox:1.36.1-1",
		Command: []string{"sleep", "3600"},
	}
	parent, err := tc.SDK.CreateSandbox(ctx, sandboxfleet.CreateOptions{
		Namespace:   ns,
		Name:        "fork-parent",
		PoolRef:     poolName,
		SlotProfile: "small",
		Container:   container,
	})
	if err != nil {
		t.Fatalf("CreateSandbox parent: %v", err)
	}
	parentSession, err := tc.SDK.OpenSandboxReady(ctx, parent.Namespace, parent.Name)
	if err != nil {
		t.Fatalf("OpenSandboxReady parent: %v", err)
	}
	defer parentSession.Close()

	fileName := "fork-note.txt"
	fileBody := []byte("sandboxfleet-fork-e2e")
	if err := parentSession.WriteSandboxFile(ctx, fileName, fileBody); err != nil {
		t.Fatalf("WriteSandboxFile parent: %v", err)
	}
	assertGuestEgress(t, ctx, parentSession)

	forked, err := tc.SDK.Fork(ctx, sandboxfleet.ForkOptions{
		ParentNamespace: ns,
		ParentName:      parent.Name,
		Count:           2,
		SnapshotName:    "fork-snap",
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

	// Parent must remain usable after snapshot.
	parentAgain, err := tc.SDK.GetSandbox(ctx, ns, parent.Name)
	if err != nil {
		t.Fatalf("GetSandbox parent: %v", err)
	}
	if parentAgain.Status.Phase != sandboxv1alpha1.SandboxPhaseRunning {
		t.Fatalf("parent phase=%q, want Running", parentAgain.Status.Phase)
	}
	assertGuestEgress(t, ctx, parentSession)

	childSessions := make([]*sandboxfleet.Sandbox, 0, len(forked.Children))
	for _, child := range forked.Children {
		session, err := tc.SDK.OpenSandboxReady(ctx, child.Namespace, child.Name)
		if err != nil {
			t.Fatalf("OpenSandboxReady %s: %v", child.Name, err)
		}
		childSessions = append(childSessions, session)
		got, err := session.ReadSandboxFile(ctx, fileName)
		if err != nil {
			t.Fatalf("ReadSandboxFile %s: %v", child.Name, err)
		}
		if string(got) != string(fileBody) {
			t.Fatalf("child %s file = %q, want %q", child.Name, got, fileBody)
		}
		assertGuestEgress(t, ctx, session)
		t.Logf("child %s file+egress ok", child.Name)
	}

	// Nested fork: snapshot a restored child and restore one grandchild.
	nestedParent := childSessions[0]
	nested, err := tc.SDK.Fork(ctx, sandboxfleet.ForkOptions{
		ParentNamespace: ns,
		ParentName:      nestedParent.Name(),
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
	got, err := grandchild.ReadSandboxFile(ctx, fileName)
	if err != nil {
		t.Fatalf("ReadSandboxFile grandchild: %v", err)
	}
	if string(got) != string(fileBody) {
		t.Fatalf("grandchild file = %q, want %q", got, fileBody)
	}
	assertGuestEgress(t, ctx, grandchild)
	t.Logf("grandchild file+egress ok")

	nestedParentObj, err := tc.SDK.GetSandbox(ctx, ns, nestedParent.Name())
	if err != nil {
		t.Fatalf("GetSandbox nested parent: %v", err)
	}
	if nestedParentObj.Status.Phase != sandboxv1alpha1.SandboxPhaseRunning {
		t.Fatalf("nested parent phase=%q, want Running", nestedParentObj.Status.Phase)
	}
	assertGuestEgress(t, ctx, nestedParent)

	// Delete root snapshot while children still reference it → must not finish deleting.
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

	// Cleanup leaf → nested snapshot → children → root snapshot → parent.
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

	for _, session := range childSessions {
		session.Close()
		if err := tc.SDK.DeleteSandbox(ctx, session.Namespace(), session.Name()); err != nil {
			t.Fatalf("DeleteSandbox %s: %v", session.Name(), err)
		}
		if err := tc.SDK.WaitSandboxDeleted(ctx, session.Namespace(), session.Name()); err != nil {
			t.Fatalf("WaitSandboxDeleted %s: %v", session.Name(), err)
		}
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
