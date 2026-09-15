package r2_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bernardoforcillo/drops/cloudflare"
	"github.com/bernardoforcillo/drops/cloudflare/r2"
)

type call struct {
	Method  string
	Path    string
	RawPath string
	Query   string
	Header  http.Header
	Body    []byte
}

type server struct {
	*httptest.Server

	t       *testing.T
	handler func(w http.ResponseWriter, r *http.Request, body []byte)
	calls   []call
}

func newServer(t *testing.T) *server {
	t.Helper()
	s := &server{t: t}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		s.calls = append(s.calls, call{
			Method:  r.Method,
			Path:    r.URL.Path,
			RawPath: r.URL.EscapedPath(),
			Query:   r.URL.RawQuery,
			Header:  r.Header.Clone(),
			Body:    raw,
		})
		s.handler(w, r, raw)
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *server) client(opts ...r2.Option) *r2.Client {
	s.t.Helper()
	cf, err := cloudflare.New("acct",
		cloudflare.WithAPIToken("tok"),
		cloudflare.WithBaseURL(s.URL),
		cloudflare.WithRetryPolicy(cloudflare.RetryPolicy{}),
	)
	if err != nil {
		s.t.Fatalf("cloudflare.New: %v", err)
	}
	return r2.New(cf, opts...)
}

func (s *server) last() call {
	s.t.Helper()
	if len(s.calls) == 0 {
		s.t.Fatal("no request was made")
	}
	return s.calls[len(s.calls)-1]
}

func envelope(result string) string {
	return `{"success":true,"errors":[],"messages":[],"result":` + result + `}`
}

func TestPutSendsTheBytesAndTheRecordedType(t *testing.T) {
	srv := newServer(t)
	srv.handler = func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		fmt.Fprint(w, envelope(`{"key":"notes.txt","size":5,"etag":"\"abc\"","storage_class":"Standard"}`))
	}

	obj, err := srv.client().Bucket("docs").
		PutString(context.Background(), "notes.txt", "hello", r2.PutOptions{
			ContentType:  "text/plain",
			StorageClass: r2.InfrequentAccess,
		})
	if err != nil {
		t.Fatalf("PutString: %v", err)
	}

	c := srv.last()
	if c.Method != http.MethodPut {
		t.Errorf("method = %s, want PUT", c.Method)
	}
	if want := "/accounts/acct/r2/buckets/docs/objects/notes.txt"; c.Path != want {
		t.Errorf("path = %s, want %s", c.Path, want)
	}
	if string(c.Body) != "hello" {
		t.Errorf("body = %q", c.Body)
	}
	if got := c.Header.Get("Content-Type"); got != "text/plain" {
		t.Errorf("Content-Type = %q, want text/plain", got)
	}
	if got := c.Header.Get("cf-r2-storage-class"); got != "InfrequentAccess" {
		t.Errorf("cf-r2-storage-class = %q", got)
	}
	// R2 quotes the etag in one place and not the other; a caller
	// comparing a listing against an upload must not have to know
	// which.
	if obj.ETag != "abc" {
		t.Errorf("ETag = %q, want it unquoted", obj.ETag)
	}
}

// A key is a path: its slashes separate segments, and everything else
// in it is escaped so a key built from user input cannot climb out of
// the prefix it was put under.
func TestKeysAreEscapedButSlashesSurvive(t *testing.T) {
	cases := []struct {
		key  string
		want string
	}{
		{"d1/2026-09-15/tenants.sql", "/accounts/acct/r2/buckets/b/objects/d1/2026-09-15/tenants.sql"},
		{"a b/c+d", "/accounts/acct/r2/buckets/b/objects/a%20b/c+d"},
		{"../../etc/passwd", "/accounts/acct/r2/buckets/b/objects/%2E%2E/%2E%2E/etc/passwd"},
		{"a/./b", "/accounts/acct/r2/buckets/b/objects/a/%2E/b"},
		{"/leading//double/", "/accounts/acct/r2/buckets/b/objects/leading/double"},
	}
	for _, tc := range cases {
		t.Run(tc.key, func(t *testing.T) {
			srv := newServer(t)
			srv.handler = func(w http.ResponseWriter, _ *http.Request, _ []byte) {
				fmt.Fprint(w, envelope(`{}`))
			}
			if _, err := srv.client().Bucket("b").PutString(context.Background(), tc.key, "x", r2.PutOptions{}); err != nil {
				t.Fatalf("Put: %v", err)
			}
			if got := srv.last().RawPath; got != tc.want {
				t.Errorf("path = %s, want %s", got, tc.want)
			}
		})
	}
}

