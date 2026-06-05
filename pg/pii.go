package pg

import (
	"fmt"
	"io"

	"github.com/bernardoforcillo/drops"
)

// PII tagging makes the difference between an ORM you can deploy
// in a GDPR jurisdiction and one you can't. The flow is two-step:
//
//  1. Mark sensitive columns so the entity binders know to wrap
//     their values:
//
//	    pg.Add(Users, pg.Text("email").AsPII())
//	    // or via the drop: tag
//	    Email pg.Secret[string] `drop:"email,pii"`
//
//  2. Hooks / loggers / tracers automatically render the wrapped
//     arg as "<redacted>" through fmt.Formatter — no per-call
//     opt-in required. The driver still receives the plain
//     value because db.Exec / db.Query unwrap before passing
//     args downstream.
//
// Combine with pg.Secret[T] for at-rest protection and the
// column is invisible end-to-end: encrypted on disk, redacted in
// logs and traces.

// piiArg is the marker type drops uses to flag PII values in
// args. It implements fmt.Formatter so every verb prints the
// redaction sentinel — protecting against the inevitable
// log.Printf("%+v", args) somebody adds later.
type piiArg struct{ Value any }

// Format implements fmt.Formatter. Every verb resolves to
// "<redacted>" so accidental fmt calls can never leak the
// underlying value.
func (p piiArg) Format(s fmt.State, _ rune) {
	_, _ = io.WriteString(s, "<redacted>")
}

// String implements fmt.Stringer for tools that don't use the
// Format interface (rare, but defensive).
func (p piiArg) String() string { return "<redacted>" }

// PII wraps value so it travels through drops as a redaction
// marker. Use it in raw queries when you don't have an Entity
// column to tag:
//
//	db.Exec(ctx, "UPDATE users SET password = $1 WHERE id = $2",
//	    pg.PII(hashedPw), id)
func PII(value any) any { return piiArg{Value: value} }

// IsPII returns true when v carries the redaction marker.
func IsPII(v any) bool {
	_, ok := v.(piiArg)
	return ok
}

// PIIParam is a drops.Expression that AddArgs a value already
// wrapped in the redaction marker. Used internally by entity
// bindings when the column is flagged PII.
type PIIParam struct{ Value any }

// WriteSQL implements drops.Expression.
func (p PIIParam) WriteSQL(b *drops.Builder) { b.AddArg(piiArg{Value: p.Value}) }

// AsPII flags the column as carrying PII. Entity binders wrap
// values for this column so any logger / tracer formatting them
// sees "<redacted>" instead of the real value. The wire path
// downstream of db.Exec / db.Query is unchanged — the driver
// receives the unwrapped value.
func (c *Col[T]) AsPII() *Col[T] {
	c.Column.pii = true
	return c
}

// IsPII reports whether the column is marked PII.
func (c *Column) IsPII() bool { return c.pii }

// unwrapPII returns a fresh slice with every piiArg marker
// unwrapped to its underlying value. The wrapped slice survives
// for hook emission; the unwrapped one goes to the driver.
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

// containsPII reports whether args includes at least one piiArg
// marker — used as a quick pre-check before allocating the
// unwrapped slice.
func containsPII(args []any) bool {
	for _, a := range args {
		if _, ok := a.(piiArg); ok {
			return true
		}
	}
	return false
}
