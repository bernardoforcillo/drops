package sqlite

import (
	"fmt"
	"io"

	"github.com/bernardoforcillo/drops"
)

// PII tagging keeps sensitive values out of logs and traces. Mark a
// value (or a column) as PII and any logger / hook that formats the
// bound args sees "<redacted>" instead of the real value; the driver
// still receives the plain value because Exec / Query unwrap before
// handing args downstream.
//
//	db.Exec(ctx, `UPDATE users SET password = ? WHERE id = ?`,
//	    sqlite.PII(hashedPw), id)
//
// Or tag the column and let entity binders wrap it:
//
//	sqlite.Add(Users, sqlite.Text("email").AsPII())

// piiArg is the marker drops uses to flag PII values in args. It
// implements fmt.Formatter so every verb prints the redaction sentinel,
// guarding against a stray log.Printf("%+v", args).
type piiArg struct{ Value any }

// Format implements fmt.Formatter — every verb resolves to "<redacted>".
func (p piiArg) Format(s fmt.State, _ rune) { _, _ = io.WriteString(s, "<redacted>") }

// String implements fmt.Stringer for tools that don't use Format.
func (p piiArg) String() string { return "<redacted>" }

// PII wraps value so it travels through drops as a redaction marker.
func PII(value any) any { return piiArg{Value: value} }

// IsPII returns true when v carries the redaction marker.
func IsPII(v any) bool {
	_, ok := v.(piiArg)
	return ok
}

// PIIParam is a drops.Expression that binds a value already wrapped in
// the redaction marker — used by entity bindings for PII columns.
type PIIParam struct{ Value any }

// WriteSQL implements drops.Expression.
func (p PIIParam) WriteSQL(b *drops.Builder) { b.AddArg(piiArg(p)) }

// AsPII flags the column as carrying PII, so formatters see "<redacted>".
func (c *Col[T]) AsPII() *Col[T] {
	c.Column.pii = true
	return c
}

// IsPII reports whether the column is marked PII.
func (c *Column) IsPII() bool { return c.pii }

// unwrapPII returns a fresh slice with every piiArg marker unwrapped to
// its underlying value. The wrapped slice is kept for hook emission; the
// unwrapped one goes to the driver.
func unwrapPII(args []any) []any {
	out := make([]any, len(args))
	for i, a := range args {
		if p, ok := a.(piiArg); ok {
			out[i] = p.Value
		} else {
			out[i] = a
		}
	}
	return out
}

// containsPII reports whether args includes at least one piiArg marker.
func containsPII(args []any) bool {
	for _, a := range args {
		if _, ok := a.(piiArg); ok {
			return true
		}
	}
	return false
}
