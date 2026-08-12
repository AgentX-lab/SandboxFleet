//go:build linux

// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package ch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"

	"golang.org/x/sys/unix"
)

// MemoryRangesFile is the cloud-hypervisor guest-RAM image inside a snapshot dir.
const MemoryRangesFile = "memory-ranges"

// MergeSparseOverlay reconstructs a COMPLETE memory snapshot from a (possibly
// sparse) post-restore CH snapshot. CH's new snapshot (deltaFile) may contain
// only the pages dirtied since restore; every other page is unchanged from the
// snapshot it restored FROM (baseFile). So the complete current memory =
// baseFile, with deltaFile's populated pages overlaid.
//
// It writes outFile = a sparse copy of baseFile, then overlays every DATA region
// of deltaFile (located via SEEK_DATA/SEEK_HOLE) at the same byte offsets.
// baseFile and deltaFile MUST be flat images of identical size and layout.
//
// Ported from substrate ateom-microvm (Firecracker-style differential snapshot
// on top of CH, which has no native diff snapshot).
func MergeSparseOverlay(ctx context.Context, baseFile, deltaFile, outFile string) error {
	bi, err := os.Stat(baseFile)
	if err != nil {
		return fmt.Errorf("stat base %q: %w", baseFile, err)
	}
	tmp := outFile + ".merge.tmp"
	_ = os.Remove(tmp)
	if o, err := exec.CommandContext(ctx, "cp", "--sparse=always", baseFile, tmp).CombinedOutput(); err != nil {
		return fmt.Errorf("cp base->tmp: %w: %s", err, o)
	}

	d, err := os.Open(deltaFile)
	if err != nil {
		return fmt.Errorf("open delta %q: %w", deltaFile, err)
	}
	defer d.Close()
	di, err := d.Stat()
	if err != nil {
		return err
	}
	if di.Size() != bi.Size() {
		return fmt.Errorf("MergeSparseOverlay: size mismatch base=%d delta=%d", bi.Size(), di.Size())
	}

	o, err := os.OpenFile(tmp, os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer o.Close()

	if _, err := copySparseRegions(d, o); err != nil {
		return err
	}
	if err := o.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, outFile)
}

// MergeDeltaIntoBase overlays deltaFile's populated pages onto baseFile in place
// and leaves the complete merged snapshot at deltaFile's path — same result as
// MergeSparseOverlay, but without copying baseFile's working set on every
// suspend when both paths share a filesystem.
//
// On EXDEV (cross-device rename) it falls back to MergeSparseOverlay.
func MergeDeltaIntoBase(ctx context.Context, baseFile, deltaFile string) error {
	bi, err := os.Stat(baseFile)
	if err != nil {
		return fmt.Errorf("stat base %q: %w", baseFile, err)
	}
	di, err := os.Stat(deltaFile)
	if err != nil {
		return fmt.Errorf("stat delta %q: %w", deltaFile, err)
	}
	if di.Size() != bi.Size() {
		return fmt.Errorf("MergeDeltaIntoBase: size mismatch base=%d delta=%d", bi.Size(), di.Size())
	}

	merged := deltaFile + ".merged.tmp"
	_ = os.Remove(merged)
	if err := os.Rename(baseFile, merged); err != nil {
		if errors.Is(err, unix.EXDEV) {
			return MergeSparseOverlay(ctx, baseFile, deltaFile, deltaFile)
		}
		return fmt.Errorf("rename base->merged: %w", err)
	}

	d, err := os.Open(deltaFile)
	if err != nil {
		return fmt.Errorf("open delta %q: %w", deltaFile, err)
	}
	defer d.Close()
	m, err := os.OpenFile(merged, os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer m.Close()
	if _, err := copySparseRegions(d, m); err != nil {
		return err
	}
	if err := m.Close(); err != nil {
		return err
	}
	// Unlink old delta first, then rename onto the free name — renaming OVER an
	// existing file can force a synchronous writeback of dirty pages on ext4.
	if err := os.Remove(deltaFile); err != nil {
		return fmt.Errorf("remove old delta: %w", err)
	}
	return os.Rename(merged, deltaFile)
}

// copySparseRegions overwrites dst with every populated (non-hole) region of src
// at the same byte offsets, leaving dst's other bytes untouched.
func copySparseRegions(src, dst *os.File) (copied int64, err error) {
	si, err := src.Stat()
	if err != nil {
		return 0, err
	}
	size := si.Size()
	sfd := int(src.Fd())
	buf := make([]byte, 1<<20)
	off := int64(0)
	for off < size {
		ds, err := unix.Seek(sfd, off, unix.SEEK_DATA)
		if err != nil {
			if errors.Is(err, unix.ENXIO) {
				break
			}
			return copied, fmt.Errorf("SEEK_DATA: %w", err)
		}
		de, err := unix.Seek(sfd, ds, unix.SEEK_HOLE)
		if err != nil {
			return copied, fmt.Errorf("SEEK_HOLE: %w", err)
		}
		if _, err := src.Seek(ds, io.SeekStart); err != nil {
			return copied, err
		}
		if _, err := dst.Seek(ds, io.SeekStart); err != nil {
			return copied, err
		}
		remaining := de - ds
		for remaining > 0 {
			n := int64(len(buf))
			if n > remaining {
				n = remaining
			}
			r, err := io.ReadFull(src, buf[:n])
			if r > 0 {
				if _, werr := dst.Write(buf[:r]); werr != nil {
					return copied, werr
				}
				copied += int64(r)
			}
			if err != nil {
				return copied, fmt.Errorf("reading data region: %w", err)
			}
			remaining -= int64(r)
		}
		off = de
	}
	return copied, nil
}
