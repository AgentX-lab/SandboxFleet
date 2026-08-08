package snapshotter

import "testing"

func TestNormalizeImageRef(t *testing.T) {
	t.Parallel()
	got, err := normalizeImageRef("python:3.12-slim")
	if err != nil {
		t.Fatal(err)
	}
	if got != "docker.io/library/python:3.12-slim" {
		t.Fatalf("got %q", got)
	}
	got, err = normalizeImageRef("registry.k8s.io/pause:3.10")
	if err != nil {
		t.Fatal(err)
	}
	if got != "registry.k8s.io/pause:3.10" {
		t.Fatalf("got %q", got)
	}
}

func TestImageRefCandidates(t *testing.T) {
	t.Parallel()
	got := imageRefCandidates("python:3.12-slim")
	if len(got) < 2 || got[0] != "docker.io/library/python:3.12-slim" || got[1] != "python:3.12-slim" {
		t.Fatalf("got %#v", got)
	}
}
