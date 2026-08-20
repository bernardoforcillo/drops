package drops

import (
	"context"
	"strings"
)

// Query tagging attaches application context — which controller, which
// action, which request — to the statement itself, as a trailing SQL
// comment in the SQLCommenter format Rails and EF Core already emit:
//
//	ctx = drops.WithQueryTags(ctx,
//	    drops.Tag{Key: "controller", Value: "users"},
//	    drops.Tag{Key: "action", Value: "show"},
//	)
//	rows, err := db.Query(ctx, "SELECT * FROM users WHERE id = $1", id)
//	// SELECT * FROM users WHERE id = $1 /*action='show',controller='users'*/
//
// The point is attribution after the fact. A slow row in
// pg_stat_statements, a line in MySQL's slow log, a statement in a
// proxy's log: none of them carry any trace of which piece of
// application code issued the query, and the comment is the only part
// of a statement that survives intact into every one of those places.
// Sorting the top-20 slow queries and then grepping the codebase for
// something that looks like them is the state of the art in Go, and
// this replaces it.
//
// A context with no tags renders exactly the statement it renders
// today — the whole feature is inert until someone calls
// [WithQueryTags], so it costs one [context.Context.Value] lookup per
// statement and nothing else.
//
// # Why this is not a Hook
//
// [Hook] fires after the operation has completed and its return value
// is discarded, by design: a hook observes, it cannot rewrite. Tagging
// has to happen before the statement reaches the driver, so it cannot
// be a hook, and the alternative — general statement-rewriting
// middleware — is a seam drops deliberately refuses, because a library
// that lets arbitrary code edit SQL on its way out can no longer make
// any promise about what it sends. Tagging is therefore a first-class
// feature with a fixed, closed grammar: a sorted list of encoded
// key/value pairs in a trailing comment, and nothing else.
//
// # Where the comment goes
//
// At the end, per the SQLCommenter convention. A leading comment is
// the more common instinct and it is the wrong one: too much of the
// surrounding ecosystem switches on a statement's first token.
// MySQL reads /*! and /*+ at the front as executable comments and
// optimizer hints; connection proxies split reads from writes by
// looking at the leading keyword; database/sql wrappers and
// statement-type sniffers do the same. Putting anything in front of
// SELECT breaks a class of tooling nobody is expecting to break.
//
// The usual argument for a leading comment — that a trailing one is
// lost when the statement is wrapped into a subquery — does not apply
// here, because drops appends the comment at the point the finished
// statement is handed to the driver, not while an expression is still
// being composed. Nothing wraps it afterwards; there is no "afterwards".
//
// # Encoding
//
// Keys and values are percent-encoded to RFC 3986's unreserved set
// (letters, digits and -._~) before they are written, which is what
// the SQLCommenter spec calls for and is also what makes this safe.
// The values come from application code and land inside a SQL
// comment: a value containing */ would end the comment early and
// hand the rest of itself to the parser as SQL. That has to be
// impossible rather than unlikely, so both * and / are outside the
// unreserved set and always encode, as do the single quote that
// delimits a value, the newline that would end a line comment, and
// the ! and + that would turn the comment's opening into a MySQL
// executable comment. What reaches the server can only ever be
// [A-Za-z0-9-._~%'=,] — a grammar with no way out of the comment.
//
// # What cannot be tagged
//
// Query arguments. A [Tag] holds two strings and nothing else: no
// any, no fmt.Stringer, no driver.Valuer, so there is no conversion
// from a bound argument to a tag to reach for. And the function that
// builds the comment, [TagStatement], is handed the statement text
// and the context — the argument slice is not in scope where the
// comment is written, so no future edit can quietly start
// interpolating one.
//
// This is a deliberate structural refusal, not an oversight. Bound
// arguments are user data; a tracing backend, a slow-query log and a
// pg_stat_statements view are all places that data is not permitted
// to go, and all three are exactly where a query comment ends up.
//
// # What a tag value is, and is not
//
// The paragraph above stops drops from carrying user data into a
// comment by accident. It does not stop a caller from putting it
// there on purpose, and the asymmetry is worth stating plainly rather
// than leaving as an inference: whatever goes into a [Tag] goes to
// every place the statement text goes. pg_stat_statements, the slow
// log, the proxy's access log, the APM vendor, and whatever backup of
// those exists — none of which are governed by the retention policy
// covering the table the row came from.
//
// So a tag is a name for a piece of code, not a name for a person.
// controller, action, service, deployment, feature flag, request id:
// all fine. An email address, an account id, a tenant name, a search
// term: not. drops has pg.Secret and the PII column marking for values
// that must not leak, and a tag is the one place in this library where
// that protection does not reach, because the protection is about
// bound arguments and a tag is not one.
//
// # Reading the tags back
//
// They come back encoded, which is the price of the grammar above.
// controller='admin/users' is recorded as controller='admin%2Fusers',
// a space as %20, and café as caf%C3%A9. Grep a slow log for the
// literal you wrote and you will find nothing; grep for the encoded
// form, or decode the comment before matching. Anything outside
// [A-Za-z0-9-._~] is encoded, so a tag value made of those characters
// reads back exactly as written — which is a reason to keep tag
// values to that alphabet where you can.
//
// # Cardinality
//
// Tag values become part of the statement text, so a distinct value
// makes a distinct statement for anything that caches by text —
// server-side prepared statements, client-side statement caches,
// proxy plan caches. Low-cardinality tags (controller, action,
// service, deployment) are free. A per-request id is the useful
// exception and the expensive one: it is what lets a single trace be
// matched to a single logged statement, and it defeats statement
// caching for exactly as long as it is set. Set it on the requests
// worth tracing, not on all of them.

