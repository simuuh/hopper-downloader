package dlc

import (
	"bytes"
	"context"
	"crypto/aes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// These pin down the two properties that were wrong in an earlier
// version of this package (see crypto.go's doc comments): the fixed
// key and IV are distinct values, and stage 1's raw key-unwrap must NOT
// be PKCS7-unpadded.

func TestJDFixedKeyAndIV_AreDistinctAndCorrectLength(t *testing.T) {
	if len(jdFixedKey) != aes.BlockSize {
		t.Errorf("jdFixedKey length = %d, want %d (AES-128)", len(jdFixedKey), aes.BlockSize)
	}
	if len(jdFixedIV) != aes.BlockSize {
		t.Errorf("jdFixedIV length = %d, want %d", len(jdFixedIV), aes.BlockSize)
	}
	if bytes.Equal(jdFixedKey, jdFixedIV) {
		t.Error("jdFixedKey and jdFixedIV must be distinct values, not the same string reused")
	}
}

func TestAesCBCDecryptRaw_DoesNotStripPlausiblePadding(t *testing.T) {
	key := []byte("0123456789abcdef")
	iv := []byte("fedcba9876543210")

	// A 16-byte plaintext whose last byte (0x01) looks exactly like a
	// valid 1-byte PKCS7 pad. aesCBCDecryptRaw must return it untouched;
	// only aesCBCDecrypt (used for stage 2) should ever strip padding.
	plaintext := append([]byte("real-key-material"[:15]), 0x01)
	ciphertext := aesCBCEncryptRaw(t, plaintext, key, iv)

	got, err := aesCBCDecryptRaw(ciphertext, key, iv)
	if err != nil {
		t.Fatalf("aesCBCDecryptRaw: %v", err)
	}
	if !bytes.Equal(got, plaintext) {
		t.Errorf("aesCBCDecryptRaw returned %q, want the full unmodified %q (it must not unpad)", got, plaintext)
	}
	if len(got) != aes.BlockSize {
		t.Errorf("len(got) = %d, want %d", len(got), aes.BlockSize)
	}
}

func TestAesCBCDecrypt_StripsRealPadding(t *testing.T) {
	key := []byte("0123456789abcdef")
	iv := []byte("fedcba9876543210")

	plaintext := []byte("this is not block-aligned")
	ciphertext := aesCBCEncryptPadded(t, plaintext, key, iv)

	got, err := aesCBCDecrypt(ciphertext, key, iv)
	if err != nil {
		t.Fatalf("aesCBCDecrypt: %v", err)
	}
	if !bytes.Equal(got, plaintext) {
		t.Errorf("got %q, want %q", got, plaintext)
	}
}

// fetchContainerKeyUsesDistinctKeyIV is exercised indirectly by
// TestParse_FullRoundTrip (buildDLCFixture encrypts with
// key=jdFixedKey, iv=jdFixedIV using aesCBCEncryptRaw, and Parse only
// succeeds if fetchContainerKey decrypts with that same, non-swapped
// key/IV pair via aesCBCDecryptRaw) — a swapped or identical key/IV
// would decrypt to garbage and fail XML parsing downstream.
func TestFetchContainerKey_WrongIVFailsToProduceUsableKey(t *testing.T) {
	// Sanity-check the asymmetry matters: decrypting with key and IV
	// swapped must not coincidentally produce the same plaintext.
	key := jdFixedKey
	iv := jdFixedIV
	plaintext := []byte("0123456789abcdef")
	ciphertext := aesCBCEncryptRaw(t, plaintext, key, iv)

	swapped, err := aesCBCDecryptRaw(ciphertext, iv, key)
	if err != nil {
		t.Fatalf("aesCBCDecryptRaw: %v", err)
	}
	if bytes.Equal(swapped, plaintext) {
		t.Fatal("decrypting with key/IV swapped should not reproduce the original plaintext")
	}
}

// A failed key-service request doesn't repeat the container key, which
// sits in the request URL (N3).
func TestFetchContainerKey_ErrorOmitsKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			_ = conn.Close()
		}
	}))
	defer srv.Close()

	_, err := fetchContainerKey(context.Background(), srv.Client(), srv.URL, "SECRETCONTAINERKEY")
	if err == nil {
		t.Fatal("fetchContainerKey succeeded against a dropped connection")
	}
	if strings.Contains(err.Error(), "SECRETCONTAINERKEY") {
		t.Fatalf("error leaks the container key: %v", err)
	}
}
