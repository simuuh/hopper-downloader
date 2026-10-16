package plugins

import "testing"

func TestFilenameFromURL(t *testing.T) {
	cases := []struct {
		name string
		url  string
		want string
	}{
		{
			name: "rapidgator info page strips .html",
			url:  "https://rapidgator.net/file/0123456789abcdef0123/00000000-0000-4000-8000-000000000001.part1.rar.html",
			want: "00000000-0000-4000-8000-000000000001.part1.rar",
		},
		{
			name: "plain filename, no .html suffix",
			url:  "https://example.com/downloads/movie.part1.rar",
			want: "movie.part1.rar",
		},
		{
			name: "percent-encoded filename decoded",
			url:  "https://example.com/movie%20name.rar",
			want: "movie name.rar",
		},
		{
			name: "root path yields no usable filename",
			url:  "https://example.com/",
			want: "",
		},
		{
			name: "no path at all",
			url:  "https://example.com",
			want: "",
		},
		{
			name: "invalid URL",
			url:  "://not a url",
			want: "",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := FilenameFromURL(c.url); got != c.want {
				t.Errorf("FilenameFromURL(%q) = %q, want %q", c.url, got, c.want)
			}
		})
	}
}
