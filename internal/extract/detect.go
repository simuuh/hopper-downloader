package extract

import (
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// Kind identifies which extractor handles a file, based on its name.
type Kind int

const (
	// KindNone means filename isn't a supported archive.
	KindNone Kind = iota
	KindZip
	KindTarGz
	KindTarBz2
	// KindRAR covers every part of a (possibly multi-part) RAR set —
	// see IsFirstPart, which distinguishes the one to actually hand to
	// unrar (it finds sibling parts itself).
	KindRAR
)

// partNNRe matches the modern multi-part naming scheme, e.g.
// "movie.part01.rar" or "movie.part1.rar" (some tools omit the leading
// zero); rNNRe matches the legacy scheme's continuation volumes, e.g.
// "movie.r00"; rarRe matches a plain/first-volume "movie.rar".
var (
	partNNRe = regexp.MustCompile(`(?i)^(.+)\.part(\d+)\.rar$`)
	rNNRe    = regexp.MustCompile(`(?i)^(.+)\.r(\d+)$`)
	rarRe    = regexp.MustCompile(`(?i)^(.+)\.rar$`)
)

// Detect classifies filename by its extension.
func Detect(filename string) Kind {
	lower := strings.ToLower(filename)
	switch {
	case strings.HasSuffix(lower, ".zip"):
		return KindZip
	case strings.HasSuffix(lower, ".tar.gz"), strings.HasSuffix(lower, ".tgz"):
		return KindTarGz
	case strings.HasSuffix(lower, ".tar.bz2"), strings.HasSuffix(lower, ".tbz2"):
		return KindTarBz2
	case partNNRe.MatchString(filename), rNNRe.MatchString(filename), rarRe.MatchString(filename):
		return KindRAR
	default:
		return KindNone
	}
}

// IsArchive reports whether filename looks like any supported archive
// (including a non-first part of a multi-part RAR set — useful for
// deciding "don't treat this file as a plain download", as distinct
// from IsFirstPart's "extract starting from this file").
func IsArchive(filename string) bool {
	return Detect(filename) != KindNone
}

// IsFirstPart reports whether filename is the part unrar should actually
// be invoked on: "name.part01.rar" (part 1 specifically — unrar finds
// part02 onward itself, as long as they're in the same directory), or a
// plain "name.rar" (the first-volume name in both the legacy .rNN scheme
// and for a non-multi-part archive). Non-RAR archives have no concept of
// "parts" and are always their own first (only) part.
func IsFirstPart(filename string) bool {
	switch Detect(filename) {
	case KindRAR:
		if m := partNNRe.FindStringSubmatch(filename); m != nil {
			n, err := strconv.Atoi(m[2])
			return err == nil && n == 1
		}
		return rarRe.MatchString(filename) // plain .rar or legacy first volume
	case KindZip, KindTarGz, KindTarBz2:
		return true
	default:
		return false
	}
}

// PartNumber returns filename's 1-based position within its multi-part
// RAR archive group (see GroupBase), for ordering parts numerically
// rather than by name or arrival order — used by queue.Manager to admit
// a multi-part set's downloads roughly in order. ".partN.rar" reports N
// directly; the legacy ".rNN" continuation scheme is offset by 2
// (".r00" is the *second* file, right after the base ".rar", which is
// part 1); a plain/first-volume ".rar" is always part 1. Returns
// 0, false for anything without a recognized multi-part position: a
// non-RAR archive (.zip/.tar.gz/.tar.bz2, which have no "parts" concept
// at all — GroupBase still groups them, trivially, with themselves) or
// any filename Detect doesn't classify as RAR.
func PartNumber(filename string) (int, bool) {
	if m := partNNRe.FindStringSubmatch(filename); m != nil {
		n, err := strconv.Atoi(m[2])
		if err != nil {
			return 0, false
		}
		return n, true
	}
	if m := rNNRe.FindStringSubmatch(filename); m != nil {
		n, err := strconv.Atoi(m[2])
		if err != nil {
			return 0, false
		}
		return n + 2, true
	}
	if rarRe.MatchString(filename) {
		return 1, true
	}
	return 0, false
}

// GroupBase returns the shared base name of a (potentially multi-part)
// archive set: "movie" for "movie.part01.rar", "movie.rar", and
// "movie.r00" alike, so a caller can recognize sibling parts (e.g.
// queue.Manager checking whether every part of a package's RAR set has
// finished downloading yet). For a non-RAR archive it's just the
// filename with its extension(s) stripped.
func GroupBase(filename string) string {
	if m := partNNRe.FindStringSubmatch(filename); m != nil {
		return m[1]
	}
	if m := rNNRe.FindStringSubmatch(filename); m != nil {
		return m[1]
	}
	if m := rarRe.FindStringSubmatch(filename); m != nil {
		return m[1]
	}
	lower := strings.ToLower(filename)
	switch {
	case strings.HasSuffix(lower, ".tar.gz"):
		return filename[:len(filename)-len(".tar.gz")]
	case strings.HasSuffix(lower, ".tar.bz2"):
		return filename[:len(filename)-len(".tar.bz2")]
	default:
		return strings.TrimSuffix(filename, filepath.Ext(filename))
	}
}
