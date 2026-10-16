package dlc

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// aesCBCEncryptPadded is the inverse of aesCBCDecrypt (PKCS7 pad +
// AES-CBC encrypt), used to build the stage-2 (content) fixture the same
// way JDownloader's server would produce it.
func aesCBCEncryptPadded(t *testing.T, plaintext, key, iv []byte) []byte {
	t.Helper()
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	padLen := aes.BlockSize - len(plaintext)%aes.BlockSize
	padded := append(append([]byte{}, plaintext...), bytes.Repeat([]byte{byte(padLen)}, padLen)...)
	out := make([]byte, len(padded))
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(out, padded)
	return out
}

// aesCBCEncryptRaw is the inverse of aesCBCDecryptRaw: no padding is
// added, so plaintext must already be exactly one (or more) AES
// block(s). Used to build the stage-1 (key-unwrap) fixture: the real
// service encrypts the raw 16-byte per-container key with no padding
// (see crypto.go's doc comments for why unpadding it would be wrong).
func aesCBCEncryptRaw(t *testing.T, plaintext, key, iv []byte) []byte {
	t.Helper()
	if len(plaintext)%aes.BlockSize != 0 {
		t.Fatalf("aesCBCEncryptRaw: plaintext length %d is not block-aligned", len(plaintext))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	out := make([]byte, len(plaintext))
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(out, plaintext)
	return out
}

// buildDLCFixture constructs a full, self-consistent .dlc file plus a
// fake JDownloader key-service HTTP handler that answers it correctly,
// mirroring the real protocol end to end as cross-verified against
// pyload's and opendlc's DLC decrypters (see crypto.go) and, for this
// base64-of-XML layer specifically, against a real server-issued .dlc
// file: stage 1 unwraps a raw, unpadded 16-byte per-container key using
// the fixed key/IV pair (two distinct values); stage 2 encrypts
// base64(xml) — not raw xml — using that raw key as both AES key and IV.
func buildDLCFixture(t *testing.T, containerXML string) (fileBytes []byte, server *httptest.Server) {
	t.Helper()
	aesKey := []byte("0123456789abcdef") // 16-byte per-container key (raw, unpadded)

	contentB64 := base64.StdEncoding.EncodeToString([]byte(containerXML))
	dlcData := aesCBCEncryptPadded(t, []byte(contentB64), aesKey, aesKey)
	dlcDataB64 := base64.StdEncoding.EncodeToString(dlcData)

	dlcKey := strings.Repeat("k", dlcKeyLen)

	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := r.URL.Query().Get("data")
		if got != dlcKey {
			t.Errorf("key service received data=%q, want %q", got, dlcKey)
		}
		encKey := aesCBCEncryptRaw(t, aesKey, jdFixedKey, jdFixedIV)
		rc := base64.StdEncoding.EncodeToString(encKey)
		fmt.Fprintf(w, "<rc>%s</rc>", rc)
	}))
	t.Cleanup(server.Close)

	fileBytes = []byte(dlcDataB64 + dlcKey)
	return fileBytes, server
}

func TestParse_FullRoundTrip(t *testing.T) {
	xmlContent := `<dlc><header></header><content>` +
		`<package name="` + b64("My Package") + `" passwords="` + b64("pw1") + `" comment="` + b64("a comment") + `">` +
		`<file><url>` + b64("http://example.com/file1.zip") + `</url>` +
		`<filename>` + b64("file1.zip") + `</filename>` +
		`<size>` + b64("12345") + `</size></file>` +
		`</package></content></dlc>`

	fileBytes, server := buildDLCFixture(t, xmlContent)

	c, err := Parse(context.Background(), server.Client(), server.URL, fileBytes)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(c.Packages) != 1 {
		t.Fatalf("expected 1 package, got %d", len(c.Packages))
	}
	pkg := c.Packages[0]
	if pkg.Name != "My Package" || pkg.Password != "pw1" || pkg.Comment != "a comment" {
		t.Errorf("unexpected package metadata: %+v", pkg)
	}
	if len(pkg.Files) != 1 {
		t.Fatalf("expected 1 file, got %d", len(pkg.Files))
	}
	f := pkg.Files[0]
	if f.URL != "http://example.com/file1.zip" || f.Filename != "file1.zip" || f.Size != 12345 {
		t.Errorf("unexpected file: %+v", f)
	}
}

func TestParse_EmptyPasswordAndComment(t *testing.T) {
	xmlContent := `<dlc><header></header><content>` +
		`<package name="` + b64("No Extras") + `" passwords="" comment="">` +
		`<file><url>` + b64("http://example.com/f") + `</url></file>` +
		`</package></content></dlc>`

	fileBytes, server := buildDLCFixture(t, xmlContent)
	c, err := Parse(context.Background(), server.Client(), server.URL, fileBytes)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	pkg := c.Packages[0]
	if pkg.Password != "" || pkg.Comment != "" {
		t.Errorf("expected empty password/comment, got %+v", pkg)
	}
}

