//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"

	sandboxfleet "github.com/AgentNaut/SandboxFleet/clients/go/sandboxfleet"
	"github.com/AgentNaut/SandboxFleet/test/e2e/framework"
	"github.com/klauspost/compress/zstd"
)

// writeAndVerifySandboxFile writes path then immediately reads it back so a
// later restore failure can be distinguished from a failed write.
func writeAndVerifySandboxFile(t *testing.T, ctx context.Context, session *sandboxfleet.Sandbox, path string, body []byte) {
	t.Helper()
	if err := session.WriteSandboxFile(ctx, path, body); err != nil {
		t.Fatalf("WriteSandboxFile %s: %v", session.Name(), err)
	}
	got, err := session.ReadSandboxFile(ctx, path)
	if err != nil {
		t.Fatalf("ReadSandboxFile %s after write: %v", session.Name(), err)
	}
	if !bytes.Equal(got, body) {
		t.Fatalf("ReadSandboxFile %s after write = %q, want %q (write did not stick in guest)", session.Name(), got, body)
	}
	t.Logf("write+readback ok for %s path=%s (%d bytes)", session.Name(), path, len(body))
}

// logSnapshotMemoryRangesMarker downloads memory-ranges.zstd from MinIO and
// reports whether marker appears as raw bytes (proves CH snapshot captured it).
func logSnapshotMemoryRangesMarker(t *testing.T, ctx context.Context, tc *framework.Context, ns, storagePath string, marker []byte) (found bool) {
	t.Helper()
	raw, err := tc.MinIOGetObject(ctx, ns, strings.TrimSuffix(storagePath, "/")+"/memory-ranges.zstd")
	if err != nil {
		t.Logf("diag: download memory-ranges.zstd: %v", err)
		return false
	}
	zr, err := zstd.NewReader(bytes.NewReader(raw))
	if err != nil {
		t.Logf("diag: open zstd memory-ranges: %v", err)
		return false
	}
	defer zr.Close()
	decoded, err := io.ReadAll(zr)
	if err != nil {
		t.Logf("diag: decompress memory-ranges: %v", err)
		return false
	}
	found = bytes.Contains(decoded, marker)
	t.Logf("diag: MinIO memory-ranges logical=%d bytes contains marker %q: %v", len(decoded), marker, found)
	return found
}

// logHostSharedHasFile checks whether the file leaked onto the host virtiofs
// share (would mean writes bypassed guest tmpfs upper).
func logHostSharedHasFile(t *testing.T, ctx context.Context, ns, worker, fileName string) {
	t.Helper()
	cmd := exec.CommandContext(ctx, "kubectl", "--kubeconfig", kubeconfigPath(),
		"--context", kubeContext(),
		"-n", ns, "exec", worker, "--",
		"sh", "-c", fmt.Sprintf("find /run/kata-containers/shared -name %q 2>/dev/null | head", fileName),
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Logf("diag: host shared find %q on %s: %v (%s)", fileName, worker, err, strings.TrimSpace(string(out)))
		return
	}
	paths := strings.TrimSpace(string(out))
	if paths == "" {
		t.Logf("diag: host shared has no %q (good — write stayed in guest upper)", fileName)
		return
	}
	t.Logf("diag: host shared HAS %q (write may have gone through virtiofs):\n%s", fileName, paths)
}

func kubeconfigPath() string {
	if v := strings.TrimSpace(os.Getenv("KUBECONFIG")); v != "" {
		return v
	}
	return "bin/KUBECONFIG"
}

func kubeContext() string {
	if v := strings.TrimSpace(os.Getenv("E2E_KUBE_CONTEXT")); v != "" {
		return v
	}
	name := strings.TrimSpace(os.Getenv("CLUSTER_NAME"))
	if name == "" {
		name = "sandboxfleet-kata"
	}
	return "kind-" + name
}
