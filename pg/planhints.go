package pg

import (
	"context"
	"strconv"
	"strings"
)

// Plan hints — the answer to "the planner picks the wrong index on
// this one query", which every service hits and no Go toolkit
// addresses.
//
// PostgreSQL has no hint syntax. The pg_hint_plan extension adds one,
// and it reads hints from a block comment at the *front* of the
// statement:
//
//	/*+ IndexScan(orders orders_customer_created_idx) */
//	SELECT ... FROM orders WHERE ...
//
// drops already has the machinery for that: [drops.WithQueryTags]
// puts a SQLCommenter comment on every statement from the same
// plumbing. The tag comment goes at the end, deliberately — MySQL and
// MariaDB execute a leading /*! or /*+ comment, so a toolkit that
// serves five dialects cannot put anything at the front by default.
// Hints are a second, PostgreSQL-only path with the opposite
// placement, and that is the whole reason this is a separate file.
//
//	ctx = pg.WithPlanHints(ctx, pg.IndexScan("orders", "orders_customer_created_idx"))
//	rows, err := Orders.Query(db, ctx, ...)
//
// # Scope the context to one query
//
// The hints live on the context, so they apply to every statement
// issued with it. pg_hint_plan ignores a hint naming a relation the
// query does not contain, so a stray hint is usually harmless — but
// "usually harmless" is not a contract, and a hint that silently
// applies to the wrong statement is worse than no hint. Build the
// context immediately before the call it is for, and do not pass it
// down:
//
//	hinted := pg.WithPlanHints(ctx, pg.NestLoop("a", "b"))
//	rows, err := db.Query(hinted, sql, args...)   // this one, nothing else
//
// # This is not free advice
//
// A hint freezes a decision the planner would otherwise revisit as
// the data changes. It is the right tool when you have established
// that the planner is wrong *and why* — usually a bad row estimate
// across a correlated predicate — and the wrong tool as a first
// response to a slow query. Pair it with [Explain]: capture the plan
// fingerprint with the hint and assert it in a test, or the hint will
// outlive the problem it was written for and nobody will know.
//
// The extension must be installed and loaded
// (shared_preload_libraries, or LOAD 'pg_hint_plan'). Without it the
// comment is a comment, the query runs unhinted, and nothing reports
// that the hint did nothing — which is the failure mode the [Explain]
// assertion catches.

// Hint is one pg_hint_plan directive. Build them with the
// constructors below; the zero Hint renders nothing.
type Hint struct {
	// name is the directive, e.g. "IndexScan".
	name string
	// args are the already-validated, already-quoted arguments.
	args []string
	// ok is false when a constructor was handed something that
	// cannot be written into a comment safely. Such a hint is
	// dropped at render time — see [HintsValid].
	ok bool
}

// String renders the hint as pg_hint_plan spells it, e.g.
// `IndexScan(orders orders_pkey)`. An invalid hint renders as "".
func (h Hint) String() string {
	if !h.ok || h.name == "" {
		return ""
	}
	if len(h.args) == 0 {
		return h.name + "()"
	}
	return h.name + "(" + strings.Join(h.args, " ") + ")"
}

// Valid reports whether the hint can be written into a comment. A
// hint is invalid when an identifier handed to its constructor
// contains something that would end the comment early or cannot be
// quoted — see [HintsValid] for why this is checkable rather than an
// error return.
func (h Hint) Valid() bool { return h.ok }

// HintsValid reports whether every hint in hints is renderable, and
// returns the first one that is not.
//
// Hints are dropped rather than raised as errors because they are
// advice: a query that loses its hint runs, more slowly, and a query
// that gains a broken comment does not run at all. Fail-closed is the
// only defensible default in the statement path.
//
// That makes the mistake silent, so it is worth an assertion at the
// point the hints are written — a test, or a check at start-up:
//
//	if bad, ok := pg.HintsValid(hints); !ok {
//	    log.Fatalf("unrenderable plan hint: %q", bad)
//	}
func HintsValid(hints []Hint) (Hint, bool) {
	for _, h := range hints {
		if !h.ok {
			return h, false
		}
	}
	return Hint{}, true
}

// --- Scan method hints ----------------------------------------------

// SeqScan forces a sequential scan on table.
func SeqScan(table string) Hint { return relHint("SeqScan", table) }

// NoSeqScan forbids a sequential scan on table. Usually the better
// half of the pair: it tells the planner what not to do and leaves it
// free to choose among the rest, so it survives a new index where
// [IndexScan] naming an old one does not.
func NoSeqScan(table string) Hint { return relHint("NoSeqScan", table) }

// TidScan forces a TID scan on table.
func TidScan(table string) Hint { return relHint("TidScan", table) }

// NoTidScan forbids a TID scan on table.
func NoTidScan(table string) Hint { return relHint("NoTidScan", table) }

