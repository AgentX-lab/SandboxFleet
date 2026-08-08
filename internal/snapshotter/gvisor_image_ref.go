package snapshotter

import (
	"fmt"

	"github.com/distribution/reference"
)

// normalizeImageRef expands Docker short names the way CRI/containerd and
// substrate (go-containerregistry name.ParseReference) do, e.g.
// "python:3.12-slim" → "docker.io/library/python:3.12-slim".
func normalizeImageRef(ref string) (string, error) {
	if ref == "" {
		return "", fmt.Errorf("image ref is empty")
	}
	named, err := reference.ParseDockerRef(ref)
	if err != nil {
		return "", err
	}
	return named.String(), nil
}

// imageRefCandidates returns lookup order for containerd GetImage: normalized
// first (matches how CRI stores tags), then the raw ref.
func imageRefCandidates(ref string) []string {
	var out []string
	seen := map[string]struct{}{}
	add := func(s string) {
		if s == "" {
			return
		}
		if _, ok := seen[s]; ok {
			return
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	if n, err := normalizeImageRef(ref); err == nil {
		add(n)
	}
	add(ref)
	return out
}
