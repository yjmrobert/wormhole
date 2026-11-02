package update

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// installArchive validates an update archive and atomically replaces the executable.
func installArchive(archivePath, executablePath string) (bool, error) {
	archive, err := os.Open(archivePath)
	if err != nil {
		return false, err
	}
	defer archive.Close()
	compressed, err := gzip.NewReader(archive)
	if err != nil {
		return false, err
	}
	defer compressed.Close()

	staged, err := os.CreateTemp(filepath.Dir(executablePath), ".wormhole-update-*")
	if err != nil {
		return false, err
	}
	stagedPath := staged.Name()
	renamed := false
	defer func() {
		staged.Close()
		if !renamed {
			os.Remove(stagedPath)
		}
	}()

	reader := tar.NewReader(compressed)
	found := false
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return false, err
		}
		name, err := cleanMemberName(header.Name)
		if err != nil {
			return false, err
		}
		if name != binaryName {
			continue
		}
		if header.Typeflag != tar.TypeReg && header.Typeflag != tar.TypeRegA {
			return false, fmt.Errorf("archive member %s is not a regular file", binaryName)
		}
		if found {
			return false, fmt.Errorf("archive contains duplicate %s members", binaryName)
		}
		n, err := io.Copy(staged, io.LimitReader(reader, maxBinarySize+1))
		if err != nil {
			return false, err
		}
		if n > maxBinarySize {
			return false, fmt.Errorf("archive member %s exceeds %d bytes", binaryName, maxBinarySize)
		}
		found = true
	}
	if !found {
		return false, fmt.Errorf("archive does not contain %s", binaryName)
	}
	if err := staged.Chmod(0o755); err != nil {
		return false, err
	}
	if err := staged.Sync(); err != nil {
		return false, err
	}
	if err := staged.Close(); err != nil {
		return false, err
	}
	if err := os.Rename(stagedPath, executablePath); err != nil {
		return false, err
	}
	renamed = true
	if err := syncDir(filepath.Dir(executablePath)); err != nil {
		return true, err
	}
	return true, nil
}

// cleanMemberName rejects absolute paths and traversal in an archive member name.
func cleanMemberName(name string) (string, error) {
	if name == "" || path.IsAbs(name) {
		return "", fmt.Errorf("archive contains unsafe member name %q", name)
	}
	for _, part := range strings.Split(name, "/") {
		if part == ".." {
			return "", fmt.Errorf("archive contains path traversal member %q", name)
		}
	}
	return path.Clean(name), nil
}

// syncDir flushes directory metadata so a completed rename survives a crash.
func syncDir(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
