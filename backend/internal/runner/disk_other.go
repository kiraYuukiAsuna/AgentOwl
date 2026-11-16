//go:build !linux

package runner

import "errors"

func diskPreflight(string, int64) error {
	return errors.New("production runner requires a Linux host with verified bounded mounts")
}

// Production is rejected by diskPreflight; this permits protocol-only tests on other hosts.
func diskAvailable(string) (int64, error) { return 1<<63 - 1, nil }
