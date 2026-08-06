// Package snapshotstore uploads and downloads sandbox memory snapshots.
//
// Layout matches substrate (atelet): one storagePath holds
//
//	manifest.json
//	<file>.zstd   for each name listed in snapshotFiles
//
// Restore always reads manifest.json first, then downloads only the listed files.
package snapshotstore

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	sandboxv1alpha1 "github.com/AgentNaut/SandboxFleet/api/v1alpha1"
)

const (
	ManifestFileName = "manifest.json"
	zstdSuffix       = ".zstd"
)

// BlobStorage is a tiny key/value blob API (S3, MinIO, GCS, …).
type BlobStorage interface {
	Put(ctx context.Context, key string, body io.Reader, size int64) error
	Get(ctx context.Context, key string) (io.ReadCloser, error)
	Delete(ctx context.Context, key string) error
}

// Manifest is the snapshot "说明书" uploaded as manifest.json.
type Manifest struct {
	RuntimeHandler string                        `json:"runtimeHandler"`
	FormatVersion  string                        `json:"formatVersion"`
	SnapshotFiles  []string                      `json:"snapshotFiles"`
	SourceSandbox  SourceSandbox                 `json:"sourceSandbox"`
	PoolRef        string                        `json:"poolRef,omitempty"`
	Container      sandboxv1alpha1.ContainerSpec `json:"container,omitempty"`
	CreatedAt      time.Time                     `json:"createdAt"`
}

type SourceSandbox struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	UID       string `json:"uid"`
}

// SnapshotStorage speaks the substrate-style multi-object layout.
type SnapshotStorage struct {
	objects BlobStorage
}

func New(objects BlobStorage) *SnapshotStorage {
	return &SnapshotStorage{objects: objects}
}

// Upload uploads every regular file under localDir as <name>.zstd,
// then writes manifest.json last (so restore never sees a half-uploaded set).
func (s *SnapshotStorage) Upload(ctx context.Context, storagePath string, localDir string, manifest Manifest) (sizeBytes int64, err error) {
	prefix := normalizeStoragePath(storagePath)
	files, err := listRegularFiles(localDir)
	if err != nil {
		return 0, err
	}
	if len(files) == 0 {
		return 0, fmt.Errorf("checkpoint dir %q has no files to upload", localDir)
	}
	manifest.SnapshotFiles = files
	if manifest.CreatedAt.IsZero() {
		manifest.CreatedAt = time.Now().UTC()
	}

	var total int64
	for _, name := range files {
		path := filepath.Join(localDir, name)
		n, err := s.uploadZstdFile(ctx, prefix+name+zstdSuffix, path)
		if err != nil {
			return total, fmt.Errorf("upload %s: %w", name, err)
		}
		total += n
	}

	raw, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return total, err
	}
	if err := s.objects.Put(ctx, prefix+ManifestFileName, strings.NewReader(string(raw)), int64(len(raw))); err != nil {
		return total, fmt.Errorf("upload manifest: %w", err)
	}
	return total, nil
}

// Download downloads manifest.json, then each listed file into localDir.
func (s *SnapshotStorage) Download(ctx context.Context, storagePath, localDir string) (Manifest, error) {
	prefix := normalizeStoragePath(storagePath)
	if err := os.MkdirAll(localDir, 0o755); err != nil {
		return Manifest{}, err
	}

	manifest, err := s.GetManifest(ctx, prefix)
	if err != nil {
		return Manifest{}, err
	}
	if len(manifest.SnapshotFiles) == 0 {
		return Manifest{}, fmt.Errorf("manifest at %q lists no snapshotFiles", prefix)
	}

	for _, name := range manifest.SnapshotFiles {
		if err := validateFileName(name); err != nil {
			return Manifest{}, err
		}
		dest := filepath.Join(localDir, name)
		if err := s.downloadZstdFile(ctx, prefix+name+zstdSuffix, dest); err != nil {
			return Manifest{}, fmt.Errorf("download %s: %w", name, err)
		}
	}
	return manifest, nil
}

// GetManifest fetches and parses manifest.json under storagePath.
func (s *SnapshotStorage) GetManifest(ctx context.Context, storagePath string) (Manifest, error) {
	prefix := normalizeStoragePath(storagePath)
	rc, err := s.objects.Get(ctx, prefix+ManifestFileName)
	if err != nil {
		return Manifest{}, fmt.Errorf("get manifest: %w", err)
	}
	defer rc.Close()
	raw, err := io.ReadAll(rc)
	if err != nil {
		return Manifest{}, err
	}
	var manifest Manifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return Manifest{}, fmt.Errorf("parse manifest: %w", err)
	}
	return manifest, nil
}

// Delete removes manifest.json and every listed <file>.zstd.
// Prefer passing snapshotFiles from CR status; if empty, tries reading manifest first.
func (s *SnapshotStorage) Delete(ctx context.Context, storagePath string, snapshotFiles []string) error {
	prefix := normalizeStoragePath(storagePath)
	files := append([]string(nil), snapshotFiles...)
	if len(files) == 0 {
		if manifest, err := s.GetManifest(ctx, prefix); err == nil {
			files = manifest.SnapshotFiles
		}
	}
	var firstErr error
	for _, name := range files {
		if err := validateFileName(name); err != nil {
			continue
		}
		if err := s.objects.Delete(ctx, prefix+name+zstdSuffix); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if err := s.objects.Delete(ctx, prefix+ManifestFileName); err != nil && firstErr == nil {
		firstErr = err
	}
	return firstErr
}

func normalizeStoragePath(path string) string {
	path = strings.TrimSpace(path)
	path = strings.TrimPrefix(path, "/")
	if path != "" && !strings.HasSuffix(path, "/") {
		path += "/"
	}
	return path
}

func listRegularFiles(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var files []string
	for _, e := range entries {
		if e.Type().IsRegular() && e.Name() != ManifestFileName {
			files = append(files, e.Name())
		}
	}
	sort.Strings(files)
	return files, nil
}

func validateFileName(name string) error {
	if name == "" || strings.Contains(name, "/") || strings.Contains(name, "..") {
		return fmt.Errorf("invalid snapshot file name %q", name)
	}
	return nil
}
