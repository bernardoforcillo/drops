package cloudflarekv_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/bernardoforcillo/drops/cache"
	"github.com/bernardoforcillo/drops/cache/cloudflarekv"
	"github.com/bernardoforcillo/drops/cloudflare"
)

type kvServer struct {
	t      *testing.T
	srv    *httptest.Server
	handle func(w http.ResponseWriter, r *http.Request)
	paths  []string
	querys []string
}

func newServer(t *testing.T, handle func(http.ResponseWriter, *http.Request)) *kvServer {
	t.Helper()
	s := &kvServer{t: t, handle: handle}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.paths = append(s.paths, r.Method+" "+r.URL.Path)
		s.querys = append(s.querys, r.URL.RawQuery)
		s.handle(w, r)
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *kvServer) cache(opts ...cloudflarekv.Option) *cloudflarekv.Cache {
	s.t.Helper()
	cf, err := cloudflare.New("acct",
		cloudflare.WithAPIToken("tok"),
		cloudflare.WithBaseURL(s.srv.URL),
		cloudflare.WithRetryPolicy(cloudflare.RetryPolicy{}))
	if err != nil {
		s.t.Fatalf("cloudflare.New: %v", err)
	}
	c, err := cloudflarekv.New(cf, "ns-1", opts...)
	if err != nil {
		s.t.Fatalf("cloudflarekv.New: %v", err)
	}
	return c
}

const okEnvelope = `{"success":true,"errors":[],"messages":[],"result":null}`

func TestNewRequiresANamespace(t *testing.T) {
	cf, _ := cloudflare.New("acct", cloudflare.WithAPIToken("t"))
	if _, err := cloudflarekv.New(cf, "  "); !errors.Is(err, cloudflarekv.ErrNoNamespace) {
		t.Errorf("err = %v, want ErrNoNamespace", err)
	}
}

func TestGetReturnsStoredBytes(t *testing.T) {
	s := newServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("cached"))
	})
	got, err := s.cache().Get(context.Background(), "k")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(got) != "cached" {
		t.Errorf("Get = %q", got)
	}
	if want := "GET /accounts/acct/storage/kv/namespaces/ns-1/values/k"; s.paths[0] != want {
		t.Errorf("path = %q, want %q", s.paths[0], want)
	}
}

