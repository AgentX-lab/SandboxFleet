package runtime

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"
	"testing"
)

func TestWriteFileViaVerifiesSize(t *testing.T) {
	t.Parallel()
	store := map[string][]byte{}
	exec := func(_ context.Context, req ExecRequest) (ExecResult, error) {
		if len(req.Command) < 6 || req.Command[3] != "write" {
			return ExecResult{ExitCode: 1, Stderr: "bad argv"}, nil
		}
		path := req.Command[4]
		encoded := req.Command[5]
		want := req.Command[6]
		data, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			return ExecResult{ExitCode: 1, Stderr: err.Error()}, nil
		}
		store[path] = append([]byte(nil), data...)
		stdout := fmt.Sprintf("WRITE_VERIFY path=%s want=%s got=%d\n", path, want, len(data))
		if want != fmt.Sprintf("%d", len(data)) {
			return ExecResult{ExitCode: 1, Stdout: stdout}, nil
		}
		return ExecResult{ExitCode: 0, Stdout: stdout}, nil
	}
	content := []byte("sandboxfleet-checkpoint-restore-e2e")
	if err := WriteFileVia(context.Background(), exec, "/app/snap-note.txt", content); err != nil {
		t.Fatalf("WriteFileVia: %v", err)
	}
	if got := string(store["/app/snap-note.txt"]); got != string(content) {
		t.Fatalf("stored = %q, want %q", got, content)
	}

	// Simulate truncate-to-empty after redirect: want=35 got=0 must fail.
	bad := func(_ context.Context, req ExecRequest) (ExecResult, error) {
		want := req.Command[6]
		stdout := fmt.Sprintf("WRITE_VERIFY path=%s want=%s got=0\n", req.Command[4], want)
		return ExecResult{ExitCode: 1, Stdout: stdout, Stderr: "test"}, nil
	}
	err := WriteFileVia(context.Background(), bad, "/app/empty.txt", content)
	if err == nil {
		t.Fatal("WriteFileVia empty file: want error")
	}
	if !strings.Contains(err.Error(), "WRITE_VERIFY") && !strings.Contains(err.Error(), "got=0") {
		t.Fatalf("error should include verify output: %v", err)
	}
}
