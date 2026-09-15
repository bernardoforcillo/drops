package d1_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bernardoforcillo/drops/cloudflare"
	"github.com/bernardoforcillo/drops/cloudflare/d1"
)

// adminServer is an httptest server plus the handler a test installs
// on it, so a case can answer differently per path.
type adminServer struct {
	*httptest.Server

	t       *testing.T
	handler func(w http.ResponseWriter, r *http.Request, body []byte)

	calls []adminCall
}

type adminCall struct {
	Method string
	Path   string
	Query  string
	Body   string
}

func newAdminServer(t *testing.T) *adminServer {
	t.Helper()
	s := &adminServer{t: t}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		s.calls = append(s.calls, adminCall{
			Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery, Body: string(raw),
		})
		s.handler(w, r, raw)
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *adminServer) admin(opts ...d1.AdminOption) *d1.Admin {
	s.t.Helper()
	cf, err := cloudflare.New("acct",
		cloudflare.WithAPIToken("tok"),
		cloudflare.WithBaseURL(s.URL),
		cloudflare.WithRetryPolicy(cloudflare.RetryPolicy{}),
	)
	if err != nil {
		s.t.Fatalf("cloudflare.New: %v", err)
	}
	return d1.NewAdmin(cf, opts...)
}

func (s *adminServer) last() adminCall {
	s.t.Helper()
	if len(s.calls) == 0 {
		s.t.Fatal("no request was made")
	}
	return s.calls[len(s.calls)-1]
}

// envelope wraps a result the way every Cloudflare endpoint does.
func envelope(result string) string {
	return `{"success":true,"errors":[],"messages":[],"result":` + result + `}`
}

func TestAdminCreateSendsTheOptionsAndReadsTheDatabaseBack(t *testing.T) {
	srv := newAdminServer(t)
	srv.handler = func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		fmt.Fprint(w, envelope(`{
			"uuid":"db-uuid","name":"tenant-42","version":"production",
			"created_at":"2026-01-02T03:04:05Z","file_size":4096,"num_tables":3,
			"read_replication":{"mode":"auto"}}`))
	}

	db, err := srv.admin().Create(context.Background(), d1.CreateOptions{
		Name:            "tenant-42",
		PrimaryLocation: d1.LocationWesternEurope,
		ReadReplication: d1.ReplicationAuto,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	call := srv.last()
	if call.Method != http.MethodPost {
		t.Errorf("method = %s, want POST", call.Method)
	}
	if want := "/accounts/acct/d1/database"; call.Path != want {
		t.Errorf("path = %s, want %s", call.Path, want)
	}
	var sent map[string]any
	if err := json.Unmarshal([]byte(call.Body), &sent); err != nil {
		t.Fatalf("body is not JSON: %v", err)
	}
	if sent["name"] != "tenant-42" || sent["primary_location_hint"] != "weur" {
		t.Errorf("body = %s", call.Body)
	}
	rr, _ := sent["read_replication"].(map[string]any)
	if rr == nil || rr["mode"] != "auto" {
		t.Errorf("read_replication = %v", sent["read_replication"])
	}

	if db.UUID != "db-uuid" || db.Name != "tenant-42" || db.NumTables != 3 || db.FileSize != 4096 {
		t.Errorf("database = %+v", db)
	}
	if !db.CreatedAt.Equal(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)) {
		t.Errorf("CreatedAt = %v", db.CreatedAt)
	}
	if !db.Replicated() {
		t.Error("Replicated() = false for a database in auto mode")
	}
}

