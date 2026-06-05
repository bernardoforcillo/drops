package pg_test

import (
	"crypto/rand"
	"errors"
	"testing"

	"github.com/bernardoforcillo/drops/pg"
)

func makeKeyring(t *testing.T) pg.Keyring {
	t.Helper()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	ring, err := pg.AESGCMKeyring(key)
	if err != nil {
		t.Fatal(err)
	}
	return ring
}

func TestSecretRoundTrip(t *testing.T) {
	pg.SetKeyring(makeKeyring(t))
	defer pg.SetKeyring(nil)

	s := pg.NewSecret("alice@example.com")
	enc, err := s.Value()
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	if string(enc.([]byte)) == "alice@example.com" {
		t.Errorf("encrypted bytes must differ from plaintext")
	}

	var dec pg.Secret[string]
	if err := dec.Scan(enc); err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	if dec.Plain != "alice@example.com" {
		t.Errorf("round-trip plaintext: %q", dec.Plain)
	}
}

func TestSecretWorksForVariousTypes(t *testing.T) {
	pg.SetKeyring(makeKeyring(t))
	defer pg.SetKeyring(nil)

	t.Run("int64", func(t *testing.T) {
		s := pg.NewSecret(int64(424242))
		enc, _ := s.Value()
		var got pg.Secret[int64]
		if err := got.Scan(enc); err != nil {
			t.Fatal(err)
		}
		if got.Plain != 424242 {
			t.Errorf("int64: %d", got.Plain)
		}
	})

	t.Run("struct", func(t *testing.T) {
		type card struct {
			Number string
			CVV    string
		}
		s := pg.NewSecret(card{Number: "4242424242424242", CVV: "999"})
		enc, _ := s.Value()
		var got pg.Secret[card]
		if err := got.Scan(enc); err != nil {
			t.Fatal(err)
		}
		if got.Plain.Number != "4242424242424242" || got.Plain.CVV != "999" {
			t.Errorf("struct: %+v", got.Plain)
		}
	})
}

func TestSecretWithoutKeyringErrors(t *testing.T) {
	pg.SetKeyring(nil)
	defer pg.SetKeyring(nil)
	s := pg.NewSecret("x")
	if _, err := s.Value(); !errors.Is(err, pg.ErrNoKeyring) {
		t.Errorf("expected ErrNoKeyring, got %v", err)
	}
	var dst pg.Secret[string]
	if err := dst.Scan([]byte{0, 1, 2}); !errors.Is(err, pg.ErrNoKeyring) {
		t.Errorf("expected ErrNoKeyring on Scan, got %v", err)
	}
}

func TestSecretRejectsTamperedCiphertext(t *testing.T) {
	pg.SetKeyring(makeKeyring(t))
	defer pg.SetKeyring(nil)
	s := pg.NewSecret("secret")
	enc, _ := s.Value()
	bytes := enc.([]byte)
	bytes[len(bytes)-1] ^= 0xff // flip a tag byte
	var got pg.Secret[string]
	if err := got.Scan(bytes); err == nil {
		t.Error("tampered ciphertext must fail to decrypt")
	}
}

func TestSecretAutoTableMapsToBytea(t *testing.T) {
	type vault struct {
		ID    int64             `drop:"id,primaryKey,autoIncrement"`
		Token pg.Secret[string] `drop:"token,notNull"`
	}
	tbl := pg.AutoTable[vault]("vault")
	col := tbl.Col("token")
	if col == nil {
		t.Fatal("token column missing")
	}
	if col.Type().TypeSQL() != "bytea" {
		t.Errorf("Secret[T] column should be bytea, got %q", col.Type().TypeSQL())
	}
	if !col.IsNotNull() {
		t.Error("notNull tag should propagate")
	}
}

func TestCiphertextTooShort(t *testing.T) {
	pg.SetKeyring(makeKeyring(t))
	defer pg.SetKeyring(nil)
	var got pg.Secret[string]
	if err := got.Scan([]byte{1, 2}); !errors.Is(err, pg.ErrCiphertextTooShort) {
		t.Errorf("expected ErrCiphertextTooShort, got %v", err)
	}
}
