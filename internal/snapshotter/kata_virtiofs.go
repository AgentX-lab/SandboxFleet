package snapshotter

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// rewriteSnapshotSocketPaths repoints snapshot config.json sockets/files into
// vmDir (vsock, serial, each virtio-fs). Returns the planned virtiofs shares
// (SharedDir from meta + new Socket paths) so restore can start matching
// virtiofsd instances. Matching prefers fs tag, then falls back to index —
// substrate matches by tag to avoid crossing shares.
func rewriteSnapshotSocketPaths(snapshotDir, vmDir string, metaShares []virtiofsShare) ([]virtiofsShare, error) {
	cfgPath := filepath.Join(snapshotDir, "config.json")
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		return nil, err
	}
	var cfg map[string]any
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, err
	}
	if vsock, ok := cfg["vsock"].(map[string]any); ok {
		vsock["socket"] = filepath.Join(vmDir, "clh.sock")
	}
	if serial, ok := cfg["serial"].(map[string]any); ok {
		if mode, _ := serial["mode"].(string); mode == "File" {
			serial["file"] = filepath.Join(vmDir, "serial.log")
		}
	}

	byTag := make(map[string]virtiofsShare, len(metaShares))
	for _, s := range metaShares {
		if s.Tag != "" {
			byTag[s.Tag] = s
		}
	}

	var planned []virtiofsShare
	if fss, ok := cfg["fs"].([]any); ok {
		for i, f := range fss {
			fm, ok := f.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("malformed fs device in snapshot config")
			}
			tag, _ := fm["tag"].(string)
			socket := filepath.Join(vmDir, fmt.Sprintf("virtiofsd-%d.sock", i))
			fm["socket"] = socket

			share, ok := byTag[tag]
			if !ok || share.SharedDir == "" {
				if i < len(metaShares) && metaShares[i].SharedDir != "" {
					share = metaShares[i]
				}
			}
			if share.SharedDir == "" {
				return nil, fmt.Errorf("fs[%d] tag %q: no sharedDir in snapshot meta (cannot start virtiofsd)", i, tag)
			}
			share.Tag = tag
			share.Socket = socket
			planned = append(planned, share)
		}
	}

	out, err := json.Marshal(cfg)
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(cfgPath, out, 0o600); err != nil {
		return nil, err
	}
	return planned, nil
}

// waitUnixSocketReady polls until path exists or ctx/deadline fails.
// Mirrors substrate StartVirtiofsd socket wait (restore must not race CH).
func waitUnixSocketReady(ctx context.Context, path string, deadline time.Duration) error {
	end := time.Now().Add(deadline)
	for {
		if st, err := os.Stat(path); err == nil && !st.IsDir() {
			return nil
		}
		if time.Now().After(end) {
			return fmt.Errorf("unix socket %q did not appear within %s", path, deadline)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}
