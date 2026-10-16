package config

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// hopperdEnvVars returns every HOPPER_* variable applyEnvOverrides reads,
// taken from this package's source so the test can't drift from it.
func hopperdEnvVars(t *testing.T) map[string]bool {
	t.Helper()
	src, err := os.ReadFile("config.go")
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]bool{}
	for _, m := range regexp.MustCompile(`"(HOPPER_[A-Z0-9_]+)"`).FindAllStringSubmatch(string(src), -1) {
		out[m[1]] = true
	}
	return out
}

// TestComposeVariablesNeverLookLikeHopperdSettings guards the naming rule
// of the Docker install: the compose files hand all of .env to the
// container (env_file), so a compose-only variable named like one of
// hopperd's own settings would silently reconfigure hopperd — e.g. a
// host port called HOPPER_PORT changing the port hopperd listens on
// inside the container. Compose variables therefore never start with
// HOPPER_, and every HOPPER_* name in the templates must be a real
// hopperd setting (a typo would be ignored without any error).
func TestComposeVariablesNeverLookLikeHopperdSettings(t *testing.T) {
	known := hopperdEnvVars(t)
	root := filepath.Join("..", "..")

	interp := regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)`)
	envKey := regexp.MustCompile(`(?m)^\s*(HOPPER_[A-Z0-9_]+)\s*:`)
	for _, f := range []string{"docker-compose.yml", filepath.Join("docker", "docker-compose.yml")} {
		raw, err := os.ReadFile(filepath.Join(root, f))
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range interp.FindAllStringSubmatch(string(raw), -1) {
			if strings.HasPrefix(m[1], "HOPPER_") {
				t.Errorf("%s: compose variable ${%s} starts with HOPPER_ — it would also reach hopperd via env_file; rename it", f, m[1])
			}
		}
		for _, m := range envKey.FindAllStringSubmatch(string(raw), -1) {
			if !known[m[1]] {
				t.Errorf("%s: environment sets %s, which hopperd doesn't read", f, m[1])
			}
		}
	}

	raw, err := os.ReadFile(filepath.Join(root, ".env.example"))
	if err != nil {
		t.Fatal(err)
	}
	// Commented-out examples count too: they're what users uncomment.
	line := regexp.MustCompile(`(?m)^#?\s*([A-Za-z_][A-Za-z0-9_]*)=`)
	for _, m := range line.FindAllStringSubmatch(string(raw), -1) {
		if strings.HasPrefix(m[1], "HOPPER_") && !known[m[1]] {
			t.Errorf(".env.example: %s is not a hopperd setting (typo?)", m[1])
		}
	}
}