// TestParse_TrailingNullTerminatorAfterPKCS7 reproduces a byte pattern
// confirmed against a real, server-issued .dlc file: JDownloader's
// stage-2 plaintext is base64(xml) followed by a 4-byte null terminator,
// *then* standard-PKCS7-padded. Stripping PKCS7 alone leaves those
// trailing 0x00 bytes in place, which Go's strict base64 decoder rejects
// ("illegal base64 data") — decryptContainerXML must trim them before
// decoding. Uses a content length chosen so the null terminator lands
// the combined length exactly on a 16-byte boundary, forcing PKCS7 to
// add a full extra block — the layout real JDownloader-created .dlc
// files have.
func TestParse_TrailingNullTerminatorAfterPKCS7(t *testing.T) {
	aesKey := []byte("0123456789abcdef")

	xmlContent := `<dlc><content><package name="` + b64("Trailing Nulls") + `" passwords="" comment="">` +
		`<file><url>` + b64("http://example.com/f.rar") + `</url></file>` +
		`</package></content></dlc>`
	contentB64 := base64.StdEncoding.EncodeToString([]byte(xmlContent))

	// Append JDownloader's null terminator, then pad however far short
	// of a block boundary that leaves us (standard PKCS7: if already
	// aligned, add a full extra block) — this is exactly the scenario
	// aesCBCEncryptPadded already implements, so just feed it the
	// content+terminator directly rather than hand-computing alignment.
	plaintext := append([]byte(contentB64), 0, 0, 0, 0)

	dlcData := aesCBCEncryptPadded(t, plaintext, aesKey, aesKey)
	dlcDataB64 := base64.StdEncoding.EncodeToString(dlcData)
	dlcKey := strings.Repeat("k", dlcKeyLen)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		encKey := aesCBCEncryptRaw(t, aesKey, jdFixedKey, jdFixedIV)
		rc := base64.StdEncoding.EncodeToString(encKey)
		fmt.Fprintf(w, "<rc>%s</rc>", rc)
	}))
	t.Cleanup(server.Close)

	fileBytes := []byte(dlcDataB64 + dlcKey)
	c, err := Parse(context.Background(), server.Client(), server.URL, fileBytes)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(c.Packages) != 1 || c.Packages[0].Name != "Trailing Nulls" {
		t.Fatalf("unexpected result: %+v", c)
	}
	if len(c.Packages[0].Files) != 1 || c.Packages[0].Files[0].URL != "http://example.com/f.rar" {
		t.Fatalf("unexpected files: %+v", c.Packages[0].Files)
	}
}

// TestDecryptContainerXML_OutputIsValidXML exercises just the
// decrypt-trim-decode step in isolation (no HTTP, no full Parse) and
// checks the result actually looks like XML before any parser sees it —
// the concrete regression check for the null-byte bug: a leftover null
// byte here would make this fail immediately with a clear signal, rather
// than surfacing as a generic XML-parse error three calls away.
func TestDecryptContainerXML_OutputIsValidXML(t *testing.T) {
	aesKey := []byte("0123456789abcdef")
	xmlContent := `<dlc><content></content></dlc>`
	contentB64 := base64.StdEncoding.EncodeToString([]byte(xmlContent))

	cases := []struct {
		name      string
		plaintext []byte
	}{
		{"no terminator", []byte(contentB64)},
		{"4-byte null terminator", append([]byte(contentB64), 0, 0, 0, 0)},
		{"1-byte null terminator", append([]byte(contentB64), 0)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dlcData := aesCBCEncryptPadded(t, c.plaintext, aesKey, aesKey)
			got, err := decryptContainerXML(dlcData, aesKey)
			if err != nil {
				t.Fatalf("decryptContainerXML: %v", err)
			}
			if !bytes.HasPrefix(got, []byte("<dlc")) && !bytes.HasPrefix(got, []byte("<?xml")) {
				t.Errorf("decrypted output does not start with valid XML: %q", got)
			}
			if bytes.ContainsRune(got, 0) {
				t.Errorf("decrypted output contains a null byte: %q", got)
			}
		})
	}
}

func TestParse_TooShortRejected(t *testing.T) {
	_, err := Parse(context.Background(), http.DefaultClient, "http://unused", []byte("short"))
	if err == nil {
		t.Fatal("expected error for too-short input, got nil")
	}
}

func TestParse_KeyServiceErrorPropagates(t *testing.T) {
	dlcKey := strings.Repeat("k", dlcKeyLen)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(server.Close)

	fileBytes := []byte(base64.StdEncoding.EncodeToString([]byte("irrelevant-but-block-aligned!!!")) + dlcKey)
	_, err := Parse(context.Background(), server.Client(), server.URL, fileBytes)
	if err == nil {
		t.Fatal("expected error when key service fails, got nil")
	}
}

func TestParsePlaintext(t *testing.T) {
	input := "http://example.com/a\r\n# a comment\r\n\r\nhttp://example.com/b\n  \n"
	c := ParsePlaintext([]byte(input))
	if len(c.Packages) != 1 {
		t.Fatalf("expected 1 package, got %d", len(c.Packages))
	}
	files := c.Packages[0].Files
	if len(files) != 2 || files[0].URL != "http://example.com/a" || files[1].URL != "http://example.com/b" {
		t.Fatalf("unexpected files: %+v", files)
	}
}

func TestParsePlaintext_Empty(t *testing.T) {
	c := ParsePlaintext([]byte("\n\n  \n"))
	if len(c.Packages) != 0 {
		t.Fatalf("expected no packages for empty input, got %d", len(c.Packages))
	}
}

func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

func TestExtractURLs(t *testing.T) {
	text := "Part 1: https://rapidgator.net/file/a/x.part1.rar.html, part 2 (https://rapidgator.net/file/b/x.part2.rar.html).\n" +
		"<a href=\"https://ddownload.com/abc\">mirror</a> https://rapidgator.net/file/a/x.part1.rar.html"
	got := ExtractURLs(text)
	want := []string{
		"https://rapidgator.net/file/a/x.part1.rar.html",
		"https://rapidgator.net/file/b/x.part2.rar.html",
		"https://ddownload.com/abc",
	}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}
