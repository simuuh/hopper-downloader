//go:build windows

package downloader

import "golang.org/x/sys/windows"

// freeBytes returns the space in dir's volume available to this process
// (honoring per-user quotas).
func freeBytes(dir string) (uint64, error) {
	p, err := windows.UTF16PtrFromString(dir)
	if err != nil {
		return 0, err
	}
	var avail, total, totalFree uint64
	if err := windows.GetDiskFreeSpaceEx(p, &avail, &total, &totalFree); err != nil {
		return 0, err
	}
	return avail, nil
}
