//go:build linux || darwin

package downloader

import "golang.org/x/sys/unix"

// freeBytes returns the space in dir's file system available to this
// process (the unprivileged figure, without the root reserve).
func freeBytes(dir string) (uint64, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil {
		return 0, err
	}
	return uint64(st.Bavail) * uint64(st.Bsize), nil //nolint:gosec,unconvert // G115: block counts and sizes are never negative; field types differ per OS
}
