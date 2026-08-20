package pgwire

import (
	"crypto/hmac"
	"crypto/md5"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
)

// Authentication request codes, from the protocol's AuthenticationXxx
// message family.
const (
	authOK                = 0
	authCleartextPassword = 3
	authMD5Password       = 5
	authSASL              = 10
	authSASLContinue      = 11
	authSASLFinal         = 12
)

// authenticate answers one Authentication message. SCRAM takes three
// round trips, and they are run here rather than in the startup loop
// so the exchange reads as one exchange.
func (c *Conn) authenticate(cfg *config, r *readBuf) error {
	switch code := r.int32(); code {
	case authOK:
		return nil
	case authCleartextPassword:
		if cfg.Password == "" {
			return fmt.Errorf("the server asked for a password and the connection string carries none — add it to the URL or set PGPASSWORD")
		}
		return c.sendPassword([]byte(cfg.Password))
	case authMD5Password:
		if cfg.Password == "" {
			return fmt.Errorf("the server asked for a password and the connection string carries none — add it to the URL or set PGPASSWORD")
		}
		salt := r.next(4)
		if r.err != nil {
			return r.err
		}
		return c.sendPassword([]byte(md5Password(cfg.User, cfg.Password, salt)))
	case authSASL:
		if cfg.Password == "" {
			return fmt.Errorf("the server asked for a password and the connection string carries none — add it to the URL or set PGPASSWORD")
		}
		return c.scram(cfg, r)
	case authSASLContinue, authSASLFinal:
		return c.fatal(fmt.Errorf("out-of-sequence SASL message from the server"))
	default:
		return fmt.Errorf("the server asked for authentication method %d, which this client does not implement — connect with sslmode and a password method the server also offers (trust, password, md5, scram-sha-256)", code)
	}
}

// sendPassword writes a PasswordMessage.
func (c *Conn) sendPassword(secret []byte) error {
	var w writeBuf
	w.start(msgPasswordMessage)
	w.bytes(secret)
	w.byte(0)
	w.finish()
	return c.send(&w)
}

// md5Password computes libpq's "md5" + md5(md5(password+user)+salt).
func md5Password(user, password string, salt []byte) string {
	inner := md5.Sum([]byte(password + user))
	var outer []byte
	outer = append(outer, []byte(hex.EncodeToString(inner[:]))...)
	outer = append(outer, salt...)
	sum := md5.Sum(outer)
	return "md5" + hex.EncodeToString(sum[:])
}

// scram runs SCRAM-SHA-256 (RFC 5802, RFC 7677) to completion,
// including verifying the server's signature: the mechanism
// authenticates the server to the client as well, and skipping that
// check would leave a password handed to whoever answered the socket.
//
// Channel binding is not offered. SCRAM-SHA-256-PLUS is refused rather
// than downgraded silently when it is the only mechanism on offer.
func (c *Conn) scram(cfg *config, r *readBuf) error {
	var mechanisms []string
	for {
		m := r.str()
		if m == "" || r.err != nil {
			break
		}
		mechanisms = append(mechanisms, m)
	}
	if !contains(mechanisms, "SCRAM-SHA-256") {
		return fmt.Errorf("the server offers SASL mechanisms %v, none of which this client implements", mechanisms)
	}

	nonce, err := scramNonce()
	if err != nil {
		return err
	}
	clientFirstBare := "n=,r=" + nonce
	clientFirst := "n,," + clientFirstBare

	var w writeBuf
	w.start(msgPasswordMessage)
	w.str("SCRAM-SHA-256")
	w.int32(int32(len(clientFirst)))
	w.bytes([]byte(clientFirst))
	w.finish()
	if err := c.send(&w); err != nil {
		return err
	}

	typ, body, err := c.recv()
	if err != nil {
		return err
	}
	if typ == msgErrorResponse {
		return parseErrorResponse(body)
	}
	if typ != msgAuthentication || body.int32() != authSASLContinue {
		return c.fatal(fmt.Errorf("expected a SASL continue message, got %q", typ))
	}
	serverFirst := string(body.rest())

	clientFinal, expectedSig, err := scramClientFinal(cfg.Password, clientFirstBare, serverFirst, nonce)
	if err != nil {
		return err
	}
	w = writeBuf{}
	w.start(msgPasswordMessage)
	w.bytes([]byte(clientFinal))
	w.finish()
	if err := c.send(&w); err != nil {
		return err
	}

	typ, body, err = c.recv()
	if err != nil {
		return err
	}
	if typ == msgErrorResponse {
		return parseErrorResponse(body)
	}
	if typ != msgAuthentication || body.int32() != authSASLFinal {
		return c.fatal(fmt.Errorf("expected a SASL final message, got %q", typ))
	}
	serverFinal := string(body.rest())
	sig, ok := strings.CutPrefix(serverFinal, "v=")
	if !ok {
		return fmt.Errorf("the server's SASL final message carries no signature: %q", serverFinal)
	}
	sig = strings.SplitN(sig, ",", 2)[0]
	if subtle.ConstantTimeCompare([]byte(sig), []byte(expectedSig)) != 1 {
		return fmt.Errorf("the server's SCRAM signature does not verify — the connection is not talking to a server that knows this password")
	}
	return nil
}