// IndexScan forces an index scan on table, restricted to the named
// indexes when any are given.
//
// Name the index and you have pinned the plan to an object a later
// migration can rename or drop, at which point the hint silently
// stops applying. [NoSeqScan] expresses the same intent without the
// dependency; prefer it unless the choice *between* indexes is the
// problem.
func IndexScan(table string, indexes ...string) Hint {
	return relHint("IndexScan", table, indexes...)
}

// NoIndexScan forbids an index scan on table.
func NoIndexScan(table string) Hint { return relHint("NoIndexScan", table) }

// IndexOnlyScan forces an index-only scan on table, restricted to the
// named indexes when any are given. PostgreSQL falls back to a plain
// index scan when an index-only scan is not available.
func IndexOnlyScan(table string, indexes ...string) Hint {
	return relHint("IndexOnlyScan", table, indexes...)
}

// NoIndexOnlyScan forbids an index-only scan on table.
func NoIndexOnlyScan(table string) Hint { return relHint("NoIndexOnlyScan", table) }

// BitmapScan forces a bitmap scan on table, restricted to the named
// indexes when any are given.
func BitmapScan(table string, indexes ...string) Hint {
	return relHint("BitmapScan", table, indexes...)
}

// NoBitmapScan forbids a bitmap scan on table.
func NoBitmapScan(table string) Hint { return relHint("NoBitmapScan", table) }

// --- Join method hints ----------------------------------------------

// NestLoop forces a nested-loop join across the named tables.
func NestLoop(tables ...string) Hint { return joinHint("NestLoop", tables) }

// NoNestLoop forbids a nested-loop join across the named tables.
func NoNestLoop(tables ...string) Hint { return joinHint("NoNestLoop", tables) }

// HashJoin forces a hash join across the named tables.
func HashJoin(tables ...string) Hint { return joinHint("HashJoin", tables) }

// NoHashJoin forbids a hash join across the named tables.
func NoHashJoin(tables ...string) Hint { return joinHint("NoHashJoin", tables) }

// MergeJoin forces a merge join across the named tables.
func MergeJoin(tables ...string) Hint { return joinHint("MergeJoin", tables) }

// NoMergeJoin forbids a merge join across the named tables.
func NoMergeJoin(tables ...string) Hint { return joinHint("NoMergeJoin", tables) }

// --- Join order -----------------------------------------------------

// Leading fixes the join order to the given sequence of tables.
//
// This is the heaviest hint in the set: it removes the planner's
// search entirely, so a table added to the query later joins in a
// position nobody chose. Reach for it when a bad estimate at the
// bottom of the tree is poisoning everything above it, which is the
// case a per-relation hint cannot fix.
func Leading(tables ...string) Hint { return joinHint("Leading", tables) }

// --- Row-count correction -------------------------------------------

// RowsAbsolute replaces the planner's row estimate for the join of
// the named tables with n.
//
// It addresses the actual cause of most bad plans — a wrong estimate
// rather than a wrong choice given the estimate — and leaves the
// planner free to decide everything else. When it works, it keeps
// working as the data changes in a way that [Leading] does not.
func RowsAbsolute(n int64, tables ...string) Hint {
	return rowsHint("#"+strconv.FormatInt(n, 10), tables)
}

// RowsMultiply scales the planner's row estimate for the join of the
// named tables by factor, e.g. 10 for "the planner is out by an order
// of magnitude here".
func RowsMultiply(factor float64, tables ...string) Hint {
	return rowsHint("*"+strconv.FormatFloat(factor, 'g', -1, 64), tables)
}

// --- Parallelism ----------------------------------------------------

// Parallel sets the number of parallel workers for table. Pass
// workers = 0 to disable parallelism for it.
//
// When hard is true the hint overrides the planner's own limits
// (pg_hint_plan's "hard" mode); when false it only adjusts what the
// planner would consider.
func Parallel(table string, workers int, hard bool) Hint {
	rel, ok := hintIdent(table)
	if !ok || workers < 0 {
		return Hint{name: "Parallel"}
	}
	mode := "soft"
	if hard {
		mode = "hard"
	}
	return Hint{name: "Parallel", args: []string{rel, strconv.Itoa(workers), mode}, ok: true}
}

// --- GUC ------------------------------------------------------------

// SetGUC applies a planner GUC for the duration of the statement, e.g.
// SetGUC("enable_seqscan", "off") or SetGUC("work_mem", "256MB").
//
// pg_hint_plan spells the directive "Set"; the constructor carries
// the suffix because [Set] is already an UPDATE assignment in this
// package, and a query builder's Set is the one a reader expects.
//
// It is the escape hatch for the knob that has no hint of its own,
// and it is scoped to the one statement — which is what makes it
// safer than the SET LOCAL a caller would otherwise reach for, since
// that one outlives the statement and takes the rest of the
// transaction with it.
func SetGUC(param, value string) Hint {
	p, okp := hintIdent(param)
	v, okv := hintIdent(value)
	if !okp || !okv {
		return Hint{name: "Set"}
	}
	return Hint{name: "Set", args: []string{p, v}, ok: true}
}

// --- Construction helpers -------------------------------------------

