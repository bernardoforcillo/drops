package hyperdrive_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bernardoforcillo/drops/cloudflare"
	"github.com/bernardoforcillo/drops/cloudflare/hyperdrive"
)

type call struct {
	Method string
	Path   string
	Body   string
}

type server struct {
	*httptest.Server

	t       *testing.T
	handler func(w http.ResponseWriter, r *http.Request)
	calls   []call
}

func newServer(t *testing.T) *server {
	t.Helper()
	s := &server{t: t}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		s.calls = append(s.calls, call{Method: r.Method, Path: r.URL.Path, Body: string(raw)})
		s.handler(w, r)
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *server) client() *hyperdrive.Client {
	s.t.Helper()
	cf, err := cloudflare.New("acct",
		cloudflare.WithAPIToken("tok"),
		cloudflare.WithBaseURL(s.URL),
		cloudflare.WithRetryPolicy(cloudflare.RetryPolicy{}),
	)
	if err != nil {
		s.t.Fatalf("cloudflare.New: %v", err)
	}
	return hyperdrive.NewClient(cf)
}

func (s *server) last() call {
	s.t.Helper()
	if len(s.calls) == 0 {
		s.t.Fatal("no request was made")
	}
	return s.calls[len(s.calls)-1]
}

const configReply = `{"success":true,"errors":[],"messages":[],"result":{
	"id":"hd-1","name":"primary",
	"origin":{"scheme":"postgres","host":"db.example.com","port":5432,"database":"app","user":"hyperdrive"},
	"caching":{"disabled":false,"max_age":30,"stale_while_revalidate":5},
	"origin_connection_limit":40,
	"created_on":"2026-09-15T04:05:06Z","modified_on":"2026-09-15T04:05:06Z"}}`

func TestCreateSendsTheOriginAndTheSecret(t *testing.T) {
	srv := newServer(t)
	srv.handler = func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, configReply) }

	cfg, err := srv.client().Create(context.Background(), hyperdrive.NewConfig{
		Name: "primary",
		Origin: hyperdrive.Origin{
			Engine: hyperdrive.PostgreSQL, Host: "db.example.com",
			Database: "app", User: "hyperdrive",
		},
		Password:              "s3cr3t",
		Caching:               hyperdrive.Caching{MaxAge: 30, StaleWhileRevalidate: 5},
		OriginConnectionLimit: 40,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	c := srv.last()
	if c.Method != http.MethodPost || c.Path != "/accounts/acct/hyperdrive/configs" {
		t.Errorf("%s %s", c.Method, c.Path)
	}
	var sent struct {
		Name   string `json:"name"`
		Origin struct {
			Scheme   string `json:"scheme"`
			Port     int    `json:"port"`
			Password string `json:"password"`
		} `json:"origin"`
		Caching struct {
			MaxAge int `json:"max_age"`
		} `json:"caching"`
		Limit int `json:"origin_connection_limit"`
	}
	if err := json.Unmarshal([]byte(c.Body), &sent); err != nil {
		t.Fatalf("body: %v", err)
	}
	if sent.Origin.Scheme != "postgresql" {
		t.Errorf("scheme = %q", sent.Origin.Scheme)
	}
	// An unset port must become the engine's default rather than
	// zero, which Hyperdrive would reject.
	if sent.Origin.Port != 5432 {
		t.Errorf("port = %d, want the PostgreSQL default", sent.Origin.Port)
	}
	if sent.Origin.Password != "s3cr3t" {
		t.Errorf("password did not travel")
	}
	if sent.Caching.MaxAge != 30 || sent.Limit != 40 {
		t.Errorf("body = %s", c.Body)
	}

	if cfg.ID != "hd-1" || cfg.Origin.Engine != hyperdrive.PostgreSQL {
		t.Errorf("config = %+v", cfg)
	}
	if cfg.Caching.MaxAge != 30 || cfg.OriginConnectionLimit != 40 {
		t.Errorf("config = %+v", cfg)
	}
	if cfg.CreatedOn.IsZero() {
		t.Error("CreatedOn was not parsed")
	}
}

func TestMySQLGetsItsOwnDefaultPort(t *testing.T) {
	srv := newServer(t)
	srv.handler = func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, configReply) }

	_, err := srv.client().Create(context.Background(), hyperdrive.NewConfig{
		Name:     "mysql",
		Origin:   hyperdrive.Origin{Engine: hyperdrive.MySQL, Host: "h", Database: "d", User: "u"},
		Password: "p",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if !strings.Contains(srv.last().Body, `"port":3306`) {
		t.Errorf("body = %s", srv.last().Body)
	}
	if !strings.Contains(srv.last().Body, `"scheme":"mysql"`) {
		t.Errorf("body = %s", srv.last().Body)
	}
}

// Cloudflare never returns the password, so an incomplete
// configuration is refused here rather than accepted and left unable
// to reach the origin.
func TestCreateRefusesAnIncompleteConfiguration(t *testing.T) {
	srv := newServer(t)
	srv.handler = func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, configReply) }
	c := srv.client()
	ctx := context.Background()

	full := hyperdrive.NewConfig{
		Name:     "primary",
		Origin:   hyperdrive.Origin{Host: "h", Database: "d", User: "u"},
		Password: "p",
	}
	cases := []struct {
		name string
		mut  func(*hyperdrive.NewConfig)
		want error
	}{
		{"no name", func(n *hyperdrive.NewConfig) { n.Name = "" }, hyperdrive.ErrNoConfigName},
		{"no host", func(n *hyperdrive.NewConfig) { n.Origin.Host = "" }, hyperdrive.ErrNoOrigin},
		{"no database", func(n *hyperdrive.NewConfig) { n.Origin.Database = "" }, hyperdrive.ErrNoOrigin},
		{"no user", func(n *hyperdrive.NewConfig) { n.Origin.User = "" }, hyperdrive.ErrNoOrigin},
		{"no password", func(n *hyperdrive.NewConfig) { n.Password = "" }, hyperdrive.ErrNoPassword},
		{"unknown engine", func(n *hyperdrive.NewConfig) { n.Origin.Engine = "sqlite" }, hyperdrive.ErrIncompleteConfig},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := full
			tc.mut(&cfg)
			if _, err := c.Create(ctx, cfg); !errors.Is(err, tc.want) {
				t.Errorf("err = %v, want %v", err, tc.want)
			}
		})
	}
	if len(srv.calls) != 0 {
		t.Errorf("%d requests reached the server", len(srv.calls))
	}
}

