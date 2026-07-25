package clickhouse

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// Column-level encryption with envelope encryption — a per-row data
// encryption key (DEK) generated freshly, wrapped by the KMS, and
// stored alongside the ciphertext. Decryption unwraps the DEK and
// decrypts with AES-256-GCM. Rotation only re-wraps DEKs; ciphertexts
// don't need rewriting.
//
// Wire up the cipher via InsertHook / table.OnInsert for transparent
// writes, or call it directly when you need to control the encryption
// boundary:
//
//	// Construct once at startup with your KMS adapter.
//	ec := clickhouse.NewEnvelopeCipher(myKMS)
//
//	// Encrypt before insert — e.g. inside an OnInsert hook.
//	ct, _ := ec.Encrypt(ctx, []byte(row.SSN))
//	row.SSN = string(ct)
//
//	// Decrypt after load.
//	plain, _ := ec.Decrypt(ctx, []byte(row.SSN))
//
// The cipher is goroutine-safe — the DEK is regenerated per call.

// KMS is the key-management contract drops uses for envelope
// encryption. It wraps and unwraps a data encryption key (DEK)
// without exposing the master key to drops.
//
// Real implementations live in user code so drops doesn't carry a
// dependency on any specific KMS SDK. Common adapters are around
// 30 lines each.
type KMS interface {
	// Wrap encrypts dek under the master key. drops calls this
	// once per encrypted column write.
	Wrap(ctx context.Context, dek []byte) ([]byte, error)

	// Unwrap decrypts wrappedDEK. drops calls this once per
	// encrypted column read.
	Unwrap(ctx context.Context, wrappedDEK []byte) ([]byte, error)
}

// EnvelopeCipher performs AES-256-GCM with per-call data encryption
// keys wrapped by the configured KMS. Ciphertext format on the wire:
//
//	[magic: 1 byte] [wrappedDEK length: uint32 BE] [wrappedDEK] [nonce: 12 bytes] [ciphertext + GCM tag]
//
// The header is fixed-length-prefixed so the decryptor can split
// without scanning. The magic byte versions the format.
type EnvelopeCipher struct {
	kms KMS
}

// NewEnvelopeCipher wraps kms in the cipher contract. Panics on a
// nil KMS — encryption with a nil KMS is never a useful programmer
// intent.
func NewEnvelopeCipher(kms KMS) *EnvelopeCipher {
	if kms == nil {
		panic("drops/clickhouse: NewEnvelopeCipher: kms is nil")
	}
	return &EnvelopeCipher{kms: kms}
}

// cipherMagic prefixes every ciphertext so we can detect mismatches
// and version the format.
const cipherMagic byte = 0x01

// Encrypt wraps plaintext into a self-contained ciphertext. The
// returned bytes are safe to store in a String column (as hex) or
// FixedString / LowCardinality column depending on your schema choice.
func (c *EnvelopeCipher) Encrypt(ctx context.Context, plaintext []byte) ([]byte, error) {
	dek := make([]byte, 32) // AES-256
	if _, err := io.ReadFull(rand.Reader, dek); err != nil {
		return nil, err
	}
	wrappedDEK, err := c.kms.Wrap(ctx, dek)
	if err != nil {
		return nil, fmt.Errorf("drops/clickhouse: EnvelopeCipher.Encrypt: KMS wrap: %w", err)
	}
	block, err := aes.NewCipher(dek)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}
	ct := gcm.Seal(nil, nonce, plaintext, nil)

	// Pack: [magic | wrappedLen(4) | wrappedDEK | nonce(N) | ct]
	out := make([]byte, 1+4+len(wrappedDEK)+len(nonce)+len(ct))
	out[0] = cipherMagic
	binary.BigEndian.PutUint32(out[1:5], uint32(len(wrappedDEK)))
	copy(out[5:], wrappedDEK)
	copy(out[5+len(wrappedDEK):], nonce)
	copy(out[5+len(wrappedDEK)+len(nonce):], ct)
	return out, nil
}

// Decrypt undoes Encrypt — unwraps the DEK via KMS, then opens the
// ciphertext with AES-GCM. Returns an error when the input has the
// wrong magic byte, truncated header, or fails the AEAD check.
func (c *EnvelopeCipher) Decrypt(ctx context.Context, blob []byte) ([]byte, error) {
	if len(blob) < 1+4 {
		return nil, errors.New("drops/clickhouse: EnvelopeCipher.Decrypt: short header")
	}
	if blob[0] != cipherMagic {
		return nil, fmt.Errorf("drops/clickhouse: EnvelopeCipher.Decrypt: bad magic 0x%x", blob[0])
	}
	wrappedLen := binary.BigEndian.Uint32(blob[1:5])
	if int(wrappedLen)+5 > len(blob) {
		return nil, errors.New("drops/clickhouse: EnvelopeCipher.Decrypt: truncated wrappedDEK")
	}
	wrappedDEK := blob[5 : 5+wrappedLen]
	rest := blob[5+wrappedLen:]
	dek, err := c.kms.Unwrap(ctx, wrappedDEK)
	if err != nil {
		return nil, fmt.Errorf("drops/clickhouse: EnvelopeCipher.Decrypt: KMS unwrap: %w", err)
	}
	block, err := aes.NewCipher(dek)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonceSize := gcm.NonceSize()
	if len(rest) < nonceSize+gcm.Overhead() {
		return nil, errors.New("drops/clickhouse: EnvelopeCipher.Decrypt: truncated payload")
	}
	nonce, ctag := rest[:nonceSize], rest[nonceSize:]
	return gcm.Open(nil, nonce, ctag, nil)
}

// ----------------------------------------------------------------------
// Local KMS (testing / development)
// ----------------------------------------------------------------------

// LocalKMS is an in-process KMS backed by a 32-byte master key.
// Useful for tests and local development — DO NOT use in production
// where the master key needs to come from an HSM / KMS service.
type LocalKMS struct {
	masterAEAD cipher.AEAD
}

// NewLocalKMS wraps key (must be 32 bytes for AES-256-GCM) as a
// LocalKMS. The same key must be supplied on every process restart
// — losing it loses every encrypted row.
func NewLocalKMS(key []byte) (*LocalKMS, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("drops/clickhouse: NewLocalKMS: master key must be 32 bytes, got %d", len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &LocalKMS{masterAEAD: gcm}, nil
}

// Wrap encrypts dek under the master key. The output prefixes the
// nonce so Unwrap can recover it without external metadata.
func (k *LocalKMS) Wrap(_ context.Context, dek []byte) ([]byte, error) {
	nonce := make([]byte, k.masterAEAD.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}
	ct := k.masterAEAD.Seal(nil, nonce, dek, nil)
	out := make([]byte, len(nonce)+len(ct))
	copy(out, nonce)
	copy(out[len(nonce):], ct)
	return out, nil
}

// Unwrap is the inverse of Wrap.
func (k *LocalKMS) Unwrap(_ context.Context, wrappedDEK []byte) ([]byte, error) {
	n := k.masterAEAD.NonceSize()
	if len(wrappedDEK) < n+k.masterAEAD.Overhead() {
		return nil, errors.New("drops/clickhouse: LocalKMS.Unwrap: truncated wrapped key")
	}
	nonce, ct := wrappedDEK[:n], wrappedDEK[n:]
	return k.masterAEAD.Open(nil, nonce, ct, nil)
}
