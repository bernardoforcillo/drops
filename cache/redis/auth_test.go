package redis_test

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"math/big"
	"net"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bernardoforcillo/drops/cache/redis"
)

// --- CredentialsProvider ------------------------------------------

func TestCredentialsProviderCalledOncePerConnection(t *testing.T) {
	fr := newFakeRedis(t)
	fr.authPass = "rotating-token"

	var calls int32
	provider := func(_ context.Context) (redis.Credentials, error) {
		atomic.AddInt32(&calls, 1)
		return redis.Credentials{Username: "tokenuser", Password: "rotating-token"}, nil
	}
	c := redis.New(redis.Options{
		Addr:        fr.addr(),
		Credentials: provider,
		MaxConns:    1,
	})
	defer c.Close()

	ctx := context.Background()
	for i := 0; i < 5; i++ {
		if err := c.Ping(ctx); err != nil {
			t.Fatalf("Ping %d: %v", i, err)
		}
	}
	// MaxConns=1, so the same connection is reused — provider should
	// be called exactly once for the initial dial.
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("provider called %d times, want 1 (per-connection)", got)
	}
}

func TestCredentialsProviderErrorFailsDial(t *testing.T) {
	fr := newFakeRedis(t)
	want := errors.New("token mint blew up")
	c := redis.New(redis.Options{
		Addr: fr.addr(),
		Credentials: func(context.Context) (redis.Credentials, error) {
			return redis.Credentials{}, want
		},
	})
	defer c.Close()

	err := c.Ping(context.Background())
	if err == nil {
		t.Fatal("Ping should have failed when the provider errored")
	}
	if !errors.Is(err, want) {
		t.Errorf("error chain missing original: %v", err)
	}
}

func TestStaticCredentialsHelper(t *testing.T) {
	fr := newFakeRedis(t)
	fr.authPass = "secret"
	c := redis.New(redis.Options{
		Addr:        fr.addr(),
		Credentials: redis.StaticCredentials("", "secret"),
	})
	defer c.Close()
	if err := c.Ping(context.Background()); err != nil {
		t.Errorf("StaticCredentials Ping: %v", err)
	}
}

func TestUsernamePasswordBackCompat(t *testing.T) {
	fr := newFakeRedis(t)
	fr.authPass = "secret"
	c := redis.New(redis.Options{
		Addr:     fr.addr(),
		Username: "tokenuser",
		Password: "secret",
	})
	defer c.Close()
	if err := c.Ping(context.Background()); err != nil {
		t.Errorf("legacy Username/Password Ping: %v", err)
	}
}

func TestCredentialsProviderTakesPrecedenceOverStaticFields(t *testing.T) {
	fr := newFakeRedis(t)
	fr.authPass = "real-token"
	c := redis.New(redis.Options{
		Addr:     fr.addr(),
		Username: "wrong-user",
		Password: "wrong-password", // these would fail
		Credentials: redis.StaticCredentials("right-user", "real-token"),
	})
	defer c.Close()
	if err := c.Ping(context.Background()); err != nil {
		t.Errorf("provider should have overridden static fields; got %v", err)
	}
}

// --- TLS ----------------------------------------------------------

// newTLSFakeRedis spins up a fakeRedis listening on a TLS endpoint
// using a self-signed cert. The cert pool is returned so the client
// can trust it.
func newTLSFakeRedis(t *testing.T) (*fakeRedis, *x509.CertPool) {
	t.Helper()
	cert, pool := selfSignedCert(t)

	l, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
	})
	if err != nil {
		t.Fatalf("tls.Listen: %v", err)
	}
	fr := &fakeRedis{t: t, listener: l, data: map[string]fakeEntry{}}
	go fr.serve()
	t.Cleanup(func() { _ = l.Close() })
	return fr, pool
}

func selfSignedCert(t *testing.T) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "drops-test"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
		DNSNames:              []string{"localhost"},
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert := tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
	parsed, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(parsed)
	return cert, pool
}

func TestTLSConnectionHappyPath(t *testing.T) {
	fr, pool := newTLSFakeRedis(t)
	host, port, _ := net.SplitHostPort(fr.addr())
	_ = port
	c := redis.New(redis.Options{
		Addr: fr.addr(),
		TLS:  &tls.Config{RootCAs: pool, ServerName: host, MinVersion: tls.VersionTLS12},
	})
	defer c.Close()
	if err := c.Ping(context.Background()); err != nil {
		t.Fatalf("Ping over TLS: %v", err)
	}
	if err := c.Set(context.Background(), "k", []byte("v"), 0); err != nil {
		t.Fatalf("Set over TLS: %v", err)
	}
	got, err := c.Get(context.Background(), "k")
	if err != nil || string(got) != "v" {
		t.Errorf("Get over TLS: %q, %v", got, err)
	}
}

