//go:build !linux && !darwin && !windows

package downloader

import "errors"

// freeBytes is not implemented on this platform; the space check is
// skipped.
func freeBytes(string) (uint64, error) {
	return 0, errors.New("free space query not supported on this platform")
}