// A jurisdiction overrides a location hint at D1, so sending both is
// the caller's business — but a value D1 does not define is refused
// here, without a round trip that would fail with a less useful
// message.
func TestAdminCreateRefusesUnknownEnums(t *testing.T) {
	srv := newAdminServer(t)
	srv.handler = func(w http.ResponseWriter, _ *http.Request, _ []byte) { fmt.Fprint(w, envelope(`{}`)) }
	a := srv.admin()
	ctx := context.Background()

	cases := []struct {
		name string
		opts d1.CreateOptions
		want error
	}{
		{"no name", d1.CreateOptions{}, d1.ErrNoDatabaseName},
		{"jurisdiction", d1.CreateOptions{Name: "x", Jurisdiction: "mars"}, d1.ErrUnknownJurisdiction},
		{"location", d1.CreateOptions{Name: "x", PrimaryLocation: "moon"}, d1.ErrUnknownLocationHint},
		{"replication", d1.CreateOptions{Name: "x", ReadReplication: "sometimes"}, d1.ErrUnknownReplicationMode},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := a.Create(ctx, tc.opts); !errors.Is(err, tc.want) {
				t.Errorf("err = %v, want %v", err, tc.want)
			}
			if len(srv.calls) != 0 {
				t.Errorf("%d requests reached the server; the check should be local", len(srv.calls))
			}
		})
	}
}

func TestAdminListWalksEveryPage(t *testing.T) {
	srv := newAdminServer(t)
	page := 0
	srv.handler = func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		page++
		if page == 1 {
			var b strings.Builder
			b.WriteString(`[`)
			for i := 0; i < 100; i++ {
				if i > 0 {
					b.WriteString(",")
				}
				fmt.Fprintf(&b, `{"uuid":"u%d","name":"n%d"}`, i, i)
			}
			b.WriteString(`]`)
			fmt.Fprint(w, envelope(b.String()))
			return
		}
		fmt.Fprint(w, envelope(`[{"uuid":"last","name":"last"}]`))
	}

	dbs, err := srv.admin().List(context.Background(), "")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(dbs) != 101 {
		t.Fatalf("%d databases, want 101", len(dbs))
	}
	if dbs[100].UUID != "last" {
		t.Errorf("last database = %+v", dbs[100])
	}
	if len(srv.calls) != 2 {
		t.Errorf("%d requests, want 2 pages", len(srv.calls))
	}
}

// The name filter is a search at D1, so a page that comes back
// without an exact match is a miss rather than a near-enough hit.
func TestAdminFindByNameNeedsAnExactMatch(t *testing.T) {
	srv := newAdminServer(t)
	srv.handler = func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		fmt.Fprint(w, envelope(`[{"uuid":"u1","name":"tenant-420"}]`))
	}
	_, err := srv.admin().FindByName(context.Background(), "tenant-42")
	if !errors.Is(err, cloudflare.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestAdminSetReadReplication(t *testing.T) {
	srv := newAdminServer(t)
	srv.handler = func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		fmt.Fprint(w, envelope(`{"uuid":"u1","read_replication":{"mode":"disabled"}}`))
	}
	db, err := srv.admin().SetReadReplication(context.Background(), "u1", d1.ReplicationDisabled)
	if err != nil {
		t.Fatalf("SetReadReplication: %v", err)
	}
	call := srv.last()
	if call.Method != http.MethodPatch {
		t.Errorf("method = %s, want PATCH", call.Method)
	}
	if !strings.Contains(call.Body, `"mode":"disabled"`) {
		t.Errorf("body = %s", call.Body)
	}
	if db.Replicated() {
		t.Error("Replicated() = true after disabling replication")
	}
}

