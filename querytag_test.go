package drops_test

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/bernardoforcillo/drops"
)

func TestTagStatementWithoutTagsIsIdentity(t *testing.T) {
	const sql = `SELECT * FROM "users" WHERE "id" = $1`
	if got := drops.TagStatement(context.Background(), sql); got != sql {
		t.Fatalf("untagged ctx rewrote the statement:\n got %q\nwant %q", got, sql)
	}
}

func TestTagStatementRendersSortedPairs(t *testing.T) {
	// Declared out of order and rendered in order: the SQLCommenter
	// format is key-sorted, so the same tags always render the same
	// text and two call sites do not become two statements.
	ctx := drops.WithQueryTags(context.Background(),
		drops.Tag{Key: "controller", Value: "users"},
		drops.Tag{Key: "action", Value: "show"},
	)
	ctx = drops.WithQueryTag(ctx, "request_id", "7f3a")

	got := drops.TagStatement(ctx, "SELECT 1")
	const want = `SELECT 1 /*action='show',controller='users',request_id='7f3a'*/`
	if got != want {
		t.Fatalf("got  %q\nwant %q", got, want)
	}
}

// The injection this feature would otherwise introduce: a value that
// closes the comment and continues as SQL. It must be impossible, so
// this asserts the encoding rather than the absence of a known-bad
// string — no * and no / can reach the rendered comment at all.
func TestTagValueCannotEscapeTheComment(t *testing.T) {
	ctx := drops.WithQueryTag(context.Background(), "controller",
		`*/ ; DROP TABLE users; --`)

	got := drops.TagStatement(ctx, "SELECT 1")

	body, ok := commentBody(got)
	if !ok {
		t.Fatalf("no trailing comment in %q", got)
	}
	// The closed grammar the encoding promises: the pairs' own
	// punctuation, percent escapes, and the unreserved set. Nothing in
	// it can terminate the comment or open a literal.
	const allowed = "abcdefghijklmnopqrstuvwxyz" +
		"ABCDEFGHIJKLMNOPQRSTUVWXYZ" + "0123456789" + "-._~" + "%'=,"
	if i := strings.IndexFunc(body, func(r rune) bool {
		return !strings.ContainsRune(allowed, r)
	}); i >= 0 {
		t.Fatalf("comment body carries %q, outside the permitted grammar: %q", body[i], body)
	}
	if !strings.Contains(got, "%2A%2F") {
		t.Fatalf("*/ was not percent-encoded: %q", got)
	}
	if strings.Count(got, "*/") != 1 {
		t.Fatalf("statement has %d comment terminators, want 1: %q",
			strings.Count(got, "*/"), got)
	}
}

func TestTagValueEncodesNewlineAndQuote(t *testing.T) {
	ctx := drops.WithQueryTag(context.Background(), "action", "sh'ow\nnext")
	got := drops.TagStatement(ctx, "SELECT 1")

	const want = `SELECT 1 /*action='sh%27ow%0Anext'*/`
	if got != want {
		t.Fatalf("got  %q\nwant %q", got, want)
	}
	if strings.Count(got, "\n") != 0 {
		t.Fatalf("a raw newline survived into the statement: %q", got)
	}
}

// MySQL executes /*! … */ and reads /*+ … */ as an optimizer hint. The
// encoding keeps both spellings unreachable no matter what a caller
// names its first tag.
func TestTagKeyCannotOpenAMySQLExecutableComment(t *testing.T) {
	for _, key := range []string{"!40101 SELECT", "+ MAX_EXECUTION_TIME(1)"} {
		ctx := drops.WithQueryTag(context.Background(), key, "x")
		got := drops.TagStatement(ctx, "SELECT 1")
		if strings.Contains(got, "/*!") || strings.Contains(got, "/*+") {
			t.Fatalf("key %q produced an executable comment: %q", key, got)
		}
	}
}

func TestTagsAccumulateAndInnermostKeyWins(t *testing.T) {
	outer := drops.WithQueryTag(context.Background(), "controller", "users")
	inner := drops.WithQueryTag(outer, "controller", "admin/users")
	sibling := drops.WithQueryTag(outer, "action", "index")

	if got := drops.TagStatement(inner, "SELECT 1"); !strings.Contains(got, `controller='admin%2Fusers'`) {
		t.Fatalf("inner ctx did not override the key: %q", got)
	}
	// The sibling branch must be unaffected by the inner one: contexts
	// share the parent's slice and a write through it would leak
	// across requests.
	if got := drops.TagStatement(sibling, "SELECT 1"); got != `SELECT 1 /*action='index',controller='users'*/` {
		t.Fatalf("sibling ctx saw a write from another branch: %q", got)
	}
	if got := drops.TagStatement(outer, "SELECT 1"); got != `SELECT 1 /*controller='users'*/` {
		t.Fatalf("parent ctx was mutated by its children: %q", got)
	}
}

func TestQueryTagsReturnsACopy(t *testing.T) {
	ctx := drops.WithQueryTag(context.Background(), "controller", "users")
	tags := drops.QueryTags(ctx)
	if len(tags) != 1 {
		t.Fatalf("QueryTags = %v, want one tag", tags)
	}
	tags[0].Value = "tampered"
	if got := drops.TagStatement(ctx, "SELECT 1"); !strings.Contains(got, "users") {
		t.Fatalf("mutating the returned slice reached the context: %q", got)
	}
}