// Tag is one key/value pair appended to a statement as part of its
// query comment.
//
// Both fields are strings on purpose. See the package documentation on
// query tagging: a tag must not be able to carry a bound argument, and
// the cheapest way to guarantee that is to give the type no way to
// hold one.
type Tag struct {
	// Key names the dimension, e.g. "controller" or "action". A tag
	// with an empty key is dropped.
	Key string

	// Value is the value for this statement, e.g. "users" or "show".
	// An empty value is kept — key='' is a meaningful observation.
	Value string
}

// Traceparent returns the tag carrying a W3C traceparent header value,
// which is the key APM backends (Google Cloud SQL Insights among them)
// look for when they join a logged statement to a distributed trace:
//
//	drops.WithQueryTags(ctx, drops.Traceparent(carrier["traceparent"]))
//
// It exists only to pin the spelling of the key; the value has to come
// from the caller's tracing SDK. drops/otel cannot produce one — it is
// driven by [Hook], which fires after the statement is already on the
// wire, and its Tracer interface deliberately exposes no span context.
func Traceparent(v string) Tag { return Tag{Key: "traceparent", Value: v} }

// queryTagKey is the context key the tag slice hangs off. An unexported
// zero-size struct type, so no other package can collide with it.
type queryTagKey struct{}

// WithQueryTags returns a context carrying tags in addition to any
// already on ctx. Tags accumulate down a call chain — an HTTP
// middleware sets controller and action, a handler adds a request id —
// and a repeated key replaces the value set further up, so the
// innermost caller wins.
//
// Tags are kept sorted by key, which is both what the SQLCommenter
// spec requires and what keeps rendering to a plain concatenation:
// the sorting and de-duplication happen here, once per request, rather
// than in [TagStatement], which runs once per statement.
func WithQueryTags(ctx context.Context, tags ...Tag) context.Context {
	if ctx == nil || len(tags) == 0 {
		return ctx
	}
	prev, _ := ctx.Value(queryTagKey{}).([]Tag)
	// Copy rather than append in place: the parent context's slice is
	// shared with every other branch of the call tree and must not be
	// written through.
	next := make([]Tag, len(prev), len(prev)+len(tags))
	copy(next, prev)
	for _, t := range tags {
		if t.Key == "" {
			continue
		}
		next = insertTag(next, t)
	}
	if len(next) == 0 {
		return ctx
	}
	return context.WithValue(ctx, queryTagKey{}, next)
}

// WithQueryTag is [WithQueryTags] for the single-pair case, which is
// most of them.
func WithQueryTag(ctx context.Context, key, value string) context.Context {
	return WithQueryTags(ctx, Tag{Key: key, Value: value})
}

// QueryTags returns the tags on ctx, sorted by key. The result is a
// fresh slice: mutating it does not affect ctx or any context derived
// from it.
func QueryTags(ctx context.Context) []Tag {
	if ctx == nil {
		return nil
	}
	tags, _ := ctx.Value(queryTagKey{}).([]Tag)
	if len(tags) == 0 {
		return nil
	}
	return append([]Tag(nil), tags...)
}