func TestTimeTravelBookmarkAndRestore(t *testing.T) {
	srv := newAdminServer(t)
	srv.handler = func(w http.ResponseWriter, r *http.Request, _ []byte) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/time_travel/bookmark"):
			fmt.Fprint(w, envelope(`{"bookmark":"00000085-0000023d-00004c1e-aaaa"}`))
		default:
			fmt.Fprint(w, envelope(`{"bookmark":"new","previous_bookmark":"old","message":"restored"}`))
		}
	}
	a := srv.admin()
	ctx := context.Background()

	got, err := a.Bookmark(ctx, "u1")
	if err != nil {
		t.Fatalf("Bookmark: %v", err)
	}
	if got != "00000085-0000023d-00004c1e-aaaa" {
		t.Errorf("bookmark = %q", got)
	}
	if q := srv.last().Query; q != "" {
		t.Errorf("current bookmark should send no timestamp, got %q", q)
	}

	at := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	if _, err = a.BookmarkAt(ctx, "u1", at); err != nil {
		t.Fatalf("BookmarkAt: %v", err)
	}
	if q := srv.last().Query; !strings.Contains(q, "timestamp=2026-03-04T05%3A06%3A07Z") {
		t.Errorf("query = %q, want the timestamp", q)
	}

	res, err := a.Restore(ctx, "u1", d1.RestoreOptions{Bookmark: "target"})
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if res.PreviousBookmark != "old" {
		t.Errorf("PreviousBookmark = %q, want old — it is the only way back", res.PreviousBookmark)
	}
	if q := srv.last().Query; !strings.Contains(q, "bookmark=target") {
		t.Errorf("query = %q", q)
	}
}

// D1 restores to a bookmark or to a timestamp. Given both, picking
// one would silently ignore half of what the caller asked for, and
// the half ignored decides which data survives.
func TestRestoreRefusesAnAmbiguousPoint(t *testing.T) {
	srv := newAdminServer(t)
	srv.handler = func(w http.ResponseWriter, _ *http.Request, _ []byte) { fmt.Fprint(w, envelope(`{}`)) }
	a := srv.admin()
	ctx := context.Background()

	_, err := a.Restore(ctx, "u1", d1.RestoreOptions{Bookmark: "b", At: time.Now()})
	if !errors.Is(err, d1.ErrAmbiguousRestorePoint) {
		t.Errorf("err = %v, want ErrAmbiguousRestorePoint", err)
	}
	_, err = a.Restore(ctx, "u1", d1.RestoreOptions{})
	if !errors.Is(err, d1.ErrNoRestorePoint) {
		t.Errorf("err = %v, want ErrNoRestorePoint", err)
	}
	if len(srv.calls) != 0 {
		t.Errorf("%d requests reached the server", len(srv.calls))
	}
}

// The export endpoint is a job: the first call starts it, and each
// one after carries the bookmark to ask again against.
func TestExportPollsUntilComplete(t *testing.T) {
	var dump = "CREATE TABLE t (a);\n"
	files := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, dump)
	}))
	defer files.Close()

	srv := newAdminServer(t)
	n := 0
	srv.handler = func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		n++
		if n < 3 {
			fmt.Fprint(w, envelope(`{"status":"active","at_bookmark":"job-1","messages":["working"]}`))
			return
		}
		fmt.Fprint(w, envelope(`{"status":"complete","at_bookmark":"job-1","messages":["done"],
			"result":{"filename":"dump.sql","signed_url":"`+files.URL+`/dump.sql"}}`))
	}

	var out bytes.Buffer
	exp, err := srv.admin(d1.WithPollInterval(time.Millisecond)).
		ExportTo(context.Background(), "u1", &out, d1.ExportOptions{NoData: true, Tables: []string{"t"}})
	if err != nil {
		t.Fatalf("ExportTo: %v", err)
	}
	if out.String() != dump {
		t.Errorf("downloaded %q, want %q", out.String(), dump)
	}
	if exp.Bookmark != "job-1" {
		t.Errorf("Bookmark = %q", exp.Bookmark)
	}
	if len(srv.calls) != 3 {
		t.Fatalf("%d requests, want 3 (start plus two polls)", len(srv.calls))
	}

	var first map[string]any
	if err := json.Unmarshal([]byte(srv.calls[0].Body), &first); err != nil {
		t.Fatalf("first body: %v", err)
	}
	if first["output_format"] != "polling" {
		t.Errorf("first body = %s", srv.calls[0].Body)
	}
	dumpOpts, _ := first["dump_options"].(map[string]any)
	if dumpOpts == nil || dumpOpts["no_data"] != true {
		t.Errorf("dump_options = %v", first["dump_options"])
	}
	var second map[string]any
	if err := json.Unmarshal([]byte(srv.calls[1].Body), &second); err != nil {
		t.Fatalf("second body: %v", err)
	}
	if second["current_bookmark"] != "job-1" {
		t.Errorf("poll body = %s, want it to carry the bookmark", srv.calls[1].Body)
	}
}

