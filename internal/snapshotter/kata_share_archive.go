package snapshotter

import (
	"archive/tar"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// packRootfsTar packs srcDir into destTar for portable cross-Worker restore.
func packRootfsTar(srcDir, destTar string) error {
	if !dirExists(srcDir) {
		return fmt.Errorf("rootfs source %q is not a directory", srcDir)
	}
	f, err := os.Create(destTar)
	if err != nil {
		return err
	}
	tw := tar.NewWriter(f)
	walkErr := filepath.Walk(srcDir, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(srcDir, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		// Skip virtual FS dirs under container rootfs.
		if skipVirtualFSPath(rel) {
			if info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		header, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		header.Name = filepath.ToSlash(rel)
		if info.Mode()&os.ModeSymlink != 0 {
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			header.Linkname = target
		}
		if err := tw.WriteHeader(header); err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		in, err := os.Open(path)
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(tw, in)
		_ = in.Close()
		return copyErr
	})
	closeErr := tw.Close()
	fileErr := f.Close()
	if walkErr != nil {
		return walkErr
	}
	if closeErr != nil {
		return closeErr
	}
	return fileErr
}

func skipVirtualFSPath(rel string) bool {
	rel = filepath.ToSlash(rel)
	segs := strings.Split(rel, "/")
	for i, s := range segs {
		if s != "proc" && s != "sys" && s != "dev" {
			continue
		}
		// Share-root virtual FS (proc/..., sys/..., dev/...).
		if i == 0 {
			return true
		}
		// Container rootfs placeholders: .../rootfs/{proc,sys,dev}.
		if segs[i-1] == "rootfs" {
			return true
		}
	}
	return false
}

// unpackRootfsTar unpacks tarPath into dstDir (created if missing).
func unpackRootfsTar(tarPath, dstDir string) error {
	if err := os.MkdirAll(dstDir, 0o755); err != nil {
		return err
	}
	f, err := os.Open(tarPath)
	if err != nil {
		return err
	}
	defer f.Close()
	tr := tar.NewReader(f)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		name := filepath.Clean(hdr.Name)
		if name == "." || strings.HasPrefix(name, "..") {
			continue
		}
		target := filepath.Join(dstDir, name)
		if !strings.HasPrefix(target, filepath.Clean(dstDir)+string(os.PathSeparator)) && target != filepath.Clean(dstDir) {
			return fmt.Errorf("tar entry escapes destination: %q", hdr.Name)
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg, tar.TypeRegA:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(hdr.Mode)&0o777)
			if err != nil {
				return err
			}
			_, copyErr := io.Copy(out, tr)
			closeErr := out.Close()
			if copyErr != nil {
				return copyErr
			}
			if closeErr != nil {
				return closeErr
			}
		case tar.TypeSymlink:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			_ = os.Remove(target)
			if err := os.Symlink(hdr.Linkname, target); err != nil {
				return err
			}
		default:
			// Skip devices/sockets/fifos; find-paths mainly needs regular files + dirs.
		}
	}
}

func rootfsTarFileName(index int) string {
	return fmt.Sprintf("rootfs-share-%d.tar", index)
}
