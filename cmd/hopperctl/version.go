package main

import "fmt"

// version is set via -ldflags "-X main.version=..." in release builds —
// same pattern as cmd/hopperd/main.go, and already wired in
// docker/Dockerfile and .github/workflows/release.yml.
var version = "dev"

func cmdVersion(args []string) error {
	fmt.Println("hopperctl", version)
	return nil
}