// ErrNotFound is the only way to tell a missing key from one holding
// an empty value, and KV stores an empty value perfectly well.
func TestMissingKeyIsErrNotFound(t *testing.T) {
	s := newServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"success":false,"errors":[{"code":10009,"message":"get: key not found"}]}`)
	})
	_, err := s.cache().Get(context.Background(), "gone")
	if !errors.Is(err, cache.ErrNotFound) {
		t.Errorf("err = %v, want cache.ErrNotFound", err)
	}
}

func TestEmptyValueIsNotAMiss(t *testing.T) {
	s := newServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	got, err := s.cache().Get(context.Background(), "empty")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got == nil || len(got) != 0 {
		t.Errorf("Get = %#v, want an empty non-nil slice", got)
	}
}

// Set has to carry both the value and the expiry marker, and the
// marker is what makes TTL answerable later.
func TestSetSendsValueAndExpiryMarker(t *testing.T) {
	var value []byte
	var meta cloudflarekv.Metadata
	s := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil {
			t.Fatalf("Content-Type: %v", err)
		}
		mr := multipart.NewReader(r.Body, params["boundary"])
		for {
			part, pErr := mr.NextPart()
			if pErr != nil {
				break
			}
			raw, _ := io.ReadAll(part)
			switch part.FormName() {
			case "value":
				value = raw
			case "metadata":
				_ = json.Unmarshal(raw, &meta)
			}
		}
		fmt.Fprint(w, okEnvelope)
	})

	before := time.Now()
	if err := s.cache().Set(context.Background(), "k", []byte("payload"), time.Hour); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if string(value) != "payload" {
		t.Errorf("value = %q", value)
	}
	if meta.ExpiresAt == 0 {
		t.Fatal("no expiry marker was written; TTL would have nothing to answer with")
	}
	got := time.Unix(meta.ExpiresAt, 0)
	if got.Before(before.Add(59*time.Minute)) || got.After(before.Add(61*time.Minute)) {
		t.Errorf("expiry = %v, want about an hour out", got)
	}
	if !strings.Contains(s.querys[0], "expiration_ttl=3600") {
		t.Errorf("query = %q, want expiration_ttl=3600", s.querys[0])
	}
}

// A TTL below KV's floor is refused rather than rounded: an entry
// given sixty seconds when it asked for five is served stale for
// fifty-five, and that bug surfaces far from here.
func TestShortTTLIsRefusedByDefault(t *testing.T) {
	s := newServer(t, func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, okEnvelope)
	})
	err := s.cache().Set(context.Background(), "k", []byte("v"), 5*time.Second)
	if !errors.Is(err, cloudflarekv.ErrTTLTooShort) {
		t.Fatalf("err = %v, want ErrTTLTooShort", err)
	}
	if len(s.paths) != 0 {
		t.Errorf("%d request(s), want 0", len(s.paths))
	}
}

func TestShortTTLRoundsUpWhenAsked(t *testing.T) {
	s := newServer(t, func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, okEnvelope)
	})
	if err := s.cache(cloudflarekv.WithRoundUp()).Set(context.Background(), "k", []byte("v"), 5*time.Second); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if !strings.Contains(s.querys[0], "expiration_ttl=60") {
		t.Errorf("query = %q, want the TTL rounded up to 60", s.querys[0])
	}
}

func TestZeroTTLMeansNoExpiry(t *testing.T) {
	s := newServer(t, func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, okEnvelope)
	})
	if err := s.cache().Set(context.Background(), "k", []byte("v"), 0); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if strings.Contains(s.querys[0], "expiration") {
		t.Errorf("query = %q, want no expiration for ttl=0", s.querys[0])
	}
}

func TestTTLReadsTheMarker(t *testing.T) {
	expires := time.Now().Add(30 * time.Minute).Unix()
	s := newServer(t, func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, `{"success":true,"errors":[],"messages":[],"result":{"drops_exp":%d}}`, expires)
	})
	got, err := s.cache().TTL(context.Background(), "k")
	if err != nil {
		t.Fatalf("TTL: %v", err)
	}
	if got < 29*time.Minute || got > 31*time.Minute {
		t.Errorf("TTL = %v, want about 30m", got)
	}
}

// A key written by something other than this package carries no
// marker, and -1 ("no expiry") is the honest answer rather than a
// guess.
func TestTTLOfAForeignKeyIsNoExpiry(t *testing.T) {
	s := newServer(t, func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"success":true,"errors":[],"messages":[],"result":{"written_by":"some other tool"}}`)
	})
	got, err := s.cache().TTL(context.Background(), "k")
	if err != nil {
		t.Fatalf("TTL: %v", err)
	}
	if got != -1 {
		t.Errorf("TTL = %v, want -1", got)
	}
}