func TestPutFileStreamsWithALength(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dump.sql")
	content := strings.Repeat("INSERT INTO t VALUES (1);\n", 100)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	srv := newServer(t)
	var length int64 = -1
	srv.handler = func(w http.ResponseWriter, r *http.Request, _ []byte) {
		length = r.ContentLength
		fmt.Fprint(w, envelope(`{"key":"dump.sql"}`))
	}

	if _, err := srv.client().Bucket("backups").
		PutFile(context.Background(), "d1/dump.sql", path, r2.PutOptions{}); err != nil {
		t.Fatalf("PutFile: %v", err)
	}
	if string(srv.last().Body) != content {
		t.Error("the file's contents did not arrive intact")
	}
	if length != int64(len(content)) {
		t.Errorf("Content-Length = %d, want %d — a chunked upload is refused by some endpoints", length, len(content))
	}
}

// The endpoint has no multipart upload, so an object over the ceiling
// cannot be split into one that fits. Refusing before sending is the
// difference between an error and 300 MB of wasted egress.
func TestPutRefusesAnObjectOverTheCeiling(t *testing.T) {
	path := filepath.Join(t.TempDir(), "big")
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := f.Truncate(r2.MaxObjectSize + 1); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	f.Close()

	srv := newServer(t)
	srv.handler = func(w http.ResponseWriter, _ *http.Request, _ []byte) { fmt.Fprint(w, envelope(`{}`)) }

	_, err = srv.client().Bucket("b").PutFile(context.Background(), "big", path, r2.PutOptions{})
	if !errors.Is(err, r2.ErrObjectTooLarge) {
		t.Fatalf("err = %v, want ErrObjectTooLarge", err)
	}
	if !strings.Contains(err.Error(), "S3-compatible") {
		t.Errorf("error %q does not name the way through", err)
	}
	if len(srv.calls) != 0 {
		t.Errorf("%d requests were made; the check should happen first", len(srv.calls))
	}
}

func TestGetReadsTheBodyAndTheHeaders(t *testing.T) {
	srv := newServer(t)
	srv.handler = func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		w.Header().Set("Content-Type", "application/sql")
		w.Header().Set("ETag", `"deadbeef"`)
		w.Header().Set("Last-Modified", "Tue, 15 Sep 2026 04:05:06 GMT")
		fmt.Fprint(w, "CREATE TABLE t (a);")
	}

	obj, err := srv.client().Bucket("backups").Get(context.Background(), "d1/dump.sql")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer obj.Body.Close()

	if got := srv.last().Header.Get("Accept"); got != "application/octet-stream" {
		t.Errorf("Accept = %q, want application/octet-stream", got)
	}
	body, _ := io.ReadAll(obj.Body)
	if string(body) != "CREATE TABLE t (a);" {
		t.Errorf("body = %q", body)
	}
	if obj.ETag != "deadbeef" {
		t.Errorf("ETag = %q, want it unquoted", obj.ETag)
	}
	if obj.ContentType != "application/sql" {
		t.Errorf("ContentType = %q", obj.ContentType)
	}
	if obj.Size != int64(len(body)) {
		t.Errorf("Size = %d, want %d", obj.Size, len(body))
	}
	if obj.LastModified.IsZero() {
		t.Error("LastModified was not parsed")
	}
}

