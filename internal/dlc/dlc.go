// Package dlc parses JDownloader-compatible .dlc (Download Link Container)
// files, plus a plaintext newline-separated-URLs fallback for containers
// that aren't real DLC files.
package dlc

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/xml"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
)

// dlcKeyLen is the fixed length of the key JDownloader appends to the end
// of every .dlc file's base64 content.
const dlcKeyLen = 88

// File is a single download link within a Package.
type File struct {
	URL      string `json:"url"`
	Filename string `json:"filename,omitempty"`
	Size     int64  `json:"size,omitempty"`
}

// Package groups related Files, optionally password-protected.
type Package struct {
	Name     string `json:"name"`
	Password string `json:"password,omitempty"`
	Comment  string `json:"comment,omitempty"`
	Files    []File `json:"files"`
}

// Container is the fully decoded content of a .dlc file (or its plaintext
// fallback equivalent).
type Container struct {
	Packages []Package `json:"packages"`
}

// xmlFile/xmlPackage/xmlContainer mirror the decrypted DLC XML structure.
// Every text and attribute node in it is base64-encoded — not just
// url/filename/size, but also package name/passwords/comment — per both
// pyload's DLCDecrypter._parse_packages (base64-decodes the "name"
// attribute) and opendlc's Debase class ("every TEXT and ATTRIBUTE nodes
// are obfuscated ... using base64.standard_b64decode"). An earlier
// version of this file assumed name/passwords/comment were plain text,
// which was wrong.
type xmlFile struct {
	URL      string `xml:"url"`
	Filename string `xml:"filename"`
	Size     string `xml:"size"`
}

type xmlPackage struct {
	Name      string    `xml:"name,attr"`
	Passwords string    `xml:"passwords,attr"`
	Comment   string    `xml:"comment,attr"`
	Files     []xmlFile `xml:"file"`
}

type xmlContainer struct {
	XMLName xml.Name `xml:"dlc"`
	Content struct {
		Packages []xmlPackage `xml:"package"`
	} `xml:"content"`
}

// Parse decodes a .dlc file's raw bytes into a Container. It performs the
// full JDownloader-compatible flow: split off the trailing container key,
// fetch the AES key for it from the JDownloader key service, decrypt the
// container XML, and base64-decode each file's fields. apiURL is
// typically config.DLC.JDAPIURL.
func Parse(ctx context.Context, client *http.Client, apiURL string, data []byte) (*Container, error) {
	dlcDataB64, dlcKey, err := splitContainer(data)
	if err != nil {
		return nil, err
	}
	dlcData, err := base64.StdEncoding.DecodeString(dlcDataB64)
	if err != nil {
		return nil, fmt.Errorf("dlc: decode container base64: %w", err)
	}

	aesKey, err := fetchContainerKey(ctx, client, apiURL, dlcKey)
	if err != nil {
		return nil, fmt.Errorf("dlc: fetch container key: %w", err)
	}

	xmlBytes, err := decryptContainerXML(dlcData, aesKey)
	if err != nil {
		return nil, err
	}

	return decodeContainerXML(xmlBytes)
}

// decryptContainerXML decrypts a container's ciphertext (with the
// per-container key from fetchContainerKey, used as both AES key and IV)
// into raw XML bytes, ready for xml.Unmarshal.
//
// aesCBCDecrypt strips the standard PKCS7 block that pads the ciphertext
// to a multiple of 16 bytes, but real containers have a second layer
// underneath: the plaintext JDownloader encrypts is base64(xml) followed
// by a null-byte terminator, *then* padded — so after PKCS7 removal
// there are still trailing 0x00 bytes before the base64 content actually
// ends. Confirmed against a real, server-issued .dlc file: PKCS7-only
// stripping left the result ending in "...==\x00\x00\x00\x00", which
// Go's strict base64 decoder rejects outright ("illegal base64 data").
// Trim both ends defensively; only trailing nulls have been observed in
// practice.
func decryptContainerXML(dlcData, aesKey []byte) ([]byte, error) {
	contentB64, err := aesCBCDecrypt(dlcData, aesKey, aesKey)
	if err != nil {
		return nil, fmt.Errorf("dlc: decrypt container: %w", err)
	}
	contentB64 = bytes.Trim(contentB64, "\x00")

	xmlBytes, err := base64.StdEncoding.DecodeString(string(contentB64))
	if err != nil {
		return nil, fmt.Errorf("dlc: decode decrypted container base64: %w", err)
	}
	return xmlBytes, nil
}

