package extract

import "testing"

func TestDetect(t *testing.T) {
	cases := map[string]Kind{
		"movie.zip":        KindZip,
		"movie.tar.gz":     KindTarGz,
		"movie.tgz":        KindTarGz,
		"movie.tar.bz2":    KindTarBz2,
		"movie.tbz2":       KindTarBz2,
		"movie.rar":        KindRAR,
		"movie.part01.rar": KindRAR,
		"movie.part1.rar":  KindRAR,
		"movie.r00":        KindRAR,
		"movie.txt":        KindNone,
		"movie":            KindNone,
		"MOVIE.RAR":        KindRAR, // extension matching is case-insensitive
	}
	for name, want := range cases {
		if got := Detect(name); got != want {
			t.Errorf("Detect(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestIsFirstPart(t *testing.T) {
	cases := map[string]bool{
		"movie.part01.rar": true,
		"movie.part1.rar":  true,
		"movie.part02.rar": false,
		"movie.part10.rar": false,
		"movie.rar":        true,  // plain/first-volume name
		"movie.r00":        false, // legacy continuation volume, not the first part
		"movie.zip":        true,  // non-RAR archives are always "their own first part"
		"movie.txt":        false,
	}
	for name, want := range cases {
		if got := IsFirstPart(name); got != want {
			t.Errorf("IsFirstPart(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestGroupBase(t *testing.T) {
	cases := map[string]string{
		"movie.part01.rar": "movie",
		"movie.part1.rar":  "movie",
		"movie.r00":        "movie",
		"movie.rar":        "movie",
		"movie.tar.gz":     "movie",
		"movie.tar.bz2":    "movie",
		"movie.zip":        "movie",
	}
	for name, want := range cases {
		if got := GroupBase(name); got != want {
			t.Errorf("GroupBase(%q) = %q, want %q", name, got, want)
		}
	}
}

func TestPartNumber(t *testing.T) {
	cases := []struct {
		name     string
		filename string
		wantN    int
		wantOK   bool
	}{
		{"part1", "movie.part1.rar", 1, true},
		{"part01 no leading-zero significance", "movie.part01.rar", 1, true},
		{"part014", "movie.part014.rar", 14, true},
		{"legacy r00 is the second file", "movie.r00", 2, true},
		{"legacy r01 is the third file", "movie.r01", 3, true},
		{"plain rar is part 1", "movie.rar", 1, true},
		{"zip has no part concept", "movie.zip", 0, false},
		{"tar.gz has no part concept", "movie.tar.gz", 0, false},
		{"non-archive filename", "movie.mp4", 0, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			n, ok := PartNumber(c.filename)
			if n != c.wantN || ok != c.wantOK {
				t.Errorf("PartNumber(%q) = (%d, %v), want (%d, %v)", c.filename, n, ok, c.wantN, c.wantOK)
			}
		})
	}
}

func TestIsArchive(t *testing.T) {
	if !IsArchive("movie.part02.rar") {
		t.Error("expected a non-first multi-part RAR to still count as an archive")
	}
	if IsArchive("movie.mkv") {
		t.Error("expected a plain media file not to count as an archive")
	}
}
