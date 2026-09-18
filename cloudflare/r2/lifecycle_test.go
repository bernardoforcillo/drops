package r2_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/bernardoforcillo/drops/cloudflare/r2"
)

func TestDeleteAfterRendersAnAgeRule(t *testing.T) {
	srv := newServer(t)
	srv.handler = func(w http.ResponseWriter, _ *http.Request, _ []byte) { fmt.Fprint(w, envelope(`{}`)) }

	err := srv.client().Bucket("backups").SetLifecycle(context.Background(), []r2.LifecycleRule{
		r2.DeleteAfter("d1-retention", "d1/", 90*24*time.Hour),
	})
	if err != nil {
		t.Fatalf("SetLifecycle: %v", err)
	}
	c := srv.last()
	if c.Method != http.MethodPut || !strings.HasSuffix(c.Path, "/buckets/backups/lifecycle") {
		t.Errorf("%s %s", c.Method, c.Path)
	}

	var sent struct {
		Rules []struct {
			ID         string `json:"id"`
			Enabled    bool   `json:"enabled"`
			Conditions struct {
				Prefix string `json:"prefix"`
			} `json:"conditions"`
			DeleteObjectsTransition struct {
				Condition struct {
					Type   string `json:"type"`
					MaxAge int64  `json:"maxAge"`
				} `json:"condition"`
			} `json:"deleteObjectsTransition"`
		} `json:"rules"`
	}
	if err := json.Unmarshal(c.Body, &sent); err != nil {
		t.Fatalf("body: %v", err)
	}
	if len(sent.Rules) != 1 {
		t.Fatalf("%d rules", len(sent.Rules))
	}
	r := sent.Rules[0]
	if r.ID != "d1-retention" || !r.Enabled || r.Conditions.Prefix != "d1/" {
		t.Errorf("rule = %+v", r)
	}
	if r.DeleteObjectsTransition.Condition.Type != "Age" {
		t.Errorf("condition type = %q", r.DeleteObjectsTransition.Condition.Type)
	}
	if want := int64((90 * 24 * time.Hour).Seconds()); r.DeleteObjectsTransition.Condition.MaxAge != want {
		t.Errorf("maxAge = %d, want %d seconds", r.DeleteObjectsTransition.Condition.MaxAge, want)
	}
}

func TestCoolAfterRendersAStorageClassTransition(t *testing.T) {
	srv := newServer(t)
	srv.handler = func(w http.ResponseWriter, _ *http.Request, _ []byte) { fmt.Fprint(w, envelope(`{}`)) }

	if err := srv.client().Bucket("backups").SetLifecycle(context.Background(), []r2.LifecycleRule{
		r2.CoolAfter("cool", "d1/", 30*24*time.Hour),
	}); err != nil {
		t.Fatalf("SetLifecycle: %v", err)
	}
	body := string(srv.last().Body)
	if !strings.Contains(body, `"storageClass":"InfrequentAccess"`) {
		t.Errorf("body = %s", body)
	}
	if !strings.Contains(body, `"storageClassTransitions"`) {
		t.Errorf("body = %s", body)
	}
}

