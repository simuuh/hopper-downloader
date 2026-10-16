package clicknload

import (
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"encoding/hex"
	"testing"
)

// encryptZeroPadded mimics what real Click'n'Load senders (filecrypt.cc,
// JDownloader's own reference implementation) produce: AES-128-CBC with
// key = IV and the plaintext padded with NUL bytes, not PKCS7.
func encryptZeroPadded(t *testing.T, plaintext, hexKey string) string {
	t.Helper()
	key, _ := hex.DecodeString(hexKey)
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	data := []byte(plaintext)
	if rem := len(data) % aes.BlockSize; rem != 0 {
		data = append(data, make([]byte, aes.BlockSize-rem)...)
	}
	out := make([]byte, len(data))
	cipher.NewCBCEncrypter(block, key).CryptBlocks(out, data)
	return base64.StdEncoding.EncodeToString(out)
}

func TestDecryptCNL2_ZeroPaddedPayload(t *testing.T) {
	const key = "31323334353637383930393837363534"
	plain := "https://rapidgator.net/file/a/x.part1.rar.html\r\nhttps://rapidgator.net/file/b/x.part2.rar.html"
	got, err := decryptCNL2(encryptZeroPadded(t, plain, key), key)
	if err != nil {
		t.Fatalf("decryptCNL2: %v", err)
	}
	if string(got) != plain {
		t.Fatalf("got %q, want %q", got, plain)
	}
}

func TestDecryptCNL2_BlockAlignedZeroPadPayload(t *testing.T) {
	const key = "31323334353637383930393837363534"
	plain := "https://a.example/0123456789abcd" // exactly 32 bytes, no padding at all
	got, err := decryptCNL2(encryptZeroPadded(t, plain, key), key)
	if err != nil {
		t.Fatalf("decryptCNL2: %v", err)
	}
	if string(got) != plain {
		t.Fatalf("got %q", got)
	}
}

func TestCnlPackageName(t *testing.T) {
	rg := func(n string) string { return "https://rapidgator.net/file/abc/" + n + ".html" }
	cases := []struct {
		name, pkgField, source string
		urls                   []string
		want                   string
	}{
		{"explicit package field wins", "My.Release.2026", "https://filecrypt.cc/Container/X.html", []string{rg("a.part1.rar")}, "My.Release.2026"},
		{"generic package field ignored", "Click'n'Load", "", []string{rg("Movie.2026.part1.rar"), rg("Movie.2026.part2.rar")}, "Movie.2026"},
		{"filecrypt source never names a release", "", "https://filecrypt.cc/Container/X.html", []string{rg("Movie.2026.part1.rar"), rg("Movie.2026.part2.rar")}, "Movie.2026"},
		{"several releases: let AddLinks split", "", "https://filecrypt.cc/Container/X.html", []string{rg("a.part1.rar"), rg("b.part1.rar")}, ""},
		{"single plain link: no package", "", "https://site.example/", []string{"https://cdn.example/file.iso"}, ""},
		{"plain links fall back to source host", "", "https://site.example/page", []string{"https://cdn.example/1.iso", "https://cdn.example/2.mkv"}, "site.example"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := cnlPackageName(c.pkgField, c.source, c.urls); got != c.want {
				t.Errorf("cnlPackageName = %q, want %q", got, c.want)
			}
		})
	}
}
