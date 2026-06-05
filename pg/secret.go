package pg

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"database/sql/driver"
	"encoding/gob"
	"errors"
	"fmt"
	"io"
	"sync"
)

// Keyring abstracts the encryption operation drops uses for
// column-level secrets. Production keyrings wrap a KMS (AWS KMS,
// GCP KMS, HashiCorp Vault, ...) and rotate keys behind the
// scenes; drops ships AESGCMKeyring as a zero-dep default for
// dev / smaller deployments.
type Keyring interface {
	// Encrypt seals plaintext. The returned bytes must include any
	// nonce / authentication tag — drops treats them as opaque.
	Encrypt(plaintext []byte) ([]byte, error)
	// Decrypt undoes Encrypt. Implementations should return a
	// distinct error (typed or sentinel) on tampered / corrupted
	// ciphertext so callers can react.
	Decrypt(ciphertext []byte) ([]byte, error)
}

// ----------------------------------------------------------------------
// AES-256-GCM default keyring
// ----------------------------------------------------------------------

// AESGCMKeyring returns a Keyring backed by AES-GCM with the
// supplied key (16, 24 or 32 bytes for AES-128/192/256). Nonces are
// 96-bit random per Seal and prepended to the ciphertext.
//
// For production: derive the key from a KMS-backed wrapping key;
// rotate by re-encrypting columns under a new ring and swapping it
// in via SetKeyring.
func AESGCMKeyring(key []byte) (Keyring, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &aesGCMKeyring{gcm: gcm}, nil
}

type aesGCMKeyring struct {
	gcm cipher.AEAD
}

// ErrCiphertextTooShort is returned when Decrypt receives bytes
// smaller than a nonce — typically a sign of a corrupt or
// truncated row.
var ErrCiphertextTooShort = errors.New("drops/pg: ciphertext too short")

func (k *aesGCMKeyring) Encrypt(plaintext []byte) ([]byte, error) {
	nonce := make([]byte, k.gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}
	return k.gcm.Seal(nonce, nonce, plaintext, nil), nil
}

func (k *aesGCMKeyring) Decrypt(ciphertext []byte) ([]byte, error) {
	if len(ciphertext) < k.gcm.NonceSize() {
		return nil, ErrCiphertextTooShort
	}
	nonce, ct := ciphertext[:k.gcm.NonceSize()], ciphertext[k.gcm.NonceSize():]
	return k.gcm.Open(nil, nonce, ct, nil)
}

// ----------------------------------------------------------------------
// Active keyring registry
// ----------------------------------------------------------------------

var (
	keyringMu sync.RWMutex
	activeRing Keyring
)

// SetKeyring registers the active keyring used by Secret[T] for
// (de)serialisation. Call once at startup, before issuing the
// first query. Passing nil clears the keyring — subsequent
// Secret[T].Value / Scan calls then return ErrNoKeyring.
func SetKeyring(k Keyring) {
	keyringMu.Lock()
	activeRing = k
	keyringMu.Unlock()
}

// ActiveKeyring returns the registered keyring or nil.
func ActiveKeyring() Keyring {
	keyringMu.RLock()
	defer keyringMu.RUnlock()
	return activeRing
}

// ErrNoKeyring signals that a Secret[T] was used without
// SetKeyring having been called.
var ErrNoKeyring = errors.New("drops/pg: no Keyring registered; call SetKeyring first")

// ----------------------------------------------------------------------
// Secret[T]
// ----------------------------------------------------------------------

// Secret wraps a value of type T so it round-trips as encrypted
// bytea through the driver. Encrypt on Value(), decrypt on Scan().
// The wire format is AES-GCM(gob-encoded T), keyed by the active
// keyring set via SetKeyring.
//
//	type User struct {
//	    ID    int64             `drop:"id,primaryKey,autoIncrement"`
//	    Email pg.Secret[string] `drop:"email"`
//	}
//
// AutoTable recognises Secret[T] and emits a bytea column. Build
// the value via NewSecret(v) or by assigning to Secret[T]{Plain: v}.
type Secret[T any] struct {
	// Plain is the un-encrypted value. Reads / writes through this
	// field are normal Go field access; encryption / decryption
	// happens only when the secret crosses the driver boundary via
	// Value() / Scan().
	Plain T
}

// NewSecret is a tiny constructor that helps with type inference.
func NewSecret[T any](v T) Secret[T] { return Secret[T]{Plain: v} }

// Value implements driver.Valuer. Encrypts s.Plain with the active
// keyring and returns the resulting bytes for storage.
func (s Secret[T]) Value() (driver.Value, error) {
	ring := ActiveKeyring()
	if ring == nil {
		return nil, ErrNoKeyring
	}
	var buf bytes.Buffer
	if err := gob.NewEncoder(&buf).Encode(s.Plain); err != nil {
		return nil, fmt.Errorf("drops/pg: Secret encode: %w", err)
	}
	return ring.Encrypt(buf.Bytes())
}

// Scan implements sql.Scanner. Decrypts the bytea payload with the
// active keyring and decodes it back into s.Plain.
func (s *Secret[T]) Scan(src any) error {
	if src == nil {
		var zero T
		s.Plain = zero
		return nil
	}
	ring := ActiveKeyring()
	if ring == nil {
		return ErrNoKeyring
	}
	var raw []byte
	switch v := src.(type) {
	case []byte:
		raw = v
	case string:
		raw = []byte(v)
	default:
		return fmt.Errorf("drops/pg: Secret.Scan unsupported src type %T", src)
	}
	plain, err := ring.Decrypt(raw)
	if err != nil {
		return fmt.Errorf("drops/pg: Secret decrypt: %w", err)
	}
	if err := gob.NewDecoder(bytes.NewReader(plain)).Decode(&s.Plain); err != nil {
		return fmt.Errorf("drops/pg: Secret decode: %w", err)
	}
	return nil
}
