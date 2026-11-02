package update

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

const stateFileName = "update.json"

type state struct {
	Enabled   bool      `json:"enabled"`
	LastCheck time.Time `json:"last_check"`
}

// statePath returns the persistent state file within the configured directory.
func (u *Updater) statePath() string {
	return filepath.Join(u.stateDir, stateFileName)
}

// loadState restores preferences and rejects malformed or non-regular state files.
func (u *Updater) loadState() error {
	if err := secureStateDir(u.stateDir); err != nil {
		return err
	}
	path := u.statePath()
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("stat update state: %w", err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("update state %q is not a regular file", path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read update state: %w", err)
	}
	if err := json.Unmarshal(data, &u.state); err != nil {
		return fmt.Errorf("decode update state: %w", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return fmt.Errorf("secure update state: %w", err)
	}
	return nil
}

// saveState atomically persists state with owner-only file permissions.
func (u *Updater) saveState() error {
	if err := secureStateDir(u.stateDir); err != nil {
		return err
	}
	data, err := json.Marshal(u.state)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	file, err := os.CreateTemp(u.stateDir, ".update-state-*")
	if err != nil {
		return err
	}
	path := file.Name()
	keep := false
	defer func() {
		file.Close()
		if !keep {
			os.Remove(path)
		}
	}()
	if err := file.Chmod(0o600); err != nil {
		return err
	}
	if _, err := file.Write(data); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Rename(path, u.statePath()); err != nil {
		return err
	}
	keep = true
	return syncDir(u.stateDir)
}

// secureStateDir creates the state directory and restricts it to its owner.
func secureStateDir(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return fmt.Errorf("create update state directory: %w", err)
	}
	if err := os.Chmod(path, 0o700); err != nil {
		return fmt.Errorf("secure update state directory: %w", err)
	}
	return nil
}
