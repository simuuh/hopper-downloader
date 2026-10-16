// Package pathsafe provides small, dependency-free helpers for building
// filesystem paths from hoster- or user-controlled input (filenames,
// package names) without allowing path traversal outside a configured
// base directory. Every code path in Hopper that turns untrusted text
// into a filesystem path goes through this package.
package pathsafe

import (
	"errors"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

// ErrEscapesBase is returned by SafeJoin when the resulting path would
// resolve outside of base.
var ErrEscapesBase = errors.New("pathsafe: resulting path escapes base directory")

// SafeJoin joins base with one or more untrusted path elements and
// rejects any result that would escape base, whether via ".." segments,
// an absolute-looking element, or any other lexical trick. Callers must
// still treat the returned path as untrusted with respect to symlinks if
// an attacker can influence the target filesystem (Hopper's download
// roots are not user-writable outside of Hopper itself, so this is not
// currently mitigated separately).
func SafeJoin(base string, elem ...string) (string, error) {
	if !filepath.IsAbs(base) {
		return "", errors.New("pathsafe: base must be an absolute path")
	}
	base = filepath.Clean(base)
	joined := filepath.Clean(filepath.Join(append([]string{base}, elem...)...))

	rel, err := filepath.Rel(base, joined)
	if err != nil {
		return "", ErrEscapesBase
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", ErrEscapesBase
	}
	return joined, nil
}

// SanitizeFilename reduces name to a single path element that is safe to
// create on Linux, macOS and Windows: directory components (either slash
// convention), NUL and other control characters are removed; characters
// Windows forbids in names (<>:"|?*) become "_"; trailing dots/spaces
// (silently dropped by Windows) are trimmed; reserved device names
// (CON, NUL, COM1, ...) get a "_" prefix; and the result is capped at
// maxFilenameBytes, keeping the extension. Falls back to "download" if
// nothing usable remains.
func SanitizeFilename(name string) string {
	name = strings.ReplaceAll(name, "\\", "/")
	name = filepath.Base(name)
	name = strings.Map(func(r rune) rune {
		switch {
		case r < 0x20 || r == 0x7f:
			return -1
		case strings.ContainsRune(`<>:"|?*`, r):
			return '_'
		}
		return r
	}, name)
	name = strings.TrimSpace(name)
	name = strings.TrimRight(name, ". ")
	if name == "" || name == "." || name == ".." || name == string(filepath.Separator) {
		return "download"
	}
	stem := strings.ToUpper(strings.SplitN(name, ".", 2)[0])
	if windowsReserved[stem] {
		name = "_" + name
	}
	return truncateFilename(name, maxFilenameBytes)
}

// maxFilenameBytes keeps names under the 255-byte limit of common
// filesystems, with room for the downloader's ".part.json" suffix.
const maxFilenameBytes = 240

var windowsReserved = map[string]bool{
	"CON": true, "PRN": true, "AUX": true, "NUL": true,
	"COM1": true, "COM2": true, "COM3": true, "COM4": true, "COM5": true, "COM6": true, "COM7": true, "COM8": true, "COM9": true,
	"LPT1": true, "LPT2": true, "LPT3": true, "LPT4": true, "LPT5": true, "LPT6": true, "LPT7": true, "LPT8": true, "LPT9": true,
}

// truncateFilename shortens name to at most max bytes without splitting a
// UTF-8 sequence, preserving the extension when it is short.
func truncateFilename(name string, max int) string {
	if len(name) <= max {
		return name
	}
	ext := filepath.Ext(name)
	if len(ext) > 16 {
		ext = ""
	}
	stem := name[:len(name)-len(ext)]
	limit := max - len(ext)
	for limit > 0 && !utf8.RuneStart(stem[limit]) {
		limit--
	}
	return stem[:limit] + ext
}
