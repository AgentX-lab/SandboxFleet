package cri

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"
	"time"

	sandboxruntime "github.com/AgentNaut/SandboxFleet/internal/runtime"
)

// File transport details stay inside the CRI adapter. Worker code must not
// assemble shell snippets for filesystem access.

func (r *Runtime) ReadFile(ctx context.Context, id sandboxruntime.ID, absPath string) ([]byte, error) {
	result, err := r.Exec(ctx, id, sandboxruntime.ExecRequest{
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
	if len(data) > sandboxruntime.MaxFileBytes {
		return nil, fmt.Errorf("file exceeds %d bytes", sandboxruntime.MaxFileBytes)
	}
	return data, nil
}

func (r *Runtime) WriteFile(ctx context.Context, id sandboxruntime.ID, absPath string, content []byte) error {
	if len(content) > sandboxruntime.MaxFileBytes {
		return fmt.Errorf("content exceeds %d bytes", sandboxruntime.MaxFileBytes)
	}
	encoded := base64.StdEncoding.EncodeToString(content)
	result, err := r.Exec(ctx, id, sandboxruntime.ExecRequest{
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

func (r *Runtime) ListFiles(ctx context.Context, id sandboxruntime.ID, absPath string) ([]sandboxruntime.FileEntry, error) {
	result, err := r.Exec(ctx, id, sandboxruntime.ExecRequest{
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
	lines := strings.Split(strings.TrimSuffix(result.Stdout, "\n"), "\n")
	entries := make([]sandboxruntime.FileEntry, 0, len(lines))
	for _, line := range lines {
		if line == "" {
			continue
		}
		kind, name, ok := strings.Cut(line, "\t")
		if !ok || name == "" {
			continue
		}
		if kind != sandboxruntime.FileTypeFile && kind != sandboxruntime.FileTypeDirectory {
			continue
		}
		entries = append(entries, sandboxruntime.FileEntry{Name: name, Type: kind})
	}
	return entries, nil
}

func (r *Runtime) FileExists(ctx context.Context, id sandboxruntime.ID, absPath string) (bool, error) {
	result, err := r.Exec(ctx, id, sandboxruntime.ExecRequest{
		Command: []string{"sh", "-c", `if [ -e "$1" ]; then echo 1; else echo 0; fi`, "exists", absPath},
		Timeout: 30 * time.Second,
	})
	if err != nil {
		return false, err
	}
	if result.ExitCode != 0 {
		return false, fmt.Errorf("exists %q: exit %d stderr=%s", absPath, result.ExitCode, strings.TrimSpace(result.Stderr))
	}
	return strings.TrimSpace(result.Stdout) == "1", nil
}
