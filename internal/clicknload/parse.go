package clicknload

import "strings"

// splitList splits a CNL urls/passwords field on CRLF (and, defensively,
// bare LF) and drops empty entries.
func splitList(raw string) []string {
	raw = strings.ReplaceAll(raw, "\r\n", "\n")
	parts := strings.Split(raw, "\n")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}
