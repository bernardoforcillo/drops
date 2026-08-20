package pgwire

import (
	"encoding/base64"
	"encoding/hex"
	"testing"
)

// The vector is RFC 7677 §3, the one every SCRAM-SHA-256 client is
// checked against. It is here because the exchange is unverifiable in
// integration: the servers this suite talks to authenticate by trust,
// so nothing else would ever execute this path until a user pointed
// the CLI at a server that does not.
func TestSCRAMMatchesRFC7677(t *testing.T) {
	const (
		clientFirstBare = "n=user,r=rOprNGfwEbeRWgbNEkqO"
		serverFirst     = "r=rOprNGfwEbeRWgbNEkqO%hvYDpWUa2RaTCAfuxFIlj)hNlF$k0,s=W22ZaJ0SNY7soEsUEjb6gQ==,i=4096"
		clientNonce     = "rOprNGfwEbeRWgbNEkqO"
		wantFinal       = "c=biws,r=rOprNGfwEbeRWgbNEkqO%hvYDpWUa2RaTCAfuxFIlj)hNlF$k0,p=dHzbZapWIk4jUhN+Ute9ytag9zjfMHgsqmmiz7AndVQ="
		wantServerSig   = "6rriTRBi23WpRR/wtup+mMhUZUn/dB5nLTJRsjl95G4="
	)
	final, serverSig, err := scramClientFinal("pencil", clientFirstBare, serverFirst, clientNonce)
	if err != nil {
		t.Fatalf("scramClientFinal: %v", err)
	}
	if final != wantFinal {
		t.Errorf("client-final\n got %s\nwant %s", final, wantFinal)
	}
	if serverSig != wantServerSig {
		t.Errorf("server signature\n got %s\nwant %s", serverSig, wantServerSig)
	}
}

// A server nonce that does not extend the client's is a server that
// did not see the client's message — an active attacker, or a broken
// proxy. Continuing would hand it a proof computed over its nonce.
func TestSCRAMRejectsForeignNonce(t *testing.T) {
	_, _, err := scramClientFinal("pencil", "n=user,r=abc", "r=zzz,s=W22ZaJ0SNY7soEsUEjb6gQ==,i=4096", "abc")
	if err == nil {
		t.Fatal("accepted a server nonce that does not extend the client's")
	}
}

func TestPBKDF2SHA256(t *testing.T) {
	salt, err := base64.StdEncoding.DecodeString("W22ZaJ0SNY7soEsUEjb6gQ==")
	if err != nil {
		t.Fatal(err)
	}
	// Computed independently with Python's hashlib.pbkdf2_hmac.
	const want = "c4a49510323ab4f952cac1fa99441939e78ea74d6be81ddf7096e87513dc615d"
	if got := hex.EncodeToString(pbkdf2SHA256([]byte("pencil"), salt, 4096, 32)); got != want {
		t.Errorf("pbkdf2\n got %s\nwant %s", got, want)
	}
}

func TestMD5Password(t *testing.T) {
	// Computed independently: md5(md5("hunter2"+"drops") + salt).
	const want = "md53258827c49d05f23bee1ec41a75a2d21"
	got := md5Password("drops", "hunter2", []byte{1, 2, 3, 4})
	if got != want {
		t.Errorf("md5Password = %s, want %s", got, want)
	}
	if same := md5Password("drops", "hunter2", []byte{4, 3, 2, 1}); same == got {
		t.Error("the salt made no difference to the hash")
	}
}