// A rule that does nothing is accepted by R2 and looks like a
// retention policy, which is the worst way for one to be wrong.
func TestSetLifecycleRefusesRulesThatDoNothing(t *testing.T) {
	srv := newServer(t)
	srv.handler = func(w http.ResponseWriter, _ *http.Request, _ []byte) { fmt.Fprint(w, envelope(`{}`)) }
	b := srv.client().Bucket("backups")
	ctx := context.Background()

	cases := []struct {
		name  string
		rules []r2.LifecycleRule
		want  error
	}{
		{"no id", []r2.LifecycleRule{{Prefix: "d1/", Delete: r2.Transition{After: time.Hour}}}, r2.ErrNoRuleID},
		{"no transition", []r2.LifecycleRule{{ID: "x", Prefix: "d1/"}}, r2.ErrEmptyRule},
		{"age and date", []r2.LifecycleRule{{
			ID:     "x",
			Delete: r2.Transition{After: time.Hour, On: time.Now()},
		}}, r2.ErrAmbiguousCondition},
		{"unknown class", []r2.LifecycleRule{{
			ID: "x",
			Transitions: []r2.StorageClassTransition{{
				Transition: r2.Transition{After: time.Hour}, StorageClass: "Glacier",
			}},
		}}, r2.ErrUnknownStorageClass},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := b.SetLifecycle(ctx, tc.rules); !errors.Is(err, tc.want) {
				t.Errorf("err = %v, want %v", err, tc.want)
			}
		})
	}
	if len(srv.calls) != 0 {
		t.Errorf("%d requests reached the server; the checks are local", len(srv.calls))
	}
}

// R2 addresses rules by id, so two rules sharing one is a set where
// only one of them survives — silently.
func TestSetLifecycleRefusesDuplicateIDs(t *testing.T) {
	srv := newServer(t)
	srv.handler = func(w http.ResponseWriter, _ *http.Request, _ []byte) { fmt.Fprint(w, envelope(`{}`)) }
	err := srv.client().Bucket("b").SetLifecycle(context.Background(), []r2.LifecycleRule{
		r2.DeleteAfter("dup", "a/", time.Hour),
		r2.DeleteAfter("dup", "b/", time.Hour),
	})
	if err == nil || !strings.Contains(err.Error(), "share the ID") {
		t.Errorf("err = %v", err)
	}
}

func TestLifecycleReadsRulesBack(t *testing.T) {
	srv := newServer(t)
	srv.handler = func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		fmt.Fprint(w, envelope(`{"rules":[
			{"id":"retain","enabled":true,"conditions":{"prefix":"d1/"},
			 "deleteObjectsTransition":{"condition":{"type":"Age","maxAge":7776000}},
			 "storageClassTransitions":[{"condition":{"type":"Age","maxAge":2592000},"storageClass":"InfrequentAccess"}],
			 "abortMultipartUploadsTransition":{"condition":{"type":"Age","maxAge":86400}}},
			{"id":"deadline","enabled":false,"conditions":{"prefix":""},
			 "deleteObjectsTransition":{"condition":{"type":"Date","date":"2027-01-01T00:00:00Z"}}}]}`))
	}

	rules, err := srv.client().Bucket("backups").Lifecycle(context.Background())
	if err != nil {
		t.Fatalf("Lifecycle: %v", err)
	}
	if len(rules) != 2 {
		t.Fatalf("%d rules", len(rules))
	}

	r := rules[0]
	if r.ID != "retain" || !r.Enabled || r.Prefix != "d1/" {
		t.Errorf("rule = %+v", r)
	}
	if r.Delete.After != 90*24*time.Hour {
		t.Errorf("delete after %s, want 90 days", r.Delete.After)
	}
	if len(r.Transitions) != 1 || r.Transitions[0].After != 30*24*time.Hour {
		t.Errorf("transitions = %+v", r.Transitions)
	}
	if r.AbortIncompleteUploadsAfter != 24*time.Hour {
		t.Errorf("abort after %s", r.AbortIncompleteUploadsAfter)
	}

	// A date condition is a deadline the whole bucket crosses at
	// once, not an age.
	d := rules[1]
	if d.Delete.After != 0 || d.Delete.On.IsZero() {
		t.Errorf("date rule = %+v", d.Delete)
	}
	if d.Enabled {
		t.Error("a disabled rule read back as enabled")
	}
}

