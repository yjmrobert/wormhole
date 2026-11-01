//go:build !windows

package cli

import (
	"os"
	"syscall"
)

func reexecCurrentProcess() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	return syscall.Exec(exe, os.Args, os.Environ())
}
