package downloader

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

// remoteFile is what probe learned about the file behind a URL.
type remoteFile struct {
	size   int64 // <= 0 if unknown
	ranges bool
	// validator identifies this version of the file, for If-Range and for
	// recognizing it again on resume: a strong ETag if the server sent
	// one, else Last-Modified, else "". A weak ETag can't be used — a
	// server must never honor it in If-Range (RFC 9110 §13.1.5).
	validator string
}

func validatorOf(h http.Header) string {
	if etag := h.Get("ETag"); etag != "" && !strings.HasPrefix(etag, "W/") {
		return etag
	}
	return h.Get("Last-Modified")
}

// probe determines the remote file's size, version validator and whether
// it supports byte range requests, without downloading the body. It tries
// HEAD first (cheap, widely supported) and falls back to a 1-byte ranged
// GET for servers that reject HEAD.
func probe(ctx context.Context, client *http.Client, url string) (remoteFile, error) {
	head, err := probeHead(ctx, client, url)
	if err == nil && head.size > 0 && head.ranges {
		return head, nil
	}
	// Many hoster CDNs answer HEAD without an Accept-Ranges header but
	// still honor Range on GET. Without this second look such downloads
	// ran as a single, non-resumable stream: no parallel chunks, and a
	// dropped connection restarted the whole file from byte 0.
	ranged, rErr := probeRangedGet(ctx, client, url)
	if rErr == nil && (ranged.ranges || head.size <= 0) {
		return ranged, nil
	}
	if err == nil && head.size > 0 {
		head.ranges = false
		return head, nil
	}
	return ranged, rErr
}

func probeHead(ctx context.Context, client *http.Client, url string) (remoteFile, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, url, nil)
	if err != nil {
		return remoteFile{}, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return remoteFile{}, redactErr(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return remoteFile{}, errStatus(resp.StatusCode)
	}
	return remoteFile{
		size:      resp.ContentLength,
		ranges:    resp.Header.Get("Accept-Ranges") == "bytes",
		validator: validatorOf(resp.Header),
	}, nil
}

func probeRangedGet(ctx context.Context, client *http.Client, url string) (remoteFile, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return remoteFile{}, err
	}
	req.Header.Set("Range", "bytes=0-0")
	resp, err := client.Do(req)
	if err != nil {
		return remoteFile{}, redactErr(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusPartialContent {
		return remoteFile{
			size:      parseContentRangeSize(resp.Header.Get("Content-Range")),
			ranges:    true,
			validator: validatorOf(resp.Header),
		}, nil
	}
	if resp.StatusCode >= 400 {
		return remoteFile{}, errStatus(resp.StatusCode)
	}
	// Server ignored the Range request and returned the full body (200).
	return remoteFile{size: resp.ContentLength, validator: validatorOf(resp.Header)}, nil
}

// parseContentRangeSize extracts the total size from a header of the form
// "bytes 0-0/12345". Returns 0 if the total is unknown ("*") or malformed.
func parseContentRangeSize(headerVal string) int64 {
	_, _, total, ok := parseContentRange(headerVal)
	if !ok || total < 0 {
		return 0
	}
	return total
}

// parseContentRange parses a single-range Content-Range header,
// "bytes first-last/total" (RFC 9110 §14.4). total is -1 when the
// server sends "*" (length unknown). ok is false for anything else,
// including unsatisfied-range forms ("bytes */total") and last < first.
func parseContentRange(headerVal string) (first, last, total int64, ok bool) {
	spec, found := strings.CutPrefix(strings.TrimSpace(headerVal), "bytes ")
	if !found {
		return 0, 0, 0, false
	}
	rng, totalStr, found := strings.Cut(spec, "/")
	if !found {
		return 0, 0, 0, false
	}
	firstStr, lastStr, found := strings.Cut(rng, "-")
	if !found {
		return 0, 0, 0, false
	}
	first, err1 := strconv.ParseInt(firstStr, 10, 64)
	last, err2 := strconv.ParseInt(lastStr, 10, 64)
	if err1 != nil || err2 != nil || first < 0 || last < first {
		return 0, 0, 0, false
	}
	total = -1
	if totalStr != "*" {
		t, err := strconv.ParseInt(totalStr, 10, 64)
		if err != nil || t <= last {
			return 0, 0, 0, false
		}
		total = t
	}
	return first, last, total, true
}

func errStatus(code int) error {
	return fmt.Errorf("downloader: probe: unexpected HTTP status %d", code)
}
