package snapshotstore

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestUploadDownloadDeleteRoundTrip(t *testing.T) {
	mem := &memBlobStorage{data: make(map[string][]byte)}
	store := New(mem)

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "checkpoint.img"), []byte("hello-memory"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "extra.bin"), []byte("extra"), 0o600); err != nil {
		t.Fatal(err)
	}

	manifest := Manifest{
		RuntimeHandler: "runsc",
		FormatVersion:  "runsc-checkpoint-v1",
		SourceSandbox:  SourceSandbox{Namespace: "ns", Name: "parent", UID: "uid-1"},
		PoolRef:        "pool",
	}
	size, err := store.Upload(context.Background(), "snapshots/parent/abc", dir, manifest)
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	if size != int64(len("hello-memory")+len("extra")) {
		t.Fatalf("sizeBytes = %d, want %d", size, len("hello-memory")+len("extra"))
	}

	keys := mem.keys()
	wantKeys := map[string]bool{
		"snapshots/parent/abc/manifest.json":      true,
		"snapshots/parent/abc/checkpoint.img.zstd": true,
		"snapshots/parent/abc/extra.bin.zstd":      true,
	}
	if len(keys) != len(wantKeys) {
		t.Fatalf("keys = %v, want %v", keys, wantKeys)
	}
	for _, k := range keys {
		if !wantKeys[k] {
			t.Fatalf("unexpected key %q", k)
		}
	}

	outDir := t.TempDir()
	got, err := store.Download(context.Background(), "snapshots/parent/abc/", outDir)
	if err != nil {
		t.Fatalf("download: %v", err)
	}
	if got.RuntimeHandler != "runsc" || len(got.SnapshotFiles) != 2 {
		t.Fatalf("manifest = %+v", got)
	}
	raw, err := os.ReadFile(filepath.Join(outDir, "checkpoint.img"))
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != "hello-memory" {
		t.Fatalf("checkpoint.img = %q", raw)
	}

	if err := store.Delete(context.Background(), "snapshots/parent/abc/", got.SnapshotFiles); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if len(mem.keys()) != 0 {
		t.Fatalf("keys after delete = %v, want empty", mem.keys())
	}
}

// memBlobStorage is a test-only BlobStorage (kept out of production packages).
type memBlobStorage struct {
	mu   sync.Mutex
	data map[string][]byte
}

func (m *memBlobStorage) Put(_ context.Context, key string, body io.Reader, _ int64) error {
	raw, err := io.ReadAll(body)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.data[key] = raw
	return nil
}

func (m *memBlobStorage) Get(_ context.Context, key string) (io.ReadCloser, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	raw, ok := m.data[key]
	if !ok {
		return nil, fmt.Errorf("object %q not found", key)
	}
	return io.NopCloser(bytes.NewReader(raw)), nil
}

func (m *memBlobStorage) Delete(_ context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.data, key)
	return nil
}

func (m *memBlobStorage) keys() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	keys := make([]string, 0, len(m.data))
	for k := range m.data {
		keys = append(keys, k)
	}
	return keys
}