func TestTTLOfAMissingKeyIsNotFound(t *testing.T) {
	s := newServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"success":false,"errors":[]}`)
	})
	d, err := s.cache().TTL(context.Background(), "k")
	if !errors.Is(err, cache.ErrNotFound) {
		t.Errorf("err = %v, want cache.ErrNotFound", err)
	}
	if d != 0 {
		t.Errorf("TTL = %v, want 0 alongside ErrNotFound", d)
	}
}

func TestExists(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		want   bool
	}{
		{"present", http.StatusOK, true},
		{"absent", http.StatusNotFound, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newServer(t, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				fmt.Fprint(w, `{"success":true,"errors":[],"messages":[],"result":{}}`)
			})
			got, err := s.cache().Exists(context.Background(), "k")
			if err != nil {
				t.Fatalf("Exists: %v", err)
			}
			if got != tc.want {
				t.Errorf("Exists = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestDeleteUsesTheBulkEndpoint(t *testing.T) {
	var sent []string
	s := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &sent)
		fmt.Fprint(w, okEnvelope)
	})
	n, err := s.cache().Delete(context.Background(), "a", "b")
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if n != 2 {
		t.Errorf("Delete = %d, want 2", n)
	}
	if len(sent) != 2 || sent[0] != "a" {
		t.Errorf("body = %v", sent)
	}
}

func TestKeyPrefixIsApplied(t *testing.T) {
	s := newServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("v"))
	})
	c := s.cache(cloudflarekv.WithKeyPrefix("app:"))
	if _, err := c.Get(context.Background(), "k"); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !strings.HasSuffix(s.paths[0], "/values/app:k") {
		t.Errorf("path = %q, want the prefix applied", s.paths[0])
	}
}

// A bulk read answers under the prefixed key; the caller asked with
// the bare one and must get it back.
func TestKeyPrefixIsStrippedFromBulkResults(t *testing.T) {
	s := newServer(t, func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"success":true,"errors":[],"messages":[],"result":{"values":{"app:k":"v"}}}`)
	})
	got, err := s.cache(cloudflarekv.WithKeyPrefix("app:")).GetMulti(context.Background(), "k")
	if err != nil {
		t.Fatalf("GetMulti: %v", err)
	}
	if string(got["k"]) != "v" {
		t.Errorf("got = %v, want the result keyed by the bare key", got)
	}
}

func TestOversizeKeyIsRefusedLocally(t *testing.T) {
	s := newServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("v"))
	})
	long := strings.Repeat("k", cloudflarekv.MaxKeyBytes+1)
	if _, err := s.cache().Get(context.Background(), long); !errors.Is(err, cloudflarekv.ErrKeyTooLong) {
		t.Errorf("err = %v, want ErrKeyTooLong", err)
	}
	if len(s.paths) != 0 {
		t.Errorf("%d request(s), want 0", len(s.paths))
	}
}

func TestEmptyKeyIsInvalid(t *testing.T) {
	s := newServer(t, func(http.ResponseWriter, *http.Request) {})
	if _, err := s.cache().Get(context.Background(), ""); !errors.Is(err, cache.ErrInvalidKey) {
		t.Errorf("err = %v, want cache.ErrInvalidKey", err)
	}
}

func TestClosedCacheRefusesEverything(t *testing.T) {
	s := newServer(t, func(http.ResponseWriter, *http.Request) {})
	c := s.cache()
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Errorf("Close is not idempotent: %v", err)
	}
	if _, err := c.Get(context.Background(), "k"); !errors.Is(err, cache.ErrClosed) {
		t.Errorf("Get after Close = %v, want cache.ErrClosed", err)
	}
	if err := c.Set(context.Background(), "k", nil, 0); !errors.Is(err, cache.ErrClosed) {
		t.Errorf("Set after Close = %v, want cache.ErrClosed", err)
	}
	if err := c.Ping(context.Background()); !errors.Is(err, cache.ErrClosed) {
		t.Errorf("Ping after Close = %v, want cache.ErrClosed", err)
	}
}

// Bulk -------------------------------------------------------------------

