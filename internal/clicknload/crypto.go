package clicknload

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"regexp"
)

// jkKeyPattern extracts the hex AES key from a CNL2 "jk" payload, e.g.
//
//	function f(){ return '89abcdef0123456789abcdef01234567'; }
//
// Real JDownloader clients may send a jk that computes its key
// dynamically; Hopper only supports this simple literal-return form,
// which is what every hoster CNL2 button script in practice emits (it's
// also all pyLoad and other JD-compatible servers support).
var jkKeyPattern = regexp.MustCompile(`return\s*['"]([0-9a-fA-F]+)['"]`)

// extractHexKey pulls the hex AES-128 key out of a jk function body.
func extractHexKey(jk string) (string, error) {
	m := jkKeyPattern.FindStringSubmatch(jk)
	if m == nil {
		return "", fmt.Errorf("clicknload: jk does not match the supported literal-return form")
	}
	return m[1], nil
}

// decryptCNL2 reverses CNL2's AES-128-CBC encryption: key and IV are both
// the 16 raw bytes decoded from hexKey, and cryptedB64 is base64(ciphertext)
// with standard PKCS7 padding.
func decryptCNL2(cryptedB64, hexKey string) ([]byte, error) {
	key, err := hex.DecodeString(hexKey)
	if err != nil {
		return nil, fmt.Errorf("clicknload: decode hex key: %w", err)
	}
	if len(key) != 16 {
		return nil, fmt.Errorf("clicknload: key must be 16 bytes (AES-128), got %d", len(key))
	}

	ciphertext, err := base64.StdEncoding.DecodeString(cryptedB64)
	if err != nil {
		return nil, fmt.Errorf("clicknload: decode base64 crypted payload: %w", err)
	}
	if len(ciphertext) == 0 || len(ciphertext)%aes.BlockSize != 0 {
		return nil, fmt.Errorf("clicknload: crypted payload is not a valid multiple of the AES block size")
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("clicknload: init cipher: %w", err)
	}
	plaintext := make([]byte, len(ciphertext))
	cipher.NewCBCDecrypter(block, key).CryptBlocks(plaintext, ciphertext)

	return stripPadding(plaintext)
}

// stripPadding removes the plaintext padding. Real Click'n'Load senders
// (filecrypt.cc and friends, matching JDownloader's own AES/CBC/NoPadding
// decrypt and pyLoad's `.replace("\x00", "")`) pad with NUL bytes, not
// PKCS7 — requiring PKCS7 rejected every such submission as "invalid
// padding". PKCS7 is still accepted when present: its pad bytes (0x01–0x10)
// can never be the legitimate last character of a URL list, so a valid
// PKCS7 trailer is unambiguous.
func stripPadding(data []byte) ([]byte, error) {
	if unpadded, err := pkcs7Unpad(data); err == nil {
		data = unpadded
	}
	data = bytes.TrimRight(data, "\x00")
	if len(data) == 0 {
		return nil, fmt.Errorf("clicknload: empty plaintext")
	}
	return data, nil
}

func pkcs7Unpad(data []byte) ([]byte, error) {
	if len(data) == 0 {
		return nil, fmt.Errorf("clicknload: empty plaintext")
	}
	padLen := int(data[len(data)-1])
	if padLen == 0 || padLen > len(data) || padLen > aes.BlockSize {
		return nil, fmt.Errorf("clicknload: invalid PKCS7 padding")
	}
	if !bytes.Equal(data[len(data)-padLen:], bytes.Repeat([]byte{byte(padLen)}, padLen)) {
		return nil, fmt.Errorf("clicknload: invalid PKCS7 padding bytes")
	}
	return data[:len(data)-padLen], nil
}
