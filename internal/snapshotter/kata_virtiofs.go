package snapshotter

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"time"
)

// rewriteRestoreSockets updates snapshot config.json sockets to the child vmDir
// (vsock, serial, virtiofs). SharedDir/UpperTar/RootfsTar from meta are kept as hints;
// callers must run prepareChildRootfsDirs before starting virtiofsd.
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

// kataRootfsPlan carries image/container ids for substrate-style rootfs rebuild.
type kataRootfsPlan struct {
	containerID string
	appImage    string
}

// childRootfsDir is where the child keeps rootfs share i under its vmDir.
func childRootfsDir(vmDir string, index int) string {
	return filepath.Join(vmDir, "virtiofs", strconv.Itoa(index))
}

// discoverRootfsRelPaths finds */rootfs dirs under shareRoot (heuristic for
// older snapshots that lack Submounts in meta).
func discoverRootfsRelPaths(shareRoot string) []string {
	var out []string
	_ = filepath.WalkDir(shareRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil || !d.IsDir() {
			return nil
		}
		if filepath.Base(path) != "rootfs" {
			return nil
		}
		rel, err := filepath.Rel(shareRoot, path)
		if err != nil || rel == "." {
			return nil
		}
		out = append(out, filepath.ToSlash(rel))
		return filepath.SkipDir
	})
	sort.Strings(out)
	return out
}

// findLiveParentRootfs picks an existing host rootfs dir for one share.
// Prefers meta SharedDir when still present; otherwise matches live parent shares by tag.
func findLiveParentRootfs(share virtiofsShare, live []virtiofsShare) (string, error) {
	if dirExists(share.SharedDir) {
		return share.SharedDir, nil
	}
	for _, l := range live {
		if share.Tag != "" && l.Tag != "" && share.Tag != l.Tag {
			continue
		}
		if dirExists(l.SharedDir) {
			return l.SharedDir, nil
		}
	}
	// Tag mismatch but parent still has a usable share (single kataShared).
	for _, l := range live {
		if dirExists(l.SharedDir) {
			return l.SharedDir, nil
		}
	}
	return "", fmt.Errorf("virtiofs tag %q: sharedDir %q missing and parent rootfs share not found on this Worker", share.Tag, share.SharedDir)
}

func dirExists(path string) bool {
	if path == "" {
		return false
	}
	st, err := os.Stat(path)
	return err == nil && st.IsDir()
}

// waitSocketReady waits until path exists (virtiofsd socket ready before CH starts).
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