func splitContainer(data []byte) (dlcDataB64, dlcKey string, err error) {
	s := strings.TrimSpace(string(data))
	if len(s) <= dlcKeyLen {
		return "", "", fmt.Errorf("dlc: file too short to contain a valid key (%d bytes)", len(s))
	}
	return s[:len(s)-dlcKeyLen], s[len(s)-dlcKeyLen:], nil
}

func decodeContainerXML(xmlBytes []byte) (*Container, error) {
	var raw xmlContainer
	if err := xml.Unmarshal(xmlBytes, &raw); err != nil {
		return nil, fmt.Errorf("dlc: parse container xml: %w", err)
	}

	c := &Container{}
	for _, p := range raw.Content.Packages {
		name, err := decodeB64Attr(p.Name)
		if err != nil {
			return nil, fmt.Errorf("dlc: decode package name: %w", err)
		}
		password, err := decodeB64Attr(p.Passwords)
		if err != nil {
			return nil, fmt.Errorf("dlc: decode package passwords: %w", err)
		}
		comment, err := decodeB64Attr(p.Comment)
		if err != nil {
			return nil, fmt.Errorf("dlc: decode package comment: %w", err)
		}

		pkg := Package{Name: name, Password: password, Comment: comment}
		for _, f := range p.Files {
			file, err := decodeXMLFile(f)
			if err != nil {
				return nil, err
			}
			pkg.Files = append(pkg.Files, file)
		}
		c.Packages = append(c.Packages, pkg)
	}
	return c, nil
}

// decodeB64Attr base64-decodes a DLC XML text/attribute node. Every such
// node is base64-encoded (see xmlFile/xmlPackage's doc comment); empty
// input decodes to empty output without error, since passwords/comment
// are routinely empty.
func decodeB64Attr(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", nil
	}
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func decodeXMLFile(f xmlFile) (File, error) {
	urlStr, err := decodeB64Attr(f.URL)
	if err != nil {
		return File{}, fmt.Errorf("dlc: decode file url: %w", err)
	}
	if urlStr == "" {
		return File{}, fmt.Errorf("dlc: file has no url")
	}
	out := File{URL: urlStr}

	filename, err := decodeB64Attr(f.Filename)
	if err != nil {
		return File{}, fmt.Errorf("dlc: decode file name: %w", err)
	}
	out.Filename = filename

	if sizeStr, err := decodeB64Attr(f.Size); err == nil && sizeStr != "" {
		out.Size, _ = strconv.ParseInt(sizeStr, 10, 64)
	}
	return out, nil
}

// ParsePlaintext treats data as a fallback link list. Lines starting with
// "#" are comments; from every other line each http(s) URL is taken (see
// ExtractURLs), so a pasted forum post or a "name - url" list works as
// well as a clean one-URL-per-line file. It never returns an error; a file
// with no usable lines yields an empty Container.
func ParsePlaintext(data []byte) *Container {
	normalized := strings.ReplaceAll(string(data), "\r\n", "\n")
	var files []File
	for _, line := range strings.Split(normalized, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		for _, u := range ExtractURLs(line) {
			files = append(files, File{URL: u})
		}
	}
	if len(files) == 0 {
		return &Container{}
	}
	return &Container{Packages: []Package{{Name: "Imported links", Files: files}}}
}

// urlRe matches http(s) URLs inside free text. Deliberately permissive in
// the path (hoster URLs contain all sorts of characters); trailing
// punctuation that is almost always sentence/markup rather than URL is
// trimmed afterwards.
var urlRe = regexp.MustCompile(`(?i)https?://[^\s<>"'\x60]+`)

// ExtractURLs returns every http(s) URL found in text, in order, without
// duplicates.
func ExtractURLs(text string) []string {
	seen := map[string]bool{}
	var out []string
	for _, m := range urlRe.FindAllString(text, -1) {
		m = strings.TrimRight(m, ".,;:!?)]}")
		if m == "" || seen[m] {
			continue
		}
		seen[m] = true
		out = append(out, m)
	}
	return out
}
