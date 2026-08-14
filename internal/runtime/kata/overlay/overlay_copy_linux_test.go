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

package overlay

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestCopyImageRootfsPreservesHardlink(t *testing.T) {
	t.Parallel()
	src := t.TempDir()
	dst := t.TempDir()
	bin := filepath.Join(src, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	busybox := filepath.Join(bin, "busybox")
	if err := os.WriteFile(busybox, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(busybox, filepath.Join(bin, "sleep")); err != nil {
		t.Fatal(err)
	}
	if err := copyImageRootfs(context.Background(), src, dst); err != nil {
		t.Fatalf("copyImageRootfs: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dst, "bin", "sleep"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "x" {
		t.Fatalf("sleep content = %q", got)
	}
	stBox, err := os.Stat(filepath.Join(dst, "bin", "busybox"))
	if err != nil {
		t.Fatal(err)
	}
	stSleep, err := os.Stat(filepath.Join(dst, "bin", "sleep"))
	if err != nil {
		t.Fatal(err)
	}
	if os.SameFile(stBox, stSleep) != true {
		t.Fatal("expected busybox and sleep to stay hardlinked")
	}
}

func TestReconstructSharedDirFromImageEmptyCID(t *testing.T) {
	t.Parallel()
	err := ReconstructSharedDirFromImage(context.Background(), "/tmp", "id", "")
	if err == nil {
		t.Fatal("want error for empty cid")
	}
}