func TestEmptyKeyIsDropped(t *testing.T) {
	ctx := drops.WithQueryTag(context.Background(), "", "orphan")
	if got := drops.TagStatement(ctx, "SELECT 1"); got != "SELECT 1" {
		t.Fatalf("a keyless tag was rendered: %q", got)
	}
}

// A statement whose last line opens a -- comment would otherwise
// swallow the tag, losing the attribution silently.
func TestTagSurvivesATrailingLineComment(t *testing.T) {
	ctx := drops.WithQueryTag(context.Background(), "action", "show")
	got := drops.TagStatement(ctx, "SELECT 1 -- chosen by the planner hint above\n")
	const want = "SELECT 1 -- chosen by the planner hint above\n/*action='show'*/"
	if got != want {
		t.Fatalf("got  %q\nwant %q", got, want)
	}
}

func TestTraceparentPinsTheKey(t *testing.T) {
	tp := "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"
	ctx := drops.WithQueryTags(context.Background(), drops.Traceparent(tp))
	got := drops.TagStatement(ctx, "SELECT 1")
	if !strings.Contains(got, "traceparent='"+tp+"'") {
		t.Fatalf("traceparent not rendered verbatim: %q", got)
	}
}

// ----------------------------------------------------------------------
// Structural guarantees
// ----------------------------------------------------------------------

// A tag must not be able to carry a bound argument. The type is the
// enforcement: two strings, no any, no interface a driver value could
// satisfy. This fails the moment someone widens a field.
func TestTagCannotHoldABoundArgument(t *testing.T) {
	tt := reflect.TypeOf(drops.Tag{})
	for i := 0; i < tt.NumField(); i++ {
		if k := tt.Field(i).Type.Kind(); k != reflect.String {
			t.Fatalf("Tag.%s is %s; a tag field that is not a string can carry a query argument into the comment",
				tt.Field(i).Name, k)
		}
	}
}

// The other half of the guarantee: the function that writes the
// comment is never handed the arguments, so no future edit can
// interpolate one without changing this signature.
func TestTagStatementNeverSeesArguments(t *testing.T) {
	ft := reflect.TypeOf(drops.TagStatement)
	if ft.IsVariadic() {
		t.Fatal("TagStatement is variadic; args could be passed to it")
	}
	if ft.NumIn() != 2 {
		t.Fatalf("TagStatement takes %d parameters, want exactly (context.Context, string)", ft.NumIn())
	}
	ctxType := reflect.TypeOf((*context.Context)(nil)).Elem()
	if ft.In(0) != ctxType || ft.In(1).Kind() != reflect.String {
		t.Fatalf("TagStatement signature is (%s, %s); want (context.Context, string)", ft.In(0), ft.In(1))
	}
}

// ----------------------------------------------------------------------
// Cost
// ----------------------------------------------------------------------

func BenchmarkTagStatementOff(b *testing.B) {
	ctx := context.Background()
	const sql = `SELECT "id", "email" FROM "users" WHERE "id" = $1`
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		sink = drops.TagStatement(ctx, sql)
	}
}

// A realistic depth of context values under the tags, so the lookup
// walks a chain the way it does in a request handler.
func BenchmarkTagStatementOffDeepContext(b *testing.B) {
	ctx := context.Background()
	for i := 0; i < 8; i++ {
		ctx = context.WithValue(ctx, benchKey(i), i)
	}
	const sql = `SELECT "id", "email" FROM "users" WHERE "id" = $1`
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		sink = drops.TagStatement(ctx, sql)
	}
}

func BenchmarkTagStatementOn(b *testing.B) {
	ctx := drops.WithQueryTags(context.Background(),
		drops.Tag{Key: "controller", Value: "users"},
		drops.Tag{Key: "action", Value: "show"},
		drops.Tag{Key: "request_id", Value: "7f3a91c0"},
	)
	const sql = `SELECT "id", "email" FROM "users" WHERE "id" = $1`
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		sink = drops.TagStatement(ctx, sql)
	}
}

type benchKey int

var sink string

// commentBody returns the text between the final /* and */ of s.
func commentBody(s string) (string, bool) {
	i := strings.LastIndex(s, "/*")
	if i < 0 || !strings.HasSuffix(s, "*/") {
		return "", false
	}
	return s[i+2 : len(s)-2], true
}

// The separator exists to keep the comment off the end of the
// statement; with no statement there is nothing to separate it from,
// and the leading space is just a byte nobody asked for. Reachable
// from every dialect's DB.Exec, which hands whatever text it was given
// straight to TagStatement.
func TestTagStatementOfNothingHasNoLeadingSpace(t *testing.T) {
	ctx := drops.WithQueryTag(context.Background(), "controller", "users")
	if got := drops.TagStatement(ctx, ""); got != "/*controller='users'*/" {
		t.Errorf("got %q", got)
	}
	// Whitespace-only is the same statement with the same answer:
	// TrimRight has already reduced it to nothing.
	if got := drops.TagStatement(ctx, "  \n"); got != "/*controller='users'*/" {
		t.Errorf("whitespace-only: got %q", got)
	}
}
