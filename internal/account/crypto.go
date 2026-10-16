package account

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"fmt"
)

// aesKeySize is fixed at AES-256; config.Validate already rejects any
// encryption_key that isn't exactly this many bytes, but Manager
// re-checks it directly since key length is a security invariant, not
// just a config nicety.
const aesKeySize = 32

func newAEAD(key string) (cipher.AEAD, error) {
	if len(key) != aesKeySize {
		return nil, fmt.Errorf("account: encryption key must be %d bytes, got %d", aesKeySize, len(key))
	}
	block, err := aes.NewCipher([]byte(key))
	if err != nil {
		return nil, fmt.Errorf("account: init cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("account: init GCM: %w", err)
	}
	return gcm, nil
}

// seal encrypts plaintext with a fresh random nonce and returns
// nonce||ciphertext||tag as a single blob suitable for storage.
func seal(gcm cipher.AEAD, plaintext []byte) ([]byte, error) {
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("account: generate nonce: %w", err)
	}
	return gcm.Seal(nonce, nonce, plaintext, nil), nil
}

// open reverses seal, verifying the authentication tag before returning
// plaintext.
func open(gcm cipher.AEAD, blob []byte) ([]byte, error) {
	ns := gcm.NonceSize()
	if len(blob) < ns {
		return nil, fmt.Errorf("account: ciphertext too short")
	}
	nonce, ciphertext := blob[:ns], blob[ns:]
	plaintext, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return nil, fmt.Errorf("account: decrypt failed (wrong key or corrupted data): %w", err)
	}
	return plaintext, nil
}
