package worker

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"
	"time"

	sandboxruntime "github.com/AgentNaut/SandboxFleet/internal/runtime"
)

// sandboxFS is the Worker-local filesystem view of one running sandbox.
// CRI parents and restored children implement the same surface so files.go
// does not branch on how the sandbox was started.
type sandboxFS interface {
	Exists(ctx context.Context, absPath string) (bool, error)
	List(ctx context.Context, absPath string) ([]SandboxFileEntry, error)
	Read(ctx context.Context, absPath string) ([]byte, error)
	Write(ctx context.Context, absPath string, content []byte) error
}

// criSandboxFS talks to a CRI-backed runtime (containerd / runsc via CRI).
type criSandboxFS struct {
	rt sandboxruntime.Runtime
	id sandboxruntime.ID
}

func (f *criSandboxFS) Exists(ctx context.Context, absPath string) (bool, error) {
	return f.rt.FileExists(ctx, f.id, absPath)
}

func (f *criSandboxFS) List(ctx context.Context, absPath string) ([]SandboxFileEntry, error) {
	entries, err := f.rt.ListFiles(ctx, f.id, absPath)
	if err != nil {
		return nil, err
	}
	out := make([]SandboxFileEntry, 0, len(entries))
	for _, entry := range entries {
		out = append(out, SandboxFileEntry{Name: entry.Name, Type: entry.Type})
	}
	return out, nil
}

func (f *criSandboxFS) Read(ctx context.Context, absPath string) ([]byte, error) {
	return f.rt.ReadFile(ctx, f.id, absPath)
}

func (f *criSandboxFS) Write(ctx context.Context, absPath string, content []byte) error {
	return f.rt.WriteFile(ctx, f.id, absPath, content)
}

// restoredExec runs a command inside a restored sandbox (no CRI handle).
type restoredExec func(ctx context.Context, req sandboxruntime.ExecRequest) (sandboxruntime.ExecResult, error)

// execSandboxFS implements file ops by shelling into a restored instance.
type execSandboxFS struct {
	exec restoredExec
}

func (f *execSandboxFS) Exists(ctx context.Context, absPath string) (bool, error) {
	result, err := f.exec(ctx, sandboxruntime.ExecRequest{
		Command: []string{"sh", "-c", `test -e "$1"`, "exists", absPath},
		Timeout: 15 * time.Second,
	})
	if err != nil {
		return false, err
	}
	return result.ExitCode == 0, nil
}

func (f *execSandboxFS) List(ctx context.Context, absPath string) ([]SandboxFileEntry, error) {
	result, err := f.exec(ctx, sandboxruntime.ExecRequest{
		Command: []string{"sh", "-c", `
if [ ! -d "$1" ]; then
  echo "not a directory" >&2
  exit 2
fi
ls -1A "$1" 2>/dev/null | while IFS= read -r name; do
  [ -n "$name" ] || continue
  if [ -d "$1/$name" ]; then
    printf 'directory\t%s\n' "$name"
  else
    printf 'file\t%s\n' "$name"
  fi
done
`, "list", absPath},
		Timeout: 30 * time.Second,
	})
	if err != nil {
		return nil, err
	}
	if result.ExitCode != 0 {
		return nil, fmt.Errorf("list %q: exit %d stderr=%s", absPath, result.ExitCode, strings.TrimSpace(result.Stderr))
	}
	return parseFileList(result.Stdout), nil
}

func (f *execSandboxFS) Read(ctx context.Context, absPath string) ([]byte, error) {
	result, err := f.exec(ctx, sandboxruntime.ExecRequest{
		Command: []string{"sh", "-c", `base64 "$1"`, "read", absPath},
		Timeout: 60 * time.Second,
	})
	if err != nil {
		return nil, err
	}
	if result.ExitCode != 0 {
		return nil, fmt.Errorf("read %q: exit %d stderr=%s", absPath, result.ExitCode, strings.TrimSpace(result.Stderr))
	}
	encoded := strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' {
			return -1
		}
		return r
	}, result.Stdout)
	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("decode file content: %w", err)
	}
	if len(data) > MaxFileBytes {
		return nil, fmt.Errorf("file exceeds %d bytes", MaxFileBytes)
	}
	return data, nil
}

func (f *execSandboxFS) Write(ctx context.Context, absPath string, content []byte) error {
	encoded := base64.StdEncoding.EncodeToString(content)
	result, err := f.exec(ctx, sandboxruntime.ExecRequest{
		Command: []string{
			"sh", "-c", `mkdir -p "$(dirname -- "$1")" && printf %s "$2" | base64 -d > "$1"`,
			"write", absPath, encoded,
		},
		Timeout: 60 * time.Second,
	})
	if err != nil {
		return err
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("write %q: exit %d stderr=%s", absPath, result.ExitCode, strings.TrimSpace(result.Stderr))
	}
	return nil
}

func parseFileList(stdout string) []SandboxFileEntry {
	lines := strings.Split(stdout, "\n")
	result := make([]SandboxFileEntry, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "\t", 2)
		if len(parts) != 2 {
			continue
		}
		result = append(result, SandboxFileEntry{Type: parts[0], Name: parts[1]})
	}
	return result
}
