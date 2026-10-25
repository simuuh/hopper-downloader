package dlc

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
)

// jdFixedKey/jdFixedIV are JDownloader's well-known, publicly
// reverse-engineered AES-128 key and IV (two DISTINCT 16-byte values, not
// the same value reused) for unwrapping the per-container key returned by
// the service.jdownloader.org key-exchange API. Cross-verified against
// two independent open-source reference implementations: pyload
// (github.com/pyload/pyload, src/pyload/plugins/containers/DLC.py) and
// opendlc (github.com/posativ/opendlc, decrypt/dlc.py). Neither is a
// secret Hopper is disclosing: both have been published in
// interoperable open-source DLC readers for years, and JDownloader
// itself hands the underlying container key to any caller of that
// public API. DLC is a link-container format for organizing download
// links, not a DRM or access-control mechanism.
var (
	jdFixedKey = []byte("cb99b5cbc24db398")
	jdFixedIV  = []byte("9bc24cb995cb8db3")
)

// aesCBCDecryptRaw performs a plain AES-CBC decrypt with no padding
// handling: the caller gets back exactly len(ciphertext) bytes of
// plaintext. Used for the stage-1 key-unwrap step, whose 16-byte output
// is raw key material, not padded data — unlike stage 2, it does not
// carry any real padding to remove, and treating its last byte as a
// PKCS7 length would corrupt the key on almost every call (only ~0.4% of
// random 16-byte blocks look like validly-padded data, so this stage
// would fail nearly 100% of the time if unpadded here; verified against
// the reference implementations above, neither of which unpads this
// step).
func aesCBCDecryptRaw(ciphertext, key, iv []byte) ([]byte, error) {
	if len(ciphertext) == 0 || len(ciphertext)%aes.BlockSize != 0 {
		return nil, fmt.Errorf("dlc: ciphertext is not a valid multiple of the AES block size")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("dlc: init cipher: %w", err)
	}
	plaintext := make([]byte, len(ciphertext))
	cipher.NewCBCDecrypter(block, iv).CryptBlocks(plaintext, ciphertext)
	return plaintext, nil
}

// aesCBCDecrypt is aesCBCDecryptRaw plus PKCS7 unpadding, for data that
// really was padded when encrypted (the stage-2 container content: an
// arbitrary-length base64 XML blob padded out to a block boundary).
// Go's base64 decoder is strict and rejects the trailing pad bytes
// outright, unlike Python's lenient b64decode (which the reference
// implementations rely on instead of an explicit unpad call) — so this
// step is required here even though it has no equivalent line in either
// reference implementation.
func aesCBCDecrypt(ciphertext, key, iv []byte) ([]byte, error) {
	plaintext, err := aesCBCDecryptRaw(ciphertext, key, iv)
	if err != nil {
		return nil, err
	}
	return pkcs7Unpad(plaintext)
}

func pkcs7Unpad(data []byte) ([]byte, error) {
	if len(data) == 0 {
		return nil, fmt.Errorf("dlc: empty plaintext")
	}
	padLen := int(data[len(data)-1])
	if padLen == 0 || padLen > len(data) || padLen > aes.BlockSize {
		return nil, fmt.Errorf("dlc: invalid PKCS7 padding")
	}
	if !bytes.Equal(data[len(data)-padLen:], bytes.Repeat([]byte{byte(padLen)}, padLen)) {
		return nil, fmt.Errorf("dlc: invalid PKCS7 padding bytes")
	}
	return data[:len(data)-padLen], nil
}

// keyServiceResponse mirrors the tiny XML envelope
// service.jdownloader.org returns: <rc>base64(...)</rc>.
type keyServiceResponse struct {
	XMLName xml.Name `xml:"rc"`
	Value   string   `xml:",chardata"`
}

// fetchContainerKey exchanges a container's trailing 88-character dlcKey
// for the raw 16-byte key that decrypts its content, via JDownloader's
// public key service. This external call is unavoidable: it's how the
// real DLC format works (see package doc).
//
// The returned key is used by the caller as *both* the AES key and IV
// for stage 2 (content decryption) — the same value for both, not a
// zero IV or a separately-derived one. This and the rest of the flow
// have been cross-verified against pyload's and opendlc's DLC decrypters
// (see jdFixedKey's doc comment), though not against a live server
// response in this environment.
func fetchContainerKey(ctx context.Context, client *http.Client, apiURL, dlcKey string) ([]byte, error) {
	u := apiURL + "?srcType=dlc&destType=pylo&data=" + url.QueryEscape(dlcKey)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, fmt.Errorf("dlc: build key request: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		// net/http's error repeats the URL, container key included.
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return nil, fmt.Errorf("dlc: key service request to %s failed: %w", req.URL.Host, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("dlc: key service returned HTTP %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if err != nil {
		return nil, fmt.Errorf("dlc: read key service response: %w", err)
	}
	var env keyServiceResponse
	if err := xml.Unmarshal(body, &env); err != nil || env.Value == "" {
		return nil, fmt.Errorf("dlc: unexpected key service response")
	}

	rc, err := base64.StdEncoding.DecodeString(env.Value)
	if err != nil {
		return nil, fmt.Errorf("dlc: decode key service response: %w", err)
	}
	if len(rc) < aes.BlockSize {
		return nil, fmt.Errorf("dlc: key service response too short (%d bytes)", len(rc))
	}
	// Reference implementations (pyload) truncate to the first block
	// defensively rather than assuming the response is exactly one
	// block; do the same.
	rc = rc[:aes.BlockSize]

	return aesCBCDecryptRaw(rc, jdFixedKey, jdFixedIV)
}
