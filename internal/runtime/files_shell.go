package runtime

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"
	"time"
)

// Every backend reaches a sandbox filesystem the same way — run `sh` inside it —
// so the snippets and base64 framing live here instead of once per adapter. Only
// the exec transport differs (CRI ExecSync vs kata-agent).

// ExecFunc runs one command in a specific sandbox.
type ExecFunc func(ctx context.Context, req ExecRequest) (ExecResult, error)

func ReadFileVia(ctx context.Context, exec ExecFunc, absPath string) ([]byte, error) {
	result, err := exec(ctx, ExecRequest{
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

func WriteFileVia(ctx context.Context, exec ExecFunc, absPath string, content []byte) error {
	if len(content) > MaxFileBytes {
		return fmt.Errorf("content exceeds %d bytes", MaxFileBytes)
	}
	encoded := base64.StdEncoding.EncodeToString(content)
	want := fmt.Sprintf("%d", len(content))
	// $3 is the expected byte count: after truncating via `>`, we wc -c and
	// refuse success if the guest file did not receive the payload (catches
	// "Write OK / Read empty" where the redirect created a 0-byte file).
	result, err := exec(ctx, ExecRequest{
		Command: []string{
			"sh", "-c",
			`mkdir -p "$(dirname -- "$1")" || exit 1
printf %s "$2" | base64 -d > "$1" || exit 1
got=$(wc -c < "$1" | tr -d ' \t\n')
echo "WRITE_VERIFY path=$1 want=$3 got=$got"
test "$got" = "$3"
`,
			"write", absPath, encoded, want,
		},
		Timeout: 60 * time.Second,
	})
	if err != nil {
		return err
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("write %q: exit %d stdout=%s stderr=%s", absPath, result.ExitCode,
			strings.TrimSpace(result.Stdout), strings.TrimSpace(result.Stderr))
	}
	return nil
}

func ListFilesVia(ctx context.Context, exec ExecFunc, absPath string) ([]FileEntry, error) {
	result, err := exec(ctx, ExecRequest{
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
	entries := make([]FileEntry, 0, len(lines))
	for _, line := range lines {
		if line == "" {
			continue
		}
		kind, name, ok := strings.Cut(line, "\t")
		if !ok || name == "" {
			continue
		}
		if kind != FileTypeFile && kind != FileTypeDirectory {
			continue
		}
		entries = append(entries, FileEntry{Name: name, Type: kind})
	}
	return entries, nil
}

func FileExistsVia(ctx context.Context, exec ExecFunc, absPath string) (bool, error) {
	result, err := exec(ctx, ExecRequest{
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