// A raw-body endpoint's failure has to arrive as the same error type
// the envelope endpoints produce, or a caller would have to know
// which kind of endpoint it asked before it could branch.
func TestGetMissingObjectIsErrNotFound(t *testing.T) {
	srv := newServer(t)
	srv.handler = func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"success":false,"errors":[{"code":10006,"message":"object not found"}]}`)
	}
	_, err := srv.client().Bucket("b").Get(context.Background(), "gone")
	if !errors.Is(err, cloudflare.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	var apiErr *cloudflare.APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("err is %T, want *cloudflare.APIError", err)
	}
	if apiErr.FirstMessage() != "object not found" {
		t.Errorf("message = %q", apiErr.FirstMessage())
	}
}

func TestGetTo(t *testing.T) {
	srv := newServer(t)
	srv.handler = func(w http.ResponseWriter, _ *http.Request, _ []byte) { fmt.Fprint(w, "payload") }
	var buf bytes.Buffer
	info, err := srv.client().Bucket("b").GetTo(context.Background(), "k", &buf)
	if err != nil {
		t.Fatalf("GetTo: %v", err)
	}
	if buf.String() != "payload" {
		t.Errorf("wrote %q", buf.String())
	}
	if info.Key != "k" {
		t.Errorf("Key = %q", info.Key)
	}
}

func TestListWalksTheCursor(t *testing.T) {
	srv := newServer(t)
	page := 0
	srv.handler = func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		page++
		if page == 1 {
			fmt.Fprint(w, `{"success":true,"errors":[],"messages":[],
				"result":[{"key":"a","size":1,"etag":"e1"},{"key":"b","size":2,"etag":"e2"}],
				"result_info":{"cursor":"next","per_page":2,"count":2}}`)
			return
		}
		fmt.Fprint(w, `{"success":true,"errors":[],"messages":[],
			"result":[{"key":"c","size":3,"etag":"e3"}],"result_info":{"per_page":2,"count":1}}`)
	}

	objs, err := srv.client().Bucket("b").List(context.Background(), r2.ListOptions{Prefix: "d1/", Delimiter: "/"})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(objs) != 3 || objs[2].Key != "c" {
		t.Fatalf("objects = %+v", objs)
	}
	if q := srv.calls[0].Query; !strings.Contains(q, "prefix=d1%2F") || !strings.Contains(q, "delimiter=%2F") {
		t.Errorf("first query = %q", q)
	}
	if q := srv.calls[1].Query; !strings.Contains(q, "cursor=next") {
		t.Errorf("second query = %q, want it to carry the cursor", q)
	}
}

// Asking whether an object is there must not fetch it: on a 300 MB
// backup that is an expensive way to say yes.
func TestExistsListsRatherThanFetches(t *testing.T) {
	srv := newServer(t)
	srv.handler = func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		fmt.Fprint(w, envelope(`[{"key":"d1/dump.sql","size":10}]`))
	}
	b := srv.client().Bucket("backups")

	ok, err := b.Exists(context.Background(), "d1/dump.sql")
	if err != nil {
		t.Fatalf("Exists: %v", err)
	}
	if !ok {
		t.Error("Exists = false for a key the listing returned")
	}
	if c := srv.last(); !strings.HasSuffix(c.Path, "/objects") {
		t.Errorf("Exists went to %s, want the listing endpoint", c.Path)
	}

	// A prefix match is not an exact match.
	ok, err = b.Exists(context.Background(), "d1/dump")
	if err != nil {
		t.Fatalf("Exists: %v", err)
	}
	if ok {
		t.Error("Exists = true for a key that only shares a prefix")
	}
}

func TestBucketLifecycle(t *testing.T) {
	srv := newServer(t)
	srv.handler = func(w http.ResponseWriter, r *http.Request, _ []byte) {
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/buckets") {
			fmt.Fprint(w, `{"success":true,"errors":[],"messages":[],
				"result":{"buckets":[{"name":"backups","location":"weur","storage_class":"Standard",
				"creation_date":"2026-09-15T04:05:06.000Z"}]}}`)
			return
		}
		fmt.Fprint(w, envelope(`{"name":"backups","location":"weur","storage_class":"Standard"}`))
	}
	c := srv.client()
	ctx := context.Background()

	made, err := c.CreateBucket(ctx, "backups", r2.CreateBucketOptions{
		Location: r2.LocationWesternEurope, StorageClass: r2.Standard,
	})
	if err != nil {
		t.Fatalf("CreateBucket: %v", err)
	}
	if made.Location != r2.LocationWesternEurope {
		t.Errorf("location = %q", made.Location)
	}
	if body := string(srv.last().Body); !strings.Contains(body, `"locationHint":"weur"`) {
		t.Errorf("create body = %s", body)
	}

	buckets, err := c.ListBuckets(ctx, "")
	if err != nil {
		t.Fatalf("ListBuckets: %v", err)
	}
	if len(buckets) != 1 || buckets[0].Name != "backups" {
		t.Fatalf("buckets = %+v", buckets)
	}
	if _, ok := buckets[0].Created(); !ok {
		t.Errorf("Created() could not parse %q", buckets[0].CreationDate)
	}

	if err := c.DeleteBucket(ctx, "backups"); err != nil {
		t.Fatalf("DeleteBucket: %v", err)
	}
	if srv.last().Method != http.MethodDelete {
		t.Errorf("method = %s", srv.last().Method)
	}
}

// A bucket created under a jurisdiction is invisible without one, so
// the header has to be on every request rather than on the ones the
// caller remembered.
func TestJurisdictionTravelsOnEveryRequest(t *testing.T) {
	srv := newServer(t)
	srv.handler = func(w http.ResponseWriter, _ *http.Request, _ []byte) { fmt.Fprint(w, envelope(`{}`)) }
	c := srv.client(r2.WithJurisdiction(r2.JurisdictionEU))
	ctx := context.Background()

	if _, err := c.CreateBucket(ctx, "b", r2.CreateBucketOptions{}); err != nil {
		t.Fatalf("CreateBucket: %v", err)
	}
	if got := srv.last().Header.Get("cf-r2-jurisdiction"); got != "eu" {
		t.Errorf("create jurisdiction = %q", got)
	}
	if _, err := c.Bucket("b").PutString(ctx, "k", "v", r2.PutOptions{StorageClass: r2.Standard}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	h := srv.last().Header
	if h.Get("cf-r2-jurisdiction") != "eu" {
		t.Errorf("put jurisdiction = %q", h.Get("cf-r2-jurisdiction"))
	}
	// Adding a storage class must not drop the jurisdiction the
	// client carries.
	if h.Get("cf-r2-storage-class") != "Standard" {
		t.Errorf("put storage class = %q", h.Get("cf-r2-storage-class"))
	}
}

func TestEmptyNamesAndKeysAreRefusedLocally(t *testing.T) {
	srv := newServer(t)
	srv.handler = func(w http.ResponseWriter, _ *http.Request, _ []byte) { fmt.Fprint(w, envelope(`{}`)) }
	c := srv.client()
	ctx := context.Background()

	if _, err := c.CreateBucket(ctx, "", r2.CreateBucketOptions{}); !errors.Is(err, r2.ErrNoBucketName) {
		t.Errorf("CreateBucket err = %v", err)
	}
	if _, err := c.Bucket("b").PutString(ctx, "  ", "v", r2.PutOptions{}); !errors.Is(err, r2.ErrNoKey) {
		t.Errorf("Put err = %v", err)
	}
	if err := c.Bucket("b").Delete(ctx, ""); !errors.Is(err, r2.ErrNoKey) {
		t.Errorf("Delete err = %v", err)
	}
	if _, err := c.Bucket("b").PutString(ctx, "k", "v", r2.PutOptions{StorageClass: "Glacier"}); !errors.Is(err, r2.ErrUnknownStorageClass) {
		t.Errorf("Put err = %v", err)
	}
	if len(srv.calls) != 0 {
		t.Errorf("%d requests reached the server", len(srv.calls))
	}
}
