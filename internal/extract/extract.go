// Package extract handles automatic post-download archive extraction
// (see config.ExtractConfig): .zip and .tar.{gz,bz2} via the standard
// library, .rar — including multi-part sets — by shelling out to an
// external unrar binary (there is no maintained pure-Go RAR decoder;
// unrar's license permits redistribution of the binary but not of
// decompilation/reimplementation, which is why this isn't stdlib-only
// like the other two formats).
package extract

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/bzip2"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/simuuh/hopper-downloader/internal/pathsafe"
)

// ErrAllPasswordsFailed is returned when every candidate password (see
// Extractor.candidatePasswords) failed to open an encrypted RAR archive.
var ErrAllPasswordsFailed = errors.New("extract: no candidate password opened the archive")

// ErrUnsupportedFormat is returned by Extract for a filename Detect
// doesn't recognize.
var ErrUnsupportedFormat = errors.New("extract: unsupported archive format")

// Result summarizes a successful extraction.
type Result struct {
	// ExtractedFiles lists the full paths of every file written.
	ExtractedFiles []string
	// PasswordUsed is the password that worked, or "" if the archive
	// wasn't encrypted (true for every non-RAR result: archive/zip
	// can't open an encrypted zip at all, so a successful KindZip result
	// is never encrypted).
	PasswordUsed string
}

// Extractor performs extraction. UnrarPath is typically sourced from
// config.ExtractConfig; unlike a caller's password list (see Extract),
// it isn't meant to change live, so it's a plain field rather than a
// per-call parameter.
type Extractor struct {
	// UnrarPath is the unrar binary to invoke for .rar archives; "" is
	// treated as "unrar" (resolved via PATH).
	UnrarPath string
}

// Extract extracts the archive at srcPath into destDir (created if it
// doesn't exist). srcPath's filename (via Detect) selects zip/tar.gz/
// tar.bz2/rar handling; for a multi-part RAR set, srcPath must be the
// first part (IsFirstPart) — unrar locates the remaining parts itself,
// as long as they're in the same directory as srcPath.
//
// pkgPassword, if non-empty, is tried first (see candidatePasswords),
// ahead of userPasswords. Neither
// has any effect on zip/tar.{gz,bz2}, which don't support
// extraction-time passwords in the standard library (an encrypted zip
// fails outright with a clear error rather than silently
// mis-extracting).
//
// userPasswords is a parameter rather than an Extractor field
// deliberately: it's sourced from live settings (PATCH
// /api/v1/settings' extract_passwords, see internal/settings), and a
// single shared *Extractor instance extracting more than one archive
// concurrently would otherwise need its own field mutated per call —
// racy for no benefit, when the caller already has the current value in
// hand at call time.
func (e *Extractor) Extract(ctx context.Context, srcPath, destDir, pkgPassword string, userPasswords []string) (*Result, error) {
	switch Detect(filepath.Base(srcPath)) {
	case KindZip:
		return extractZip(srcPath, destDir)
	case KindTarGz:
		return extractTarGz(srcPath, destDir)
	case KindTarBz2:
		return extractTarBz2(srcPath, destDir)
	case KindRAR:
		return e.extractRAR(ctx, srcPath, destDir, candidatePasswords(pkgPassword, userPasswords))
	default:
		return nil, fmt.Errorf("%w: %s", ErrUnsupportedFormat, filepath.Base(srcPath))
	}
}

// candidatePasswords builds the try-order: no password first (most
// archives aren't encrypted — trying "" first, rather than jumping
// straight to a real candidate, keeps Result.PasswordUsed accurate for
// that common case instead of reporting some arbitrary password that
// was merely tried, not actually needed), then the package password,
// then userPasswords — de-duplicated after the leading "" (in first-occurrence order) so an
// entry appearing in more than one source is only ever attempted once.
func candidatePasswords(pkgPassword string, userPasswords []string) []string {
	seen := map[string]bool{"": true}
	out := []string{""}
	add := func(p string) {
		if p == "" || seen[p] {
			return
		}
		seen[p] = true
		out = append(out, p)
	}
	add(pkgPassword)
	for _, p := range userPasswords {
		add(p)
	}
	return out
}

func extractZip(srcPath, destDir string) (*Result, error) {
	r, err := zip.OpenReader(srcPath)
	if err != nil {
		return nil, fmt.Errorf("extract: open zip: %w", err)
	}
	defer func() { _ = r.Close() }()

	var extracted []string
	for _, f := range r.File {
		dest, err := pathsafe.SafeJoin(destDir, f.Name)
		if err != nil {
			return nil, fmt.Errorf("extract: zip entry %q: %w", f.Name, err)
		}
		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(dest, 0o750); err != nil {
				return nil, fmt.Errorf("extract: create directory: %w", err)
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(dest), 0o750); err != nil {
			return nil, fmt.Errorf("extract: create directory: %w", err)
		}
		if err := writeZipEntry(f, dest); err != nil {
			return nil, err
		}
		extracted = append(extracted, dest)
	}
	return &Result{ExtractedFiles: extracted}, nil
}

func writeZipEntry(f *zip.File, dest string) error {
	rc, err := f.Open()
	if err != nil {
		return fmt.Errorf("extract: open zip entry %q: %w", f.Name, err)
	}
	defer func() { _ = rc.Close() }()

	out, err := os.OpenFile(dest, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("extract: create %q: %w", dest, err)
	}
	defer func() { _ = out.Close() }()

	// Archive contents come from a file the user chose to download, not
	// an untrusted multi-tenant upload — same trust boundary as the
	// download itself, so an unbounded copy (no decompression-bomb cap)
	// is consistent with the rest of Hopper's threat model.
	if _, err := io.Copy(out, rc); err != nil { //nolint:gosec // G110: see comment above
		return fmt.Errorf("extract: write %q: %w", dest, err)
	}
	return nil
}