func TestExportReportsD1sOwnFailure(t *testing.T) {
	srv := newAdminServer(t)
	srv.handler = func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		fmt.Fprint(w, envelope(`{"status":"error","error":"database is too large to export"}`))
	}
	_, err := srv.admin().Export(context.Background(), "u1", d1.ExportOptions{})
	if !errors.Is(err, d1.ErrExportFailed) {
		t.Fatalf("err = %v, want ErrExportFailed", err)
	}
	if !strings.Contains(err.Error(), "too large") {
		t.Errorf("error %q drops D1's own reason", err)
	}
}

// A job that never finishes is D1's to keep running; what times out
// here is the waiting, and the message has to say so.
func TestExportGivesUpWaiting(t *testing.T) {
	srv := newAdminServer(t)
	srv.handler = func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		fmt.Fprint(w, envelope(`{"status":"active","at_bookmark":"job-1"}`))
	}
	_, err := srv.admin(d1.WithPollInterval(time.Millisecond), d1.WithPollTimeout(5*time.Millisecond)).
		Export(context.Background(), "u1", d1.ExportOptions{})
	if !errors.Is(err, d1.ErrPollTimeout) {
		t.Fatalf("err = %v, want ErrPollTimeout", err)
	}
	if !strings.Contains(err.Error(), "still running at D1") {
		t.Errorf("error %q does not say the job outlives the wait", err)
	}
}

// The import is three steps D1 requires: hash, presigned upload,
// ingest-and-poll.
func TestImportRunsInitUploadIngest(t *testing.T) {
	var uploaded []byte
	var uploadAuth string
	uploads := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		uploaded, _ = io.ReadAll(r.Body)
		uploadAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer uploads.Close()

	srv := newAdminServer(t)
	n := 0
	srv.handler = func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		n++
		switch n {
		case 1:
			fmt.Fprint(w, envelope(`{"upload_url":"`+uploads.URL+`/put","filename":"acct/db.sql"}`))
		case 2:
			fmt.Fprint(w, envelope(`{"status":"active","at_bookmark":"ingest-1"}`))
		default:
			fmt.Fprint(w, envelope(`{"status":"complete","at_bookmark":"ingest-1",
				"result":{"final_bookmark":"after","num_queries":7,"meta":{"rows_written":12}}}`))
		}
	}

	sql := []byte("CREATE TABLE t (a);\nINSERT INTO t VALUES (1);\n")
	res, err := srv.admin(d1.WithPollInterval(time.Millisecond)).
		Import(context.Background(), "u1", sql)
	if err != nil {
		t.Fatalf("Import: %v", err)
	}

	if !bytes.Equal(uploaded, sql) {
		t.Errorf("uploaded %q, want the dump", uploaded)
	}
	// The presigned URL is its own authorisation; sending the API
	// token to R2 as well would be handing a credential to a service
	// that never asked for one.
	if uploadAuth != "" {
		t.Errorf("the presigned upload carried an Authorization header: %q", uploadAuth)
	}
	if res.FinalBookmark != "after" || res.NumQueries != 7 || res.Meta.RowsWritten != 12 {
		t.Errorf("result = %+v", res)
	}

	var init map[string]any
	if err := json.Unmarshal([]byte(srv.calls[0].Body), &init); err != nil {
		t.Fatalf("init body: %v", err)
	}
	if init["action"] != "init" || init["etag"] == "" {
		t.Errorf("init body = %s", srv.calls[0].Body)
	}
	var ingest map[string]any
	if err := json.Unmarshal([]byte(srv.calls[1].Body), &ingest); err != nil {
		t.Fatalf("ingest body: %v", err)
	}
	if ingest["action"] != "ingest" || ingest["filename"] != "acct/db.sql" {
		t.Errorf("ingest body = %s", srv.calls[1].Body)
	}
	if ingest["etag"] != init["etag"] {
		t.Errorf("ingest etag %v does not match the init etag %v", ingest["etag"], init["etag"])
	}
}