// scramClientFinal builds the client-final message and returns it
// alongside the server signature the server is expected to answer
// with, both base64-encoded per the mechanism.
func scramClientFinal(password, clientFirstBare, serverFirst, clientNonce string) (final, serverSig string, err error) {
	var combinedNonce, saltB64 string
	iterations := 0
	for _, part := range strings.Split(serverFirst, ",") {
		key, value, ok := strings.Cut(part, "=")
		if !ok {
			continue
		}
		switch key {
		case "r":
			combinedNonce = value
		case "s":
			saltB64 = value
		case "i":
			iterations, err = strconv.Atoi(value)
			if err != nil {
				return "", "", fmt.Errorf("SCRAM iteration count %q is not a number", value)
			}
		}
	}
	if !strings.HasPrefix(combinedNonce, clientNonce) {
		return "", "", fmt.Errorf("the server's SCRAM nonce does not extend the client's")
	}
	if iterations <= 0 {
		return "", "", fmt.Errorf("the server asked for %d SCRAM iterations", iterations)
	}
	salt, err := base64.StdEncoding.DecodeString(saltB64)
	if err != nil {
		return "", "", fmt.Errorf("decode SCRAM salt: %w", err)
	}

	saltedPassword := pbkdf2SHA256([]byte(password), salt, iterations, sha256.Size)
	clientKey := hmacSHA256(saltedPassword, []byte("Client Key"))
	storedKey := sha256.Sum256(clientKey)
	withoutProof := "c=biws,r=" + combinedNonce
	authMessage := clientFirstBare + "," + serverFirst + "," + withoutProof
	clientSig := hmacSHA256(storedKey[:], []byte(authMessage))
	proof := make([]byte, len(clientKey))
	for i := range clientKey {
		proof[i] = clientKey[i] ^ clientSig[i]
	}
	serverKey := hmacSHA256(saltedPassword, []byte("Server Key"))
	serverSig = base64.StdEncoding.EncodeToString(hmacSHA256(serverKey, []byte(authMessage)))
	return withoutProof + ",p=" + base64.StdEncoding.EncodeToString(proof), serverSig, nil
}

func hmacSHA256(key, msg []byte) []byte {
	m := hmac.New(sha256.New, key)
	m.Write(msg)
	return m.Sum(nil)
}

// pbkdf2SHA256 is PBKDF2 (RFC 8018) with HMAC-SHA-256. It is a dozen
// lines and golang.org/x/crypto is a dependency this module does not
// have, so it lives here.
func pbkdf2SHA256(password, salt []byte, iterations, keyLen int) []byte {
	hashLen := sha256.Size
	blocks := (keyLen + hashLen - 1) / hashLen
	out := make([]byte, 0, blocks*hashLen)
	buf := make([]byte, 4)
	for block := 1; block <= blocks; block++ {
		binary.BigEndian.PutUint32(buf, uint32(block))
		u := hmacSHA256(password, append(append([]byte(nil), salt...), buf...))
		t := append([]byte(nil), u...)
		for i := 1; i < iterations; i++ {
			u = hmacSHA256(password, u)
			for j := range t {
				t[j] ^= u[j]
			}
		}
		out = append(out, t...)
	}
	return out[:keyLen]
}

// scramNonce returns a fresh client nonce as base64, which is within
// the printable range the mechanism requires and excludes ','.
func scramNonce() (string, error) {
	var raw [18]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("generate SCRAM nonce: %w", err)
	}
	return base64.StdEncoding.EncodeToString(raw[:]), nil
}

func contains(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}