// relHint builds a scan hint: one relation, then optional indexes.
func relHint(name, table string, indexes ...string) Hint {
	rel, ok := hintIdent(table)
	if !ok {
		return Hint{name: name}
	}
	args := make([]string, 0, 1+len(indexes))
	args = append(args, rel)
	for _, idx := range indexes {
		q, ok := hintIdent(idx)
		if !ok {
			return Hint{name: name}
		}
		args = append(args, q)
	}
	return Hint{name: name, args: args, ok: true}
}

// joinHint builds a hint over a list of relations. A join hint needs
// at least two of them; pg_hint_plan ignores one with fewer, and
// rendering it anyway would put a directive in the comment that does
// nothing and reports nothing.
func joinHint(name string, tables []string) Hint {
	if len(tables) < 2 {
		return Hint{name: name}
	}
	args := make([]string, 0, len(tables))
	for _, t := range tables {
		q, ok := hintIdent(t)
		if !ok {
			return Hint{name: name}
		}
		args = append(args, q)
	}
	return Hint{name: name, args: args, ok: true}
}

// rowsHint builds a Rows(...) correction. The correction term is
// generated here, never supplied by the caller, so it needs no
// validation of its own.
func rowsHint(correction string, tables []string) Hint {
	if len(tables) < 2 {
		return Hint{name: "Rows"}
	}
	args := make([]string, 0, len(tables)+1)
	for _, t := range tables {
		q, ok := hintIdent(t)
		if !ok {
			return Hint{name: "Rows"}
		}
		args = append(args, q)
	}
	return Hint{name: "Rows", args: append(args, correction), ok: true}
}

// hintIdent renders s as an identifier safe to place inside a block
// comment, quoting it when it is not already a bare lowercase
// identifier.
//
// Two things make this stricter than [quoteIdent]. A comment ends at
// the first "*/", and PostgreSQL nests block comments, so an embedded
// "/*" changes where the comment ends too — neither can be escaped
// away inside a quoted identifier the way a double quote can, so a
// name containing either is refused outright. And pg_hint_plan parses
// the comment body itself, so a newline or a NUL in a name would end
// the hint rather than appear in it.
func hintIdent(s string) (string, bool) {
	if s == "" {
		return "", false
	}
	if strings.Contains(s, "*/") || strings.Contains(s, "/*") {
		return "", false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] == 0x7f {
			return "", false
		}
	}
	if isBareHintIdent(s) {
		return s, true
	}
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`, true
}

// isBareHintIdent reports whether s can be written without quotes:
// the lowercase-identifier shape PostgreSQL would fold to anyway.
func isBareHintIdent(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c == '_':
		case c >= '0' && c <= '9':
			if i == 0 {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// --- Context plumbing -----------------------------------------------

type planHintKey struct{}

// WithPlanHints returns a context carrying hints for the statements
// issued with it. Calling it again replaces the previous set rather
// than adding to it: pg_hint_plan reads one comment per statement, so
// two sets cannot both apply and the later call is the one the caller
// meant.
//
// Passing no hints clears them, which is how a context that was
// handed down can opt a sub-query out.
func WithPlanHints(ctx context.Context, hints ...Hint) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if len(hints) == 0 {
		return context.WithValue(ctx, planHintKey{}, []Hint(nil))
	}
	cp := make([]Hint, len(hints))
	copy(cp, hints)
	return context.WithValue(ctx, planHintKey{}, cp)
}

// PlanHints returns the hints on ctx, or nil.
func PlanHints(ctx context.Context) []Hint {
	if ctx == nil {
		return nil
	}
	hints, _ := ctx.Value(planHintKey{}).([]Hint)
	return hints
}

// RenderPlanHints returns the `/*+ ... */` comment for hints, or ""
// when none of them is renderable. Exported so a test can assert the
// exact comment a set of hints produces without a database.
func RenderPlanHints(hints []Hint) string {
	if len(hints) == 0 {
		return ""
	}
	var b strings.Builder
	b.Grow(len(hints)*24 + 6)
	b.WriteString("/*+")
	n := 0
	for _, h := range hints {
		s := h.String()
		if s == "" {
			continue
		}
		b.WriteByte(' ')
		b.WriteString(s)
		n++
	}
	if n == 0 {
		return ""
	}
	b.WriteString(" */")
	return b.String()
}

// hintStatement returns sql with the hints on ctx prepended as a
// pg_hint_plan comment.
//
// It runs in [DB.Exec] and [DB.Query], next to [drops.TagStatement]
// and after it — the tag goes on the end, the hint goes on the front,
// and doing the hint second means the statement the span and the hook
// report is the one the server receives, comment and all.
func hintStatement(ctx context.Context, sql string) string {
	if ctx == nil {
		return sql
	}
	hints, _ := ctx.Value(planHintKey{}).([]Hint)
	if len(hints) == 0 {
		return sql
	}
	comment := RenderPlanHints(hints)
	if comment == "" {
		return sql
	}
	return comment + "\n" + sql
}
