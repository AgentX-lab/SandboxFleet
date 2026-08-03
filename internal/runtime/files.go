package runtime

import (
	"fmt"
	"path"
	"strings"
)

const (
	// DefaultFilesRoot mirrors agent-sandbox example runtimes (/app).
	DefaultFilesRoot = "/app"
	// MaxFileBytes caps read/write payloads for the MVP transport.
	MaxFileBytes = 1 << 20 // 1 MiB

	FileTypeFile      = "file"
	FileTypeDirectory = "directory"
)

// FileEntry is a directory listing item.
type FileEntry struct {
	Name string
	Type string
}

// ResolveUnderRoot maps a sandbox-relative path onto filesRoot.
// Paths are re-rooted under filesRoot; ".." traversal is rejected.
func ResolveUnderRoot(filesRoot, rel string) (string, error) {
	stripped := strings.TrimSpace(rel)
	if stripped == "" {
		return "", fmt.Errorf("path must not be empty")
	}
	normalized := path.Clean(stripped)
	normalized = strings.TrimPrefix(normalized, "/")
	if normalized == ".." || strings.HasPrefix(normalized, "../") {
		return "", fmt.Errorf("path escapes files root")
	}
	for _, part := range strings.Split(normalized, "/") {
		if part == ".." {
			return "", fmt.Errorf("path escapes files root")
		}
	}
	if normalized == "." || normalized == "" {
		return filesRoot, nil
	}
	return path.Join(filesRoot, normalized), nil
}

// ValidateWriteName enforces the Go agent-sandbox Write rule: plain filename only.
func ValidateWriteName(name string) error {
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("write path must not be empty")
	}
	base := path.Base(name)
	if base == "." || base == ".." || base == "/" || base != name {
		return fmt.Errorf("write path %q is not a plain filename", name)
	}
	if strings.Contains(name, "/") || strings.Contains(name, "\\") {
		return fmt.Errorf("write path %q is not a plain filename", name)
	}
	return nil
}
