//go:build !windows

package main

// isWindowsService always reports false outside Windows — there's no
// Service Control Manager to have been launched by. See
// winservice_windows.go for the real implementation.
func isWindowsService() bool {
	return false
}

// runAsWindowsService is unreachable on non-Windows platforms: main only
// calls it when isWindowsService() is true, which it never is here.
func runAsWindowsService() error {
	return nil
}
