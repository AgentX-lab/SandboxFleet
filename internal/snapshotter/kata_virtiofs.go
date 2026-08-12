package snapshotter

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// rewriteRestoreSockets repoints the snapshot config.json's per-VMDir paths at
// the restoring instance's dir (vsock, serial, virtiofs sockets) and returns one
// planned share per fs device. SharedDir from meta is only a hint; the caller
// rebuilds the share and overrides it before starting virtiofsd.
func rewriteRestoreSockets(snapshotDir, vmDir string, metaShares []virtiofsShare) ([]virtiofsShare, error) {
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
			if !ok {
				if i < len(metaShares) {
					share = metaShares[i]
				}
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

func dirExists(path string) bool {
	if path == "" {
		return false
	}
	st, err := os.Stat(path)
	return err == nil && st.IsDir()
}

// waitSocketReady waits until path exists (a unix socket a child process binds).
func waitSocketReady(ctx context.Context, path string, deadline time.Duration) error {
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
