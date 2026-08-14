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
	"time"

	sandboxfleet "github.com/AgentNaut/SandboxFleet/clients/go/sandboxfleet"
	"github.com/AgentNaut/SandboxFleet/test/e2e/framework"
	"github.com/klauspost/compress/zstd"
)

// writeAndVerifySandboxFile writes path then immediately reads it back so a
// later restore failure can be distinguished from a failed write.
func writeAndVerifySandboxFile(t *testing.T, ctx context.Context, session *sandboxfleet.Sandbox, path string, body []byte) {
	t.Helper()
	t.Logf("writeAndVerify: sandbox=%s path=%s wantBytes=%d", session.Name(), path, len(body))
	if err := session.WriteSandboxFile(ctx, path, body); err != nil {
		_ = logGuestFileLayers(t, ctx, session, path)
		t.Fatalf("WriteSandboxFile %s: %v", session.Name(), err)
	}
	layersAfterWrite := logGuestFileLayers(t, ctx, session, path)
	t.Logf("writeAndVerify: after WriteSandboxFile layers={%s}", layersAfterWrite)

	got, err := session.ReadSandboxFile(ctx, path)
	if err != nil {
		_ = logGuestFileLayers(t, ctx, session, path)
		t.Fatalf("ReadSandboxFile %s after write: %v", session.Name(), err)
	}
	t.Logf("writeAndVerify: ReadSandboxFile gotBytes=%d", len(got))
	if !bytes.Equal(got, body) {
		failSandboxFileMismatch(t, ctx, session, path, got, body, "write did not stick in guest")
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

// logGuestFileLayers dumps /app/<file> vs overlay upper (/run/ateom-upper) so a
// restore-empty failure can be classified (inode empty vs wrong mount layer).
// Returns a one-line summary suitable for t.Fatalf; full dump is always t.Logf'd
// (go test prints logs for failed tests even without -v).
func logGuestFileLayers(t *testing.T, ctx context.Context, session *sandboxfleet.Sandbox, fileName string) (summary string) {
	t.Helper()
	appPath := "/app/" + fileName
	script := `
set +e
f="$1"
echo "=== app path ==="
ls -la -- "$f" 2>&1
wc -c -- "$f" 2>&1
echo "=== app hex (first 64) ==="
od -An -tx1 -N64 -- "$f" 2>&1
echo "=== pwd /app ==="
pwd 2>&1
ls -la /app 2>&1 | head -40
echo "=== overlay upper matches ==="
find /run/ateom-upper -name "$(basename -- "$f")" 2>/dev/null | while IFS= read -r p; do
  echo "-- $p"
  ls -la -- "$p" 2>&1
  wc -c -- "$p" 2>&1
  od -An -tx1 -N64 -- "$p" 2>&1
done
echo "=== mounts (overlay|virtio|upper) ==="
mount 2>/dev/null | grep -E 'overlay|virtio|ateom-upper|kataShared' || echo "(none)"
`
	result, err := session.Exec(ctx, sandboxfleet.ExecOptions{
		Command: []string{"sh", "-c", script, "diag-file-layers", appPath},
		Timeout: 30 * time.Second,
	})
	if err != nil {
		summary = fmt.Sprintf("guestFileLayers exec err: %v", err)
		t.Logf("diag[%s]: %s", session.Name(), summary)
		return summary
	}
	out := strings.TrimSpace(result.Stdout + result.Stderr)
	t.Logf("diag[%s] guest file layers for %s (exit=%d):\n%s", session.Name(), appPath, result.ExitCode, out)

	appBytes := parseWCBytes(out, appPath)
	upperHits := strings.Count(out, "-- /run/ateom-upper")
	summary = fmt.Sprintf("appBytes=%d upperHits=%d", appBytes, upperHits)
	return summary
}

// failSandboxFileMismatch logs guest-layer + MinIO marker diagnostics then Fatals.
func failSandboxFileMismatch(t *testing.T, ctx context.Context, session *sandboxfleet.Sandbox, fileName string, got, want []byte, extra string) {
	t.Helper()
	layers := logGuestFileLayers(t, ctx, session, fileName)
	t.Fatalf("sandbox %s file %q = %q (%d bytes), want %q (%d bytes); layers={%s}; %s",
		session.Name(), fileName, got, len(got), want, len(want), layers, extra)
}

func parseWCBytes(diagOut, path string) int {
	base := path
	if i := strings.LastIndex(path, "/"); i >= 0 {
		base = path[i+1:]
	}
	for _, line := range strings.Split(diagOut, "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) < 2 {
			continue
		}
		last := fields[len(fields)-1]
		if last != path && !strings.HasSuffix(last, "/"+base) {
			continue
		}
		var n int
		if _, err := fmt.Sscanf(fields[0], "%d", &n); err == nil {
			return n
		}
	}
	return -1
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
