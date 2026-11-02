//go:build windows

package cli

import "errors"

// reexecCurrentProcess reports that Unix process replacement is unavailable on Windows.
func reexecCurrentProcess() error {
	return errors.New("automatic re-execution is unsupported on Windows; run the command again")
}