func TestTLSFailsAgainstPlaintextServer(t *testing.T) {
	fr := newFakeRedis(t) // plain TCP
	host, _, _ := net.SplitHostPort(fr.addr())
	c := redis.New(redis.Options{
		Addr:        fr.addr(),
		TLS:         &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12},
		DialTimeout: 500 * time.Millisecond,
	})
	defer c.Close()
	if err := c.Ping(context.Background()); err == nil {
		t.Fatal("Ping should fail when TLS is required but server is plaintext")
	}
}

// --- ParseURL -----------------------------------------------------

func TestParseURLShapes(t *testing.T) {
	cases := []struct {
		raw  string
		want redis.Options
	}{
		{
			raw:  "redis://localhost",
			want: redis.Options{Addr: "localhost:6379"},
		},
		{
			raw:  "redis://localhost:6380",
			want: redis.Options{Addr: "localhost:6380"},
		},
		{
			raw:  "redis://user:pw@example.com:6379/3",
			want: redis.Options{Addr: "example.com:6379", Username: "user", Password: "pw", DB: 3},
		},
		{
			raw:  "redis://:pw@example.com:6379",
			want: redis.Options{Addr: "example.com:6379", Password: "pw"},
		},
		{
			raw:  "rediss://example.com:6380/0",
			want: redis.Options{Addr: "example.com:6380"}, // TLS asserted separately
		},
	}
	for _, tc := range cases {
		t.Run(tc.raw, func(t *testing.T) {
			got, err := redis.ParseURL(tc.raw)
			if err != nil {
				t.Fatalf("ParseURL(%q): %v", tc.raw, err)
			}
			gotCmp := got
			gotCmp.TLS = nil // compared below
			if !reflect.DeepEqual(gotCmp, tc.want) {
				t.Errorf("ParseURL(%q)\n  got:  %+v\n  want: %+v", tc.raw, gotCmp, tc.want)
			}
			// rediss must set a TLS config; redis must not.
			isTLS := got.TLS != nil
			wantTLS := tc.raw[:6] == "rediss"
			if isTLS != wantTLS {
				t.Errorf("ParseURL(%q) TLS set = %v, want %v", tc.raw, isTLS, wantTLS)
			}
			if wantTLS {
				if got.TLS.ServerName != "example.com" {
					t.Errorf("TLS.ServerName = %q, want example.com", got.TLS.ServerName)
				}
				if got.TLS.MinVersion < tls.VersionTLS12 {
					t.Errorf("TLS.MinVersion = %d, want >= TLS1.2", got.TLS.MinVersion)
				}
			}
		})
	}
}

func TestParseURLRejectsBadInput(t *testing.T) {
	cases := []string{
		"",
		"http://example.com",
		"redis://",
		"redis://:6379",
		"redis://host/notanint",
	}
	for _, in := range cases {
		t.Run(in, func(t *testing.T) {
			if _, err := redis.ParseURL(in); err == nil {
				t.Errorf("ParseURL(%q) should have errored", in)
			}
		})
	}
}

func TestParseURLOptionsDriveClient(t *testing.T) {
	fr := newFakeRedis(t)
	fr.authPass = "p"
	url := fmt.Sprintf("redis://u:p@%s/0", fr.addr())
	opts, err := redis.ParseURL(url)
	if err != nil {
		t.Fatal(err)
	}
	c := redis.New(opts)
	defer c.Close()
	if err := c.Ping(context.Background()); err != nil {
		t.Errorf("Ping via ParseURL opts: %v", err)
	}
}

// --- guard: a no-creds connect still works against a no-auth server

func TestNoCredentialsAgainstOpenServer(t *testing.T) {
	fr := newFakeRedis(t)
	c := redis.New(redis.Options{Addr: fr.addr()}) // no creds at all
	defer c.Close()
	if err := c.Ping(context.Background()); err != nil {
		t.Errorf("Ping without creds against open server: %v", err)
	}
}

// stub so the import of bufio in this test file is anchored to a real
// reference — used implicitly via the shared fakeRedis test helper.
var _ = bufio.NewReader