func TestReplaceKeepsTheIDAndSetCachingPatches(t *testing.T) {
	srv := newServer(t)
	srv.handler = func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, configReply) }
	c := srv.client()
	ctx := context.Background()

	_, err := c.Replace(ctx, "hd-1", hyperdrive.NewConfig{
		Name:     "primary",
		Origin:   hyperdrive.Origin{Host: "h", Database: "d", User: "u"},
		Password: "p",
	})
	if err != nil {
		t.Fatalf("Replace: %v", err)
	}
	if got := srv.last(); got.Method != http.MethodPut || !strings.HasSuffix(got.Path, "/configs/hd-1") {
		t.Errorf("%s %s", got.Method, got.Path)
	}

	if _, err := c.SetCaching(ctx, "hd-1", hyperdrive.Caching{Disabled: true}); err != nil {
		t.Fatalf("SetCaching: %v", err)
	}
	got := srv.last()
	if got.Method != http.MethodPatch {
		t.Errorf("method = %s, want PATCH", got.Method)
	}
	// Changing the cache must not need the password again, so the
	// patch must not carry an origin at all.
	if strings.Contains(got.Body, "origin") {
		t.Errorf("caching patch carries an origin: %s", got.Body)
	}
	if !strings.Contains(got.Body, `"disabled":true`) {
		t.Errorf("body = %s", got.Body)
	}
}

func TestListAndFindByName(t *testing.T) {
	srv := newServer(t)
	srv.handler = func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"success":true,"errors":[],"messages":[],"result":[
			{"id":"hd-1","name":"primary","origin":{"scheme":"postgresql","host":"h","port":5432,"database":"d","user":"u"}},
			{"id":"hd-2","name":"analytics","origin":{"scheme":"mysql","host":"h2","port":3306,"database":"d2","user":"u2"}}]}`)
	}
	c := srv.client()
	ctx := context.Background()

	all, err := c.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("%d configurations", len(all))
	}
	if all[1].Origin.Engine != hyperdrive.MySQL {
		t.Errorf("engine = %q", all[1].Origin.Engine)
	}

	found, err := c.FindByName(ctx, "analytics")
	if err != nil {
		t.Fatalf("FindByName: %v", err)
	}
	if found.ID != "hd-2" {
		t.Errorf("found = %+v", found)
	}
	if _, err := c.FindByName(ctx, "absent"); !errors.Is(err, cloudflare.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

// The direct connection is what logical replication, the statement
// registry and everything else in Unsupported need, and it is not the
// one a Worker gets.
func TestDirectConfigAddressesTheOrigin(t *testing.T) {
	srv := newServer(t)
	srv.handler = func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, configReply) }

	cfg, err := srv.client().Get(context.Background(), "hd-1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	direct := cfg.DirectConfig("p@ss/word")
	direct.SSLMode = "require"

	dsn, err := direct.DSN()
	if err != nil {
		t.Fatalf("DSN: %v", err)
	}
	if !strings.Contains(dsn, "db.example.com:5432") {
		t.Errorf("dsn = %q, want the origin host", dsn)
	}
	// The password's slash must be encoded, or the URL parses as a
	// different host entirely.
	if strings.Contains(dsn, "p@ss/word") {
		t.Errorf("dsn = %q, want the password percent-encoded", dsn)
	}
	if !strings.Contains(dsn, "sslmode=require") {
		t.Errorf("dsn = %q", dsn)
	}
}

func TestEmptyIdentifiersAreRefusedLocally(t *testing.T) {
	srv := newServer(t)
	srv.handler = func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, configReply) }
	c := srv.client()
	ctx := context.Background()

	if _, err := c.Get(ctx, ""); !errors.Is(err, hyperdrive.ErrNoConfigID) {
		t.Errorf("Get err = %v", err)
	}
	if err := c.Delete(ctx, ""); !errors.Is(err, hyperdrive.ErrNoConfigID) {
		t.Errorf("Delete err = %v", err)
	}
	if _, err := c.SetCaching(ctx, "", hyperdrive.Caching{}); !errors.Is(err, hyperdrive.ErrNoConfigID) {
		t.Errorf("SetCaching err = %v", err)
	}
	if len(srv.calls) != 0 {
		t.Errorf("%d requests reached the server", len(srv.calls))
	}
}