// Removing the policy is a thing to do on purpose.
func TestClearLifecycleSendsAnEmptySet(t *testing.T) {
	srv := newServer(t)
	srv.handler = func(w http.ResponseWriter, _ *http.Request, _ []byte) { fmt.Fprint(w, envelope(`{}`)) }
	if err := srv.client().Bucket("b").ClearLifecycle(context.Background()); err != nil {
		t.Fatalf("ClearLifecycle: %v", err)
	}
	if !strings.Contains(string(srv.last().Body), `"rules":[]`) {
		t.Errorf("body = %s", srv.last().Body)
	}
}

func TestTemporaryCredentials(t *testing.T) {
	srv := newServer(t)
	srv.handler = func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		fmt.Fprint(w, envelope(`{"accessKeyId":"ak","secretAccessKey":"sk","sessionToken":"st"}`))
	}

	creds, err := srv.client().TemporaryCredentials(context.Background(), r2.CredentialRequest{
		Bucket:            "backups",
		ParentAccessKeyID: "parent-key",
		Permission:        r2.ObjectReadOnly,
		TTL:               15 * time.Minute,
		Prefixes:          []string{"d1/2026-09-15/"},
	})
	if err != nil {
		t.Fatalf("TemporaryCredentials: %v", err)
	}
	if creds.AccessKeyID != "ak" || creds.SecretAccessKey != "sk" || creds.SessionToken != "st" {
		t.Errorf("credentials = %+v", creds)
	}
	c := srv.last()
	if !strings.HasSuffix(c.Path, "/r2/temp-access-credentials") {
		t.Errorf("path = %s", c.Path)
	}
	var sent map[string]any
	if err := json.Unmarshal(c.Body, &sent); err != nil {
		t.Fatalf("body: %v", err)
	}
	if sent["permission"] != "object-read-only" || sent["ttlSeconds"] != float64(900) {
		t.Errorf("body = %s", c.Body)
	}
	if sent["parentAccessKeyId"] != "parent-key" {
		t.Errorf("body = %s", c.Body)
	}
	if _, ok := sent["objects"]; ok {
		t.Errorf("an empty object list was sent: %s", c.Body)
	}
}

func TestTemporaryCredentialsRefusesAnUnboundedOne(t *testing.T) {
	srv := newServer(t)
	srv.handler = func(w http.ResponseWriter, _ *http.Request, _ []byte) { fmt.Fprint(w, envelope(`{}`)) }
	c := srv.client()
	ctx := context.Background()

	full := r2.CredentialRequest{
		Bucket: "b", ParentAccessKeyID: "k", Permission: r2.ObjectReadOnly, TTL: time.Minute,
	}
	cases := []struct {
		name string
		mut  func(*r2.CredentialRequest)
		want string
	}{
		{"no bucket", func(r *r2.CredentialRequest) { r.Bucket = "" }, "bucket name is empty"},
		{"no parent key", func(r *r2.CredentialRequest) { r.ParentAccessKeyID = "" }, "parent R2 access key"},
		{"bad permission", func(r *r2.CredentialRequest) { r.Permission = "root" }, "unknown permission"},
		{"no ttl", func(r *r2.CredentialRequest) { r.TTL = 0 }, "not temporary"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := full
			tc.mut(&req)
			_, err := c.TemporaryCredentials(ctx, req)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want it to mention %q", err, tc.want)
			}
		})
	}
	if len(srv.calls) != 0 {
		t.Errorf("%d requests reached the server", len(srv.calls))
	}
}

func TestLifecycleRefusesAnEmptyBucketName(t *testing.T) {
	srv := newServer(t)
	srv.handler = func(w http.ResponseWriter, _ *http.Request, _ []byte) { fmt.Fprint(w, envelope(`{}`)) }
	b := srv.client().Bucket("")
	ctx := context.Background()

	if _, err := b.Lifecycle(ctx); !errors.Is(err, r2.ErrNoBucketName) {
		t.Errorf("Lifecycle err = %v", err)
	}
	if err := b.SetLifecycle(ctx, nil); !errors.Is(err, r2.ErrNoBucketName) {
		t.Errorf("SetLifecycle err = %v", err)
	}
}