// insertTag places t into the key-sorted tags, replacing an existing
// entry with the same key. Linear because the slice is a handful of
// entries; a map plus a sort would allocate more and compare slower at
// this size.
func insertTag(tags []Tag, t Tag) []Tag {
	for i, existing := range tags {
		switch {
		case existing.Key == t.Key:
			tags[i] = t
			return tags
		case existing.Key > t.Key:
			tags = append(tags, Tag{})
			copy(tags[i+1:], tags[i:])
			tags[i] = t
			return tags
		}
	}
	return append(tags, t)
}

// TagStatement returns sql with the query tags on ctx appended as a
// trailing SQLCommenter comment, or sql unchanged when ctx carries no
// tags. Every dialect's DB calls it on the way to the driver, which is
// the one place a finished statement and the caller's context are both
// in hand.
//
// Note the signature: statement text and context, no arguments. That
// is the guarantee described in the package documentation on query
// tagging — bound arguments are not reachable from where the comment
// is built, so they cannot end up inside it.
func TagStatement(ctx context.Context, sql string) string {
	if ctx == nil {
		return sql
	}
	tags, _ := ctx.Value(queryTagKey{}).([]Tag)
	if len(tags) == 0 {
		return sql
	}
	return appendTagComment(sql, tags)
}

// appendTagComment writes the comment. Split from [TagStatement] so
// the untagged path — every statement in a program that never calls
// [WithQueryTags] — is a value lookup and a length check, small enough
// for the compiler to inline into the caller.
func appendTagComment(sql string, tags []Tag) string {
	// Trailing whitespace would put the comment on a line of its own
	// for no reason, and it hides whether the statement ends inside a
	// line comment.
	sql = strings.TrimRight(sql, " \t\r\n")

	var b strings.Builder
	// 2 for "/*", 2 for "*/", and about 16 bytes for a typical
	// key='value' pair. An over-estimate costs one transient
	// allocation; an under-estimate costs a copy of the whole
	// statement.
	b.Grow(len(sql) + 4 + 16*len(tags))
	b.WriteString(sql)
	if endsInLineComment(sql) {
		// A statement whose last line opened a -- comment would
		// swallow the tag and silently lose the attribution. Cheaper
		// to start a new line than to explain the missing comment.
		b.WriteByte('\n')
	} else {
		b.WriteByte(' ')
	}
	b.WriteString("/*")
	for i, t := range tags {
		if i > 0 {
			b.WriteByte(',')
		}
		encodeTag(&b, t.Key)
		b.WriteString("='")
		encodeTag(&b, t.Value)
		b.WriteByte('\'')
	}
	b.WriteString("*/")
	return b.String()
}

// endsInLineComment reports whether sql's last line contains a --
// comment opener, in which case anything appended to that line is
// commented out.
//
// It does not parse: a -- inside a string literal on the last line
// reads as a comment here and costs a newline nobody needed. That is
// the right way round to be wrong — the alternative, a real lexer on
// the hot path of every tagged statement, buys a cosmetic improvement
// at a cost this has no business paying.
func endsInLineComment(sql string) bool {
	if i := strings.LastIndexByte(sql, '\n'); i >= 0 {
		sql = sql[i+1:]
	}
	return strings.Contains(sql, "--")
}

// encodeTag percent-encodes s into b, escaping everything outside RFC
// 3986's unreserved set. Byte-wise, so a multi-byte rune encodes as
// the percent-escapes of its UTF-8 bytes, which is what a URL decoder
// on the other end expects.
//
// net/url would do this, very nearly: QueryEscape writes a space as +
// and PathEscape leaves a pile of sub-delims — including ! and + — in
// place. The set has to be exact here, because it is the whole of the
// injection argument, so it is spelled out rather than borrowed.
func encodeTag(b *strings.Builder, s string) {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if unreservedTagByte(c) {
			b.WriteByte(c)
			continue
		}
		b.WriteByte('%')
		b.WriteByte(upperHex[c>>4])
		b.WriteByte(upperHex[c&0x0f])
	}
}

const upperHex = "0123456789ABCDEF"

// unreservedTagByte reports whether c is in RFC 3986's unreserved set
// and may therefore be written into a query comment as itself.
func unreservedTagByte(c byte) bool {
	switch {
	case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		return true
	case c == '-', c == '.', c == '_', c == '~':
		return true
	}
	return false
}