// ImportFile hashes by streaming so a dump larger than memory does
// not have to be held in it; the etag must come out the same either
// way, or D1 would reject the file it was just told about.
func TestImportFileHashesTheSameAsImport(t *testing.T) {
	sql := []byte("CREATE TABLE t (a);\n")
	path := filepath.Join(t.TempDir(), "dump.sql")
	if err := os.WriteFile(path, sql, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	uploads := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer uploads.Close()

	etags := map[string]string{}
	run := func(key string, call func(a *d1.Admin) error) {
		srv := newAdminServer(t)
		n := 0
		srv.handler = func(w http.ResponseWriter, _ *http.Request, body []byte) {
			n++
			if n == 1 {
				var init map[string]any
				_ = json.Unmarshal(body, &init)
				etags[key], _ = init["etag"].(string)
				fmt.Fprint(w, envelope(`{"upload_url":"`+uploads.URL+`/put","filename":"f"}`))
				return
			}
			fmt.Fprint(w, envelope(`{"status":"complete","result":{"final_bookmark":"after"}}`))
		}
		if err := call(srv.admin(d1.WithPollInterval(time.Millisecond))); err != nil {
			t.Fatalf("%s: %v", key, err)
		}
	}

	run("bytes", func(a *d1.Admin) error {
		_, err := a.Import(context.Background(), "u1", sql)
		return err
	})
	run("file", func(a *d1.Admin) error {
		_, err := a.ImportFile(context.Background(), "u1", path)
		return err
	})

	if etags["bytes"] == "" || etags["bytes"] != etags["file"] {
		t.Errorf("etags differ: %q vs %q", etags["bytes"], etags["file"])
	}
}

func TestImportReportsARejectedUpload(t *testing.T) {
	uploads := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, "signature expired")
	}))
	defer uploads.Close()

	srv := newAdminServer(t)
	srv.handler = func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		fmt.Fprint(w, envelope(`{"upload_url":"`+uploads.URL+`/put","filename":"f"}`))
	}
	_, err := srv.admin().Import(context.Background(), "u1", []byte("SELECT 1;"))
	if !errors.Is(err, d1.ErrUploadRejected) {
		t.Fatalf("err = %v, want ErrUploadRejected", err)
	}
	if !strings.Contains(err.Error(), "signature expired") {
		t.Errorf("error %q drops what the upload said", err)
	}
}

func TestAdminRefusesAnEmptyDatabaseID(t *testing.T) {
	srv := newAdminServer(t)
	srv.handler = func(w http.ResponseWriter, _ *http.Request, _ []byte) { fmt.Fprint(w, envelope(`{}`)) }
	a := srv.admin()
	ctx := context.Background()

	checks := map[string]func() error{
		"Get":    func() error { _, err := a.Get(ctx, ""); return err },
		"Delete": func() error { return a.Delete(ctx, "") },
		"Bookmark": func() error {
			_, err := a.Bookmark(ctx, "")
			return err
		},
		"Export": func() error {
			_, err := a.Export(ctx, "", d1.ExportOptions{})
			return err
		},
		"Import": func() error {
			_, err := a.Import(ctx, "", nil)
			return err
		},
		"SetReadReplication": func() error {
			_, err := a.SetReadReplication(ctx, "", d1.ReplicationAuto)
			return err
		},
	}
	for name, fn := range checks {
		if err := fn(); !errors.Is(err, d1.ErrNoDatabaseID) {
			t.Errorf("%s err = %v, want ErrNoDatabaseID", name, err)
		}
	}
	if len(srv.calls) != 0 {
		t.Errorf("%d requests reached the server", len(srv.calls))
	}
}
