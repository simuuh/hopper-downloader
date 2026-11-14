package account

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"fmt"
)

// aesKeySize is fixed at AES-256; config.Validate already rejects any
// encryption_key that isn't exactly this many bytes, but Manager
// re-checks it directly since key length is a security invariant, not
// just a config nicety.
const aesKeySize = 32

// Cipher versions of a stored record (record.CipherVersion).
const (
	// cipherLegacy: AES-256-GCM keyed with encryption_key as is, no
	// associated data. Only read, to migrate old records.
	cipherLegacy = 0
	// cipherV1: AES-256-GCM keyed with an HKDF-SHA256 derivation of
	// encryption_key, the ciphertext bound to the record's id and hoster
	// key as associated data.
	cipherV1 = 1
)

// hkdfInfo separates the account-secret key from anything else that
// might ever be derived from the same encryption_key.
const hkdfInfo = "hopper account secrets v1"

// ciphers holds the AEADs for every cipher version Manager can read.
type ciphers struct {
	current cipher.AEAD // cipherV1
	legacy  cipher.AEAD // cipherLegacy
}

func newCiphers(key string) (ciphers, error) {
	if len(key) != aesKeySize {
		return ciphers{}, fmt.Errorf("account: encryption key must be %d bytes, got %d", aesKeySize, len(key))
	}
	legacy, err := newAEAD([]byte(key))
	if err != nil {
		return ciphers{}, err
	}
	derived, err := hkdf.Key(sha256.New, []byte(key), nil, hkdfInfo, aesKeySize)
	if err != nil {
		return ciphers{}, fmt.Errorf("account: derive key: %w", err)
	}
	current, err := newAEAD(derived)
	if err != nil {
		return ciphers{}, err
	}
	return ciphers{current: current, legacy: legacy}, nil
}

func newAEAD(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("account: init cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("account: init GCM: %w", err)
	}
	return gcm, nil
}

// recordAAD is the associated data a cipherV1 ciphertext is bound to: a
// secret copied into another record (another id, or another hoster)
// fails authentication instead of decrypting as that account's.
func recordAAD(id, hosterKey string) []byte {
	return []byte("hopper/account/v1\x00" + id + "\x00" + hosterKey)
}

// seal encrypts plaintext with a fresh random nonce and returns
// nonce||ciphertext||tag as a single blob suitable for storage.
func seal(gcm cipher.AEAD, plaintext, aad []byte) ([]byte, error) {
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("account: generate nonce: %w", err)
	}
	return gcm.Seal(nonce, nonce, plaintext, aad), nil
}

// open reverses seal, verifying the authentication tag (over the
// ciphertext and aad) before returning plaintext.
func open(gcm cipher.AEAD, blob, aad []byte) ([]byte, error) {
	ns := gcm.NonceSize()
	if len(blob) < ns {
		return nil, fmt.Errorf("account: ciphertext too short")
	}
	nonce, ciphertext := blob[:ns], blob[ns:]
	plaintext, err := gcm.Open(nil, nonce, ciphertext, aad)
	if err != nil {
		return nil, fmt.Errorf("account: decrypt failed (wrong key or corrupted data): %w", err)
	}
	return plaintext, nil
}
