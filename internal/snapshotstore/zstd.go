package snapshotstore

import (
	"context"
	"fmt"
	"io"
	"os"
	"runtime"

	"github.com/klauspost/compress/zstd"
)

// uploadZstdFile compresses localPath with zstd and uploads it as key.
// Returns the uncompressed (logical) size.
func (s *SnapshotStorage) uploadZstdFile(ctx context.Context, key, localPath string) (int64, error) {
	src, err := os.Open(localPath)
	if err != nil {
		return 0, err
	}
	defer src.Close()

	tmp, err := os.CreateTemp("", "sandboxfleet-zstd-*")
	if err != nil {
		return 0, err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	defer tmp.Close()

	zw, err := zstd.NewWriter(tmp,
		zstd.WithEncoderLevel(zstd.SpeedFastest),
		zstd.WithEncoderConcurrency(runtime.GOMAXPROCS(0)),
	)
	if err != nil {
		return 0, err
	}
	logical, err := io.Copy(zw, src)
	if err != nil {
		zw.Close()
		return 0, err
	}
	if err := zw.Close(); err != nil {
		return 0, err
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		return 0, err
	}
	st, err := tmp.Stat()
	if err != nil {
		return 0, err
	}
	if err := s.objects.Put(ctx, key, tmp, st.Size()); err != nil {
		return 0, err
	}
	return logical, nil
}

func (s *SnapshotStorage) downloadZstdFile(ctx context.Context, key, destPath string) error {
	rc, err := s.objects.Get(ctx, key)
	if err != nil {
		return err
	}
	defer rc.Close()

	zr, err := zstd.NewReader(rc)
	if err != nil {
		return fmt.Errorf("open zstd: %w", err)
	}
	defer zr.Close()

	out, err := os.OpenFile(destPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err := io.Copy(out, zr); err != nil {
		return err
	}
	return nil
}
