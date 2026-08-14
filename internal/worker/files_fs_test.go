package worker

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"
	"testing"

	sandboxruntime "github.com/AgentNaut/SandboxFleet/internal/runtime"
)

func TestExecSandboxFSWriteReadListExists(t *testing.T) {
	ctx := context.Background()
	store := map[string][]byte{}
	fs := &execSandboxFS{
		exec: func(_ context.Context, req sandboxruntime.ExecRequest) (sandboxruntime.ExecResult, error) {
			return fakeShellExec(store, req)
		},
	}

	const absPath = "/app/note.txt"
	content := []byte("hello restored")
	if err := fs.Write(ctx, absPath, content); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	ok, err := fs.Exists(ctx, absPath)
	if err != nil {
		t.Fatalf("Exists() error = %v", err)
	}
	if !ok {
		t.Fatal("Exists() = false, want true")
	}
	got, err := fs.Read(ctx, absPath)
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	if string(got) != string(content) {
		t.Fatalf("Read() = %q, want %q", got, content)
	}
	entries, err := fs.List(ctx, "/app")
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	found := false
	for _, e := range entries {
		if e.Name == "note.txt" && e.Type == FileTypeFile {
			found = true
		}
	}
	if !found {
		t.Fatalf("List() = %#v, want note.txt", entries)
	}
}

func TestParseFileList(t *testing.T) {
	got := parseFileList("file\ta.txt\ndirectory\tdir\n\nbadline\n")
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2: %#v", len(got), got)
	}
	if got[0] != (SandboxFileEntry{Type: FileTypeFile, Name: "a.txt"}) {
		t.Fatalf("got[0] = %#v", got[0])
	}
	if got[1] != (SandboxFileEntry{Type: FileTypeDirectory, Name: "dir"}) {
		t.Fatalf("got[1] = %#v", got[1])
	}
}

// fakeShellExec understands the argv shape produced by execSandboxFS.
func fakeShellExec(store map[string][]byte, req sandboxruntime.ExecRequest) (sandboxruntime.ExecResult, error) {
	if len(req.Command) < 4 || req.Command[0] != "sh" || req.Command[1] != "-c" {
		return sandboxruntime.ExecResult{ExitCode: 1, Stderr: "bad argv"}, nil
	}
	name := req.Command[3]
	switch name {
	case "exists":
		path := req.Command[4]
		if _, ok := store[path]; ok {
			return sandboxruntime.ExecResult{ExitCode: 0}, nil
		}
		return sandboxruntime.ExecResult{ExitCode: 1}, nil
	case "list":
		dir := req.Command[4]
		var b strings.Builder
		prefix := dir + "/"
		for path := range store {
			if !strings.HasPrefix(path, prefix) {
				continue
			}
			name := strings.TrimPrefix(path, prefix)
			if strings.Contains(name, "/") {
				continue
			}
			b.WriteString("file\t")
			b.WriteString(name)
			b.WriteByte('\n')
		}
		return sandboxruntime.ExecResult{ExitCode: 0, Stdout: b.String()}, nil
	case "read":
		path := req.Command[4]
		data, ok := store[path]
		if !ok {
			return sandboxruntime.ExecResult{ExitCode: 1, Stderr: "missing"}, nil
		}
		if len(data) == 0 {
			return sandboxruntime.ExecResult{ExitCode: 0, Stdout: "READ_VERIFY bytes=0\n"}, nil
		}
		stdout := base64.StdEncoding.EncodeToString(data) + "\n" + fmt.Sprintf("READ_VERIFY bytes=%d\n", len(data))
		return sandboxruntime.ExecResult{ExitCode: 0, Stdout: stdout}, nil
	case "write":
		path := req.Command[4]
		encoded := req.Command[5]
		data, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			return sandboxruntime.ExecResult{ExitCode: 1, Stderr: err.Error()}, nil
		}
		out := make([]byte, len(data))
		copy(out, data)
		store[path] = out
		want := ""
		if len(req.Command) > 6 {
			want = req.Command[6]
		}
		stdout := fmt.Sprintf("WRITE_VERIFY path=%s want=%s got=%d\n", path, want, len(out))
		if want != "" && want != fmt.Sprintf("%d", len(out)) {
			return sandboxruntime.ExecResult{ExitCode: 1, Stdout: stdout, Stderr: "size mismatch"}, nil
		}
		return sandboxruntime.ExecResult{ExitCode: 0, Stdout: stdout}, nil
	default:
		return sandboxruntime.ExecResult{ExitCode: 1, Stderr: "unknown op " + name}, nil
	}
}
