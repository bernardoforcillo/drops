package pg_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"strings"
	"testing"

	"github.com/bernardoforcillo/drops/pg"
)

func TestLocalKMSRoundTripsDEK(t *testing.T) {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	kms, err := pg.NewLocalKMS(key)
	if err != nil {
		t.Fatalf("NewLocalKMS: %v", err)
	}
	dek := []byte("data-encryption-key-32-bytes....")
	wrapped, err := kms.Wrap(context.Background(), dek)
	if err != nil {
		t.Fatalf("Wrap: %v", err)
	}
	got, err := kms.Unwrap(context.Background(), wrapped)
	if err != nil {
		t.Fatalf("Unwrap: %v", err)
	}
	if !bytes.Equal(got, dek) {
		t.Errorf("DEK mismatch: got %x want %x", got, dek)
	}
}

func TestNewLocalKMSRejectsBadKeyLength(t *testing.T) {
	if _, err := pg.NewLocalKMS([]byte("too-short")); err == nil {
		t.Error("expected error for short key")
	}
}

func TestEnvelopeCipherRoundTripsPlaintext(t *testing.T) {
	key := make([]byte, 32)
	_, _ = rand.Read(key)
	kms, _ := pg.NewLocalKMS(key)
	cipher := pg.NewEnvelopeCipher(kms)

	plain := []byte("ssn:123-45-6789")
	ct, err := cipher.Encrypt(context.Background(), plain)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if bytes.Equal(ct, plain) {
		t.Error("ciphertext == plaintext")
	}
	got, err := cipher.Decrypt(context.Background(), ct)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if !bytes.Equal(got, plain) {
		t.Errorf("plaintext mismatch: got %q want %q", got, plain)
	}
}

func TestEnvelopeCipherProducesDifferentCiphertextEachCall(t *testing.T) {
	key := make([]byte, 32)
	_, _ = rand.Read(key)
	kms, _ := pg.NewLocalKMS(key)
	cipher := pg.NewEnvelopeCipher(kms)

	plain := []byte("same plaintext")
	a, _ := cipher.Encrypt(context.Background(), plain)
	b, _ := cipher.Encrypt(context.Background(), plain)
	if bytes.Equal(a, b) {
		t.Error("ciphertext must vary across calls (fresh DEK + nonce)")
	}
}

func TestEnvelopeCipherRejectsBadMagic(t *testing.T) {
	key := make([]byte, 32)
	_, _ = rand.Read(key)
	kms, _ := pg.NewLocalKMS(key)
	cipher := pg.NewEnvelopeCipher(kms)

	garbage := []byte{0xFF, 0x00, 0x00, 0x00, 0x00}
	_, err := cipher.Decrypt(context.Background(), garbage)
	if err == nil || !strings.Contains(err.Error(), "bad magic") {
		t.Errorf("expected bad-magic error, got %v", err)
	}
}

func TestEnvelopeCipherRejectsTampering(t *testing.T) {
	key := make([]byte, 32)
	_, _ = rand.Read(key)
	kms, _ := pg.NewLocalKMS(key)
	cipher := pg.NewEnvelopeCipher(kms)

	ct, _ := cipher.Encrypt(context.Background(), []byte("plain"))
	// Flip a bit in the tail (ciphertext + tag region).
	ct[len(ct)-1] ^= 0x01
	if _, err := cipher.Decrypt(context.Background(), ct); err == nil {
		t.Error("expected AEAD tag failure on tampered ciphertext")
	}
}

func TestEnvelopeCipherSurfacesKMSWrapError(t *testing.T) {
	cipher := pg.NewEnvelopeCipher(&failingKMS{wrapErr: errors.New("hsm offline")})
	_, err := cipher.Encrypt(context.Background(), []byte("x"))
	if err == nil || !strings.Contains(err.Error(), "hsm offline") {
		t.Errorf("expected wrap error to surface, got %v", err)
	}
}

func TestEnvelopeCipherSurfacesKMSUnwrapError(t *testing.T) {
	key := make([]byte, 32)
	_, _ = rand.Read(key)
	localKMS, _ := pg.NewLocalKMS(key)
	ct, _ := pg.NewEnvelopeCipher(localKMS).Encrypt(context.Background(), []byte("x"))

	bad := pg.NewEnvelopeCipher(&failingKMS{unwrapErr: errors.New("hsm denied")})
	_, err := bad.Decrypt(context.Background(), ct)
	if err == nil || !strings.Contains(err.Error(), "hsm denied") {
		t.Errorf("expected unwrap error to surface, got %v", err)
	}
}

func TestEnvelopeCipherRejectsShortBlob(t *testing.T) {
	key := make([]byte, 32)
	_, _ = rand.Read(key)
	kms, _ := pg.NewLocalKMS(key)
	cipher := pg.NewEnvelopeCipher(kms)
	if _, err := cipher.Decrypt(context.Background(), []byte{0x01, 0x00}); err == nil {
		t.Error("expected error on short blob")
	}
}

// failingKMS returns canned errors so tests can exercise the
// wrap/unwrap failure paths.
type failingKMS struct {
	wrapErr   error
	unwrapErr error
}

func (k *failingKMS) Wrap(_ context.Context, dek []byte) ([]byte, error) {
	if k.wrapErr != nil {
		return nil, k.wrapErr
	}
	return dek, nil
}

func (k *failingKMS) Unwrap(_ context.Context, wrapped []byte) ([]byte, error) {
	if k.unwrapErr != nil {
		return nil, k.unwrapErr
	}
	return wrapped, nil
}
