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
		if name != "wormhole" {
			continue
		}
		if header.Typeflag != tar.TypeReg && header.Typeflag != tar.TypeRegA {
			return false, fmt.Errorf("archive member wormhole is not a regular file")
		}
		if found {
			return false, errors.New("archive contains duplicate wormhole members")
		}
		if _, err := io.Copy(staged, reader); err != nil {
			return false, err
		}
		found = true
	}
	if !found {
		return false, errors.New("archive does not contain wormhole")
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

func syncDir(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