func extractTarGz(srcPath, destDir string) (*Result, error) {
	f, err := os.Open(srcPath)
	if err != nil {
		return nil, fmt.Errorf("extract: open %q: %w", srcPath, err)
	}
	defer func() { _ = f.Close() }()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return nil, fmt.Errorf("extract: gzip reader: %w", err)
	}
	defer func() { _ = gz.Close() }()
	return extractTar(gz, destDir)
}

func extractTarBz2(srcPath, destDir string) (*Result, error) {
	f, err := os.Open(srcPath)
	if err != nil {
		return nil, fmt.Errorf("extract: open %q: %w", srcPath, err)
	}
	defer func() { _ = f.Close() }()
	return extractTar(bzip2.NewReader(f), destDir)
}

func extractTar(r io.Reader, destDir string) (*Result, error) {
	tr := tar.NewReader(r)
	var extracted []string
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("extract: read tar entry: %w", err)
		}
		dest, err := pathsafe.SafeJoin(destDir, hdr.Name)
		if err != nil {
			return nil, fmt.Errorf("extract: tar entry %q: %w", hdr.Name, err)
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(dest, 0o750); err != nil {
				return nil, fmt.Errorf("extract: create directory: %w", err)
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(dest), 0o750); err != nil {
				return nil, fmt.Errorf("extract: create directory: %w", err)
			}
			if err := writeTarEntry(tr, dest); err != nil {
				return nil, err
			}
			extracted = append(extracted, dest)
		default:
			// Symlinks and other special entry types: skipped, not
			// relevant for typical downloaded-archive contents, and
			// this avoids replicating tar's own symlink-traversal
			// footguns on top of the path-traversal protection above.
		}
	}
	return &Result{ExtractedFiles: extracted}, nil
}

func writeTarEntry(tr *tar.Reader, dest string) error {
	out, err := os.OpenFile(dest, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("extract: create %q: %w", dest, err)
	}
	defer func() { _ = out.Close() }()
	if _, err := io.Copy(out, tr); err != nil { //nolint:gosec // G110: see extractZip's writeZipEntry comment
		return fmt.Errorf("extract: write %q: %w", dest, err)
	}
	return nil
}

// extractRAR tries each candidate password (see candidatePasswords) in
// order, stopping at the first one that works. It distinguishes a wrong
// password (try the next one) from a real failure — missing unrar
// binary, corrupt archive, disk full — which aborts immediately rather
// than burning through the whole password list for an error no password
// would fix.
func (e *Extractor) extractRAR(ctx context.Context, srcPath, destDir string, passwords []string) (*Result, error) {
	if len(passwords) == 0 {
		passwords = []string{""}
	}

	var lastErr error
	for _, pw := range passwords {
		files, err := e.runUnrar(ctx, srcPath, destDir, pw)
		if err == nil {
			return &Result{ExtractedFiles: files, PasswordUsed: pw}, nil
		}
		if !isWrongPasswordErr(err) {
			return nil, err
		}
		lastErr = err
	}
	return nil, fmt.Errorf("%w (tried %d candidate password(s)): %w", ErrAllPasswordsFailed, len(passwords), lastErr)
}

func (e *Extractor) runUnrar(ctx context.Context, srcPath, destDir, password string) ([]string, error) {
	unrarPath := e.UnrarPath
	if unrarPath == "" {
		unrarPath = "unrar"
	}
	if err := os.MkdirAll(destDir, 0o750); err != nil {
		return nil, fmt.Errorf("extract: create destination directory: %w", err)
	}

	// unrar's own text output format isn't stable across versions, so
	// success is measured by diffing destDir's contents before/after
	// rather than parsing stdout.
	before, err := snapshotFiles(destDir)
	if err != nil {
		return nil, err
	}

	args := []string{"x", "-o+", "-y"}
	if password != "" {
		args = append(args, "-p"+password)
	} else {
		// -p- means "never prompt for a password" — without it, unrar
		// blocks waiting on stdin for an encrypted archive with no -p
		// given, hanging this call indefinitely.
		args = append(args, "-p-")
	}
	args = append(args, srcPath, destDir+string(os.PathSeparator))

	cmd := exec.CommandContext(ctx, unrarPath, args...) //nolint:gosec // G204: unrarPath is operator-configured (config.ExtractConfig.UnrarPath), not user input
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	cmd.Stdin = nil // never attached; -p- above is the real guard against a blocking password prompt

	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("extract: unrar: %w: %s", err, strings.TrimSpace(out.String()))
	}

	after, err := snapshotFiles(destDir)
	if err != nil {
		return nil, err
	}
	var newFiles []string
	for path := range after {
		if !before[path] {
			newFiles = append(newFiles, path)
		}
	}
	sort.Strings(newFiles)
	return newFiles, nil
}

func snapshotFiles(dir string) (map[string]bool, error) {
	files := make(map[string]bool)
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			files[path] = true
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("extract: scan destination directory: %w", err)
	}
	return files, nil
}

// isWrongPasswordErr heuristically recognizes unrar's wrong-password
// output. unrar has no single stable exit code for this across versions
// (a bad password typically surfaces as a checksum/CRC error, the same
// symptom a corrupt archive produces), so this is a best-effort text
// match, not a guarantee — worst case, a genuinely corrupt archive gets
// (harmlessly) retried against a few more passwords before failing.
func isWrongPasswordErr(err error) bool {
	msg := strings.ToLower(err.Error())
	for _, marker := range []string{"wrong password", "password incorrect", "encrypted", "checksum error", "crc failed"} {
		if strings.Contains(msg, marker) {
			return true
		}
	}
	return false
}
