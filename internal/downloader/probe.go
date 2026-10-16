package downloader

import (
	"context"
	"fmt"
	"net/http"
)

// probe determines the remote file's size and whether it supports byte
// range requests, without downloading the body. It tries HEAD first (cheap,
// widely supported) and falls back to a 1-byte ranged GET for servers that
// reject HEAD.
func probe(ctx context.Context, client *http.Client, url string) (size int64, supportsRange bool, err error) {
	size, supportsRange, err = probeHead(ctx, client, url)
	if err == nil && size > 0 && supportsRange {
		return size, true, nil
	}
	// Many hoster CDNs answer HEAD without an Accept-Ranges header but
	// still honor Range on GET. Without this second look such downloads
	// ran as a single, non-resumable stream: no parallel chunks, and a
	// dropped connection restarted the whole file from byte 0.
	rSize, rRange, rErr := probeRangedGet(ctx, client, url)
	if rErr == nil && (rRange || size <= 0) {
		return rSize, rRange, nil
	}
	if err == nil && size > 0 {
		return size, false, nil
	}
	return rSize, rRange, rErr
}

func probeHead(ctx context.Context, client *http.Client, url string) (int64, bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, url, nil)
	if err != nil {
		return 0, false, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return 0, false, errStatus(resp.StatusCode)
	}
	return resp.ContentLength, resp.Header.Get("Accept-Ranges") == "bytes", nil
}

func probeRangedGet(ctx context.Context, client *http.Client, url string) (int64, bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, false, err
	}
	req.Header.Set("Range", "bytes=0-0")
	resp, err := client.Do(req)
	if err != nil {
		return 0, false, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusPartialContent {
		size := parseContentRangeSize(resp.Header.Get("Content-Range"))
		return size, true, nil
	}
	if resp.StatusCode >= 400 {
		return 0, false, errStatus(resp.StatusCode)
	}
	// Server ignored the Range request and returned the full body (200).
	return resp.ContentLength, false, nil
}

// parseContentRangeSize extracts the total size from a header of the form
// "bytes 0-0/12345". Returns 0 if the total is unknown ("*") or malformed.
func parseContentRangeSize(headerVal string) int64 {
	var start, end, total int64
	n, err := fmt.Sscanf(headerVal, "bytes %d-%d/%d", &start, &end, &total)
	if err != nil || n != 3 {
		return 0
	}
	return total
}

func errStatus(code int) error {
	return fmt.Errorf("downloader: probe: unexpected HTTP status %d", code)
}
