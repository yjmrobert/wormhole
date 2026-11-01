//go:build windows

package cli

import "errors"

func reexecCurrentProcess() error {
	return errors.New("automatic re-execution is unsupported on Windows; run the command again")
}