func TestGetMultiReadsInOneRequest(t *testing.T) {
	s := newServer(t, func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"success":true,"errors":[],"messages":[],"result":{"values":{"a":"one","b":"two","c":null}}}`)
	})
	got, err := s.cache().GetMulti(context.Background(), "a", "b", "c")
	if err != nil {
		t.Fatalf("GetMulti: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d entries, want 2 — a null value is an absent key", len(got))
	}
	if string(got["a"]) != "one" || string(got["b"]) != "two" {
		t.Errorf("got = %v", got)
	}
	if len(s.paths) != 1 {
		t.Errorf("%d request(s), want 1", len(s.paths))
	}
}

// The bulk endpoint answers in JSON, so a value that is not valid
// UTF-8 comes back with its bytes replaced. Returning that would be
// silent corruption, so those keys are re-read byte-exact.
func TestGetMultiRefetchesValuesTheTextDecodeMangled(t *testing.T) {
	binary := []byte{0xff, 0xfe, 0x00, 0x01}

	// What a JSON encoder does to bytes that are not valid UTF-8:
	// each one becomes the replacement character.
	mangled, err := json.Marshal(map[string]any{
		"values": map[string]any{
			"good": "fine",
			"blob": string([]rune{0xFFFD, 0xFFFD, 0x00, 0x01}),
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	var n int
	s := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		n++
		if strings.HasSuffix(r.URL.Path, "/bulk/get") {
			fmt.Fprintf(w, `{"success":true,"errors":[],"messages":[],"result":%s}`, mangled)
			return
		}
		_, _ = w.Write(binary)
	})

	got, err := s.cache().GetMulti(context.Background(), "good", "blob")
	if err != nil {
		t.Fatalf("GetMulti: %v", err)
	}
	if string(got["good"]) != "fine" {
		t.Errorf("good = %q", got["good"])
	}
	if string(got["blob"]) != string(binary) {
		t.Errorf("blob = %#v, want the stored bytes %#v", got["blob"], binary)
	}
	if n != 2 {
		t.Errorf("%d request(s), want 2 — the bulk read plus one re-read", n)
	}
}

func TestGetMultiOfNothingIsNotARequest(t *testing.T) {
	s := newServer(t, func(http.ResponseWriter, *http.Request) {})
	got, err := s.cache().GetMulti(context.Background())
	if err != nil {
		t.Fatalf("GetMulti: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %v", got)
	}
	if len(s.paths) != 0 {
		t.Errorf("%d request(s), want 0", len(s.paths))
	}
}

func TestSetMultiWritesBase64SoBinarySurvives(t *testing.T) {
	var entries []struct {
		Key           string `json:"key"`
		Value         string `json:"value"`
		Base64        bool   `json:"base64"`
		ExpirationTTL int64  `json:"expiration_ttl"`
	}
	s := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &entries)
		fmt.Fprint(w, okEnvelope)
	})
	err := s.cache().SetMulti(context.Background(), map[string][]byte{
		"a": {0xff, 0x00},
	}, time.Hour)
	if err != nil {
		t.Fatalf("SetMulti: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("%d entries", len(entries))
	}
	if !entries[0].Base64 {
		t.Error("base64 flag is not set; binary values would not survive the JSON body")
	}
	if entries[0].Value != "/wA=" {
		t.Errorf("value = %q, want the base64 of the bytes", entries[0].Value)
	}
	if entries[0].ExpirationTTL != 3600 {
		t.Errorf("expiration_ttl = %d, want 3600", entries[0].ExpirationTTL)
	}
}

func TestSetMultiRefusesAShortTTL(t *testing.T) {
	s := newServer(t, func(http.ResponseWriter, *http.Request) {})
	err := s.cache().SetMulti(context.Background(), map[string][]byte{"a": []byte("v")}, time.Second)
	if !errors.Is(err, cloudflarekv.ErrTTLTooShort) {
		t.Errorf("err = %v, want ErrTTLTooShort", err)
	}
}

func TestUnauthorizedIsClassified(t *testing.T) {
	s := newServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, `{"success":false,"errors":[{"code":10001,"message":"Unauthorized"}]}`)
	})
	_, err := s.cache().Get(context.Background(), "k")
	if !errors.Is(err, cloudflare.ErrUnauthorized) {
		t.Errorf("err = %v, want cloudflare.ErrUnauthorized", err)
	}
}

var (
	_ cache.Cache      = (*cloudflarekv.Cache)(nil)
	_ cache.MultiCache = (*cloudflarekv.Cache)(nil)
)
