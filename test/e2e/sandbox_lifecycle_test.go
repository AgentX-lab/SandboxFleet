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
)

// 1. Create namespace + SandboxPool (from testdata YAML) and wait until Ready.
// 2. Create two Sandboxes on that pool (busybox + sleep) and OpenSandboxReady.
// 3. Assert both are Assigned to the same Worker with distinct SlotIDs.
// 4. Exec echo in each sandbox and check exit code + stdout.
// 5. On sandbox A only: Write / Exists / Read / List under the files root.
// 6. Delete both Sandboxes and wait until they are gone.
func TestSandboxLifecycleAndExec(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	tc := framework.New(t)
	ns := fmt.Sprintf("sandboxfleet-e2e-%d", time.Now().UnixNano())
	poolName := "e2e-pool"

	tc.CreateNamespace(ctx, ns)
	t.Logf("created namespace %s", ns)

	// Pool comes from test/e2e/testdata/sandboxpool.yaml (slotsPerWorker=2).
	tc.CreatePool(ctx, ns, poolName)
	t.Logf("created SandboxPool %s/%s", ns, poolName)

	tc.WaitPoolReady(ctx, ns, poolName)
	t.Logf("SandboxPool %s/%s is Ready", ns, poolName)

	container := sandboxv1alpha1.ContainerSpec{
		Image:   "registry.k8s.io/e2e-test-images/busybox:1.36.1-1",
		Command: []string{"sleep", "3600"},
	}

	a, err := tc.SDK.CreateSandbox(ctx, sandboxfleet.CreateOptions{
		Namespace:   ns,
		Name:        "e2e-sandbox-a",
		PoolRef:     poolName,
		SlotProfile: "default",
		Container:   container,
	})
	if err != nil {
		t.Fatalf("CreateSandbox a: %v", err)
	}
	t.Logf("created Sandbox %s/%s", a.Namespace, a.Name)

	b, err := tc.SDK.CreateSandbox(ctx, sandboxfleet.CreateOptions{
		Namespace:   ns,
		Name:        "e2e-sandbox-b",
		PoolRef:     poolName,
		SlotProfile: "default",
		Container:   container,
	})
	if err != nil {
		t.Fatalf("CreateSandbox b: %v", err)
	}
	t.Logf("created Sandbox %s/%s", b.Namespace, b.Name)

	sessionA, err := tc.SDK.OpenSandboxReady(ctx, a.Namespace, a.Name)
	if err != nil {
		t.Fatalf("OpenSandboxReady a: %v", err)
	}
	defer sessionA.Close()
	sessionB, err := tc.SDK.OpenSandboxReady(ctx, b.Namespace, b.Name)
	if err != nil {
		t.Fatalf("OpenSandboxReady b: %v", err)
	}
	defer sessionB.Close()

	readyA := sessionA.Object()
	readyB := sessionB.Object()
	if readyA.Status.Assignment == nil || readyB.Status.Assignment == nil {
		t.Fatal("expected both Sandboxes to be assigned")
	}

	assignA, assignB := readyA.Status.Assignment, readyB.Status.Assignment
	t.Logf("Sandbox a Running on worker=%s slot=%d", assignA.Worker, assignA.SlotID)
	t.Logf("Sandbox b Running on worker=%s slot=%d", assignB.Worker, assignB.SlotID)

	if assignA.Worker != assignB.Worker {
		t.Fatalf("expected both Sandboxes on the same Worker, got %q and %q", assignA.Worker, assignB.Worker)
	}
	if assignA.SlotID == assignB.SlotID {
		t.Fatalf("expected distinct SlotIDs on one Worker, both got %d", assignA.SlotID)
	}

	for _, session := range []*sandboxfleet.Sandbox{sessionA, sessionB} {
		want := fmt.Sprintf("hello-%s", session.Name())
		result, err := session.Exec(ctx, sandboxfleet.ExecOptions{Command: []string{"echo", want}, Timeout: 30 * time.Second})
		if err != nil {
			t.Fatalf("Exec %s: %v", session.Name(), err)
		}
		if result.ExitCode != 0 {
			t.Fatalf("Exec %s exit=%d stderr=%q", session.Name(), result.ExitCode, result.Stderr)
		}
		if !strings.Contains(result.Stdout, want) {
			t.Fatalf("Exec %s stdout=%q, want %q", session.Name(), result.Stdout, want)
		}
		t.Logf("Exec %s ok: stdout=%q", session.Name(), strings.TrimSpace(result.Stdout))
	}

	fileName := "e2e-note.txt"
	fileBody := []byte("sandboxfleet-files")
	if err := sessionA.WriteSandboxFile(ctx, fileName, fileBody); err != nil {
		t.Fatalf("WriteSandboxFile: %v", err)
	}
	exists, err := sessionA.ExistsSandboxFile(ctx, fileName)
	if err != nil {
		t.Fatalf("ExistsSandboxFile: %v", err)
	}
	if !exists {
		t.Fatalf("ExistsSandboxFile(%q) = false after Write", fileName)
	}
	got, err := sessionA.ReadSandboxFile(ctx, fileName)
	if err != nil {
		t.Fatalf("ReadSandboxFile: %v", err)
	}
	if string(got) != string(fileBody) {
		t.Fatalf("ReadSandboxFile(%q) = %q, want %q", fileName, got, fileBody)
	}
	entries, err := sessionA.ListSandboxFiles(ctx, ".")
	if err != nil {
		t.Fatalf("ListSandboxFiles: %v", err)
	}
	found := false
	for _, entry := range entries {
		if entry.Name == fileName && entry.Type == "file" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("ListSandboxFiles(.) = %#v, want %q", entries, fileName)
	}
	t.Logf("file ops on %s ok", sessionA.Name())

	sessionA.Close()
	sessionB.Close()

	for _, name := range []string{a.Name, b.Name} {
		if err := tc.SDK.DeleteSandbox(ctx, ns, name); err != nil {
			t.Fatalf("DeleteSandbox %s: %v", name, err)
		}
		if err := tc.SDK.WaitSandboxDeleted(ctx, ns, name); err != nil {
			t.Fatalf("WaitSandboxDeleted %s: %v", name, err)
		}
		t.Logf("Sandbox %s/%s deleted", ns, name)
	}
}
