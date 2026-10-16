package dlc

import (
	"context"
	"net/http"
	"os"
	"testing"
	"time"
)

// TestRealFile_Optional decrypts a real, server-issued .dlc file end to
// end against the live JDownloader key service. It's not part of the
// normal test suite's guarantees — network access and a real file are
// both required — so it's skipped unless HOPPER_DLC_TEST_FILE points at
// one. Useful for re-verifying against production data if the crypto
// logic ever needs to change again; see internal/dlc's package doc and
// crypto.go's comments for how the last such change was found (a
// production file surfaced a null-byte-termination detail no reference
// implementation documents explicitly).
func TestRealFile_Optional(t *testing.T) {
	path := os.Getenv("HOPPER_DLC_TEST_FILE")
	if path == "" {
		t.Skip("set HOPPER_DLC_TEST_FILE to a real .dlc file to run this test")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}

	client := &http.Client{Timeout: 30 * time.Second}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	container, err := Parse(ctx, client, "http://service.jdownloader.org/dlcrypt/service.php", data)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(container.Packages) == 0 {
		t.Fatal("Parse succeeded but returned no packages")
	}
	for _, pkg := range container.Packages {
		t.Logf("package %q: %d file(s)", pkg.Name, len(pkg.Files))
		for _, f := range pkg.Files {
			t.Logf("  file: name=%q size=%d url=%s", f.Filename, f.Size, f.URL)
		}
	}
}
