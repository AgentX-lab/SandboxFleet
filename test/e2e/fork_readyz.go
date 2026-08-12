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
)

// forkE2EContainer runs a tiny HTTP /readyz server (substrate-style app probe).
func forkE2EContainer() sandboxv1alpha1.ContainerSpec {
	return sandboxv1alpha1.ContainerSpec{
		Image: "python:3.12-slim",
		Command: []string{"python", "-u", "-c", `
from http.server import BaseHTTPRequestHandler, HTTPServer
class H(BaseHTTPRequestHandler):
    def do_GET(self):
        code = 200 if self.path in ("/readyz", "/") else 404
        self.send_response(code)
        self.end_headers()
        if code == 200:
            self.wfile.write(b"ok")
    def log_message(self, *args):
        pass
HTTPServer(("0.0.0.0", 8080), H).serve_forever()
`},
	}
}

const guestReadyzURL = "http://127.0.0.1:8080/readyz"

// waitGuestReadyz polls the guest /readyz until HTTP 200 (retries exec races too).
func waitGuestReadyz(t *testing.T, ctx context.Context, session *sandboxfleet.Sandbox) {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	var last string
	for time.Now().Before(deadline) {
		if err := ctx.Err(); err != nil {
			t.Fatalf("readyz %s cancelled: %v (last %s)", session.Name(), err, last)
		}
		result, err := session.Exec(ctx, sandboxfleet.ExecOptions{
			Command: []string{"python", "-c", fmt.Sprintf(
				"import urllib.request; urllib.request.urlopen(%q, timeout=2).read()", guestReadyzURL,
			)},
			Timeout: 10 * time.Second,
		})
		if err == nil && result.ExitCode == 0 {
			t.Logf("readyz ok for %s", session.Name())
			return
		}
		if err != nil {
			last = err.Error()
		} else {
			last = fmt.Sprintf("exit=%d stderr=%s", result.ExitCode, strings.TrimSpace(result.Stderr))
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("readyz %s not ready: %s", session.Name(), last)
}

// assertGuestEgressPython checks guest internet via stdlib (no wget in python slim).
func assertGuestEgressPython(t *testing.T, ctx context.Context, session *sandboxfleet.Sandbox) {
	t.Helper()
	result, err := session.Exec(ctx, sandboxfleet.ExecOptions{
		Command: []string{"python", "-c", fmt.Sprintf(
			"import urllib.request; urllib.request.urlopen(%q, timeout=20)", guestEgressURL,
		)},
		Timeout: 45 * time.Second,
	})
	if err != nil {
		logGuestNetworkDiag(t, ctx, session)
		t.Fatalf("egress Exec %s: %v", session.Name(), err)
	}
	if result.ExitCode != 0 {
		logGuestNetworkDiag(t, ctx, session)
		t.Fatalf("egress Exec %s exit=%d stderr=%q stdout=%q",
			session.Name(), result.ExitCode, strings.TrimSpace(result.Stderr), strings.TrimSpace(result.Stdout))
	}
	t.Logf("egress ok for %s", session.Name())
}

// logGuestNetworkDiag dumps guest resolv/routes and probes gw + 8.8.8.8 when egress
// fails. python:3.12-slim has no ip/ping, so this is stdlib-only.
func logGuestNetworkDiag(t *testing.T, ctx context.Context, session *sandboxfleet.Sandbox) {
	t.Helper()
	const script = `
import socket, os
def show(path):
    print("===", path, "===")
    try:
        print(open(path).read())
    except Exception as e:
        print("err:", e)
show("/etc/resolv.conf")
show("/proc/net/route")
for host, port in (("10.89.0.1", 80), ("8.8.8.8", 53), ("1.1.1.1", 53)):
    s = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
    s.settimeout(3)
    try:
        s.connect((host, port))
        print(f"tcp {host}:{port} ok")
    except Exception as e:
        print(f"tcp {host}:{port} FAIL: {e}")
    finally:
        s.close()
try:
    print("getaddrinfo www.gstatic.com:", socket.getaddrinfo("www.gstatic.com", 80)[:1])
except Exception as e:
    print("getaddrinfo www.gstatic.com FAIL:", e)
`
	result, err := session.Exec(ctx, sandboxfleet.ExecOptions{
		Command: []string{"python", "-c", script},
		Timeout: 30 * time.Second,
	})
	if err != nil {
		t.Logf("guest net diag %s: exec err: %v", session.Name(), err)
		return
	}
	t.Logf("guest net diag %s exit=%d\n%s%s",
		session.Name(), result.ExitCode, result.Stdout, result.Stderr)
}
