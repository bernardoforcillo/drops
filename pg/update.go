package pg

import (
	"context"

	"github.com/bernardoforcillo/drops"
)

// UpdateBuilder composes an UPDATE statement.
type UpdateBuilder struct {
	db        *DB
	table     *Table
	sets      []ColumnValue
	from      []*Table
	wheres    []drops.Expression
	returning []drops.Expression
	unscoped  bool
}

// Set adds one or more assignments. Use (*Col[T]).Val(v) to bind a typed
// value or (*Col[T]).Expr(e) to bind an expression.
func (u *UpdateBuilder) Set(values ...ColumnValue) *UpdateBuilder {
	u.sets = append(u.sets, values...)
	return u
}

// From adds tables to a PostgreSQL UPDATE ... FROM clause for joins.
//
// Each table is joined as a scoped table, not as a bare relation name:
// its DefaultFilters and its ContextFilters are carried by the
// statement like the target table's own. There is no ON clause to
// choose between here — UPDATE ... FROM states its join condition in
// the WHERE clause — so they go into the WHERE clause with everything
// else, and none of the placement reasoning a SELECT's outer joins
// need applies.
//
// Carrying them is what stops the join from deciding the write. While
// the FROM tables went in unfiltered, "UPDATE accounts SET ... FROM
// posts WHERE accounts.id = posts.accountId" matched against every
// tenant's posts and every soft-deleted one, so whose rows got written
// depended on another tenant's data. Say Unscoped() to opt the whole
// statement out.
func (u *UpdateBuilder) From(tables ...*Table) *UpdateBuilder {
	u.from = append(u.from, tables...)
	return u
}

// Where appends predicates joined by AND.
func (u *UpdateBuilder) Where(preds ...drops.Expression) *UpdateBuilder {
	u.wheres = append(u.wheres, preds...)
	return u
}

// Returning sets a RETURNING clause.
func (u *UpdateBuilder) Returning(cols ...drops.Expression) *UpdateBuilder {
	u.returning = append(u.returning, cols...)
	return u
}

// Unscoped opts out of the automatic predicates for this UPDATE — both
// the DefaultFilter list and the ContextFilter list, of the target
// table and of every table named in From alike. Use when an
// administrative job must bypass a soft-delete guard, or write across
// every tenant.
//
// It is statement-wide rather than per table for the reason
// [SelectBuilder.Unscoped] is: a flag that unscoped the target while a
// FROM table kept its tenant axis would answer the administrative
// UPDATE by writing a silently narrowed set of rows — neither the
// caller's intent nor the safe refusal, and invisible in the row count.
//
// On an UPDATE the widening is the dangerous direction: an unscoped
// statement does not read another tenant's rows, it writes them. Prefer
// restating the scope you meant — Unscoped().Where(...) — over dropping
// it wholesale.
func (u *UpdateBuilder) Unscoped() *UpdateBuilder {
	u.unscoped = true
	return u
}

// WriteSQL renders the UPDATE.
func (u *UpdateBuilder) WriteSQL(b *drops.Builder) {
	sets := u.sets
	if u.table.hasUpdateHooks() {
		sets = u.applyUpdateHooks()
	}
	wheres := u.wheres
	if !u.unscoped {
		if auto := u.autoWheres(); len(auto) > 0 {
			wheres = append(auto, wheres...)
		}
	}
	b.WriteString("UPDATE ")
	u.table.writeFrom(b)
	b.WriteString(" SET ")
	for j, s := range sets {
		if j > 0 {
			b.WriteString(", ")
		}
		b.WriteIdent(s.column().Name())
		b.WriteString(" = ")
		s.writeValue(b)
	}
	if len(u.from) > 0 {
		b.WriteString(" FROM ")
		for j, t := range u.from {
			if j > 0 {
				b.WriteString(", ")
			}
			t.writeFrom(b)
		}
	}
	if len(wheres) > 0 {
		b.WriteString(" WHERE ")
		writeAnd(b, wheres)
	}
	if len(u.returning) > 0 {
		b.WriteString(" RETURNING ")
		b.AppendList(", ", u.returning)
	}
}

// autoWheres gathers the render-time predicates the statement carries
// on its own account — the DefaultFilters of the target table and of
// every table named in the FROM clause — in the order they are written,
// ahead of the caller's own. So a statement reads scoping first, intent
// second, and a query log shows at a glance whether a write was scoped
// at all.
//
// Two things about it are load-bearing.
//
// The filters are taken through Table.resolveDefaultFilters rather than
// read off Table.defaultFilters, which is what restates them against
// the instance of the table the statement actually names. A filter is
// an opaque tree of closures over the declared column handles, so
// against UPDATE "notes" AS "n" a raw read renders "notes"."deletedAt"
// — a relation the statement has no FROM entry for, and PostgreSQL
// answers 42P01. That is not a widened write, it is a statement that
// cannot run, which made the tables that most need their scoping the
// ones that could not be written under an alias at all.
//
// The FROM tables are included because UPDATE ... FROM puts its join
// condition in the WHERE clause: there is no ON clause here and so
// none of the placement reasoning joinKind.filterPlacement has to do
// for SELECT. An unfiltered FROM table therefore does not merely widen
// a result — it lets another tenant's rows, or rows a soft delete
// retired, decide which of this tenant's rows get written. Scoping the
// target table alone answered that with a statement that looked
// scoped.
func (u *UpdateBuilder) autoWheres() []drops.Expression {
	var out []drops.Expression
	out = append(out, u.table.resolveDefaultFilters()...)
	for _, t := range u.from {
		out = append(out, t.resolveDefaultFilters()...)
	}
	return out
}

// applyUpdateHooks runs every UpdateHook registered on the table and
// returns the (possibly extended) SET list.
func (u *UpdateBuilder) applyUpdateHooks() []ColumnValue {
	ctx := &UpdateHookCtx{bound: make(map[*Column]bool, len(u.sets))}
	for _, s := range u.sets {
		ctx.bound[s.column().key()] = true
	}
	for _, h := range u.table.updateHooks {
		h.BeforeUpdate(ctx)
	}
	if len(ctx.add) == 0 {
		return u.sets
	}
	out := append([]ColumnValue(nil), u.sets...)
	out = append(out, ctx.add...)
	return out
}

// ToSQL renders the statement.
//
// A table's context filters are resolved by the executors and do not
// appear here — see [SelectBuilder.ToSQL] for why, and use
// [UpdateBuilder.ToSQLCtx] for the statement a given ctx would send.
func (u *UpdateBuilder) ToSQL() (sql string, args []any) {
	b := drops.NewBuilder()
	u.WriteSQL(b)
	return b.SQL()
}

// ToSQLCtx renders the complete statement for ctx, with every context
// filter on the target table resolved into the WHERE clause.
func (u *UpdateBuilder) ToSQLCtx(ctx context.Context) (sql string, args []any, err error) {
	r, err := u.resolveCtx(ctx)
	if err != nil {
		return "", nil, err
	}
	sql, args = r.ToSQL()
	return sql, args, nil
}

// resolveCtx returns the builder to render for one execution — this one
// when no table the statement names has a context filter, otherwise a
// shallow copy whose WHERE list carries the resolved predicates. The
// copy is what keeps the same builder executable twice without
// accumulating a predicate per run; see [SelectBuilder.resolveCtx].
//
// Every table the statement names is resolved, the FROM tables
// included, because a context filter is a property of the table and the
// promise in tenant.go is about statements rather than about clauses.
// The FROM table of an UPDATE ... FROM is joined through the WHERE
// clause, so leaving its tenant axis off does not narrow a result set —
// it lets another tenant's rows select which of this tenant's rows are
// written, which is the read defect one grade worse.
//
// A filter that refuses — [TenantFilter] with no tenant on ctx — aborts
// the whole resolution, for the FROM tables exactly as for the target.
// A write that cannot say which tenant's rows it is joining must not be
// sent unfiltered.
func (u *UpdateBuilder) resolveCtx(ctx context.Context) (*UpdateBuilder, error) {
	if u.unscoped {
		return u, nil
	}
	var preds []drops.Expression
	tp, err := u.table.resolveContextFilters(ctx)
	if err != nil {
		return nil, err
	}
	preds = append(preds, tp...)
	for _, t := range u.from {
		fp, err := t.resolveContextFilters(ctx)
		if err != nil {
			return nil, err
		}
		preds = append(preds, fp...)
	}
	if len(preds) == 0 {
		return u, nil
	}
	cp := *u
	cp.wheres = append(append([]drops.Expression(nil), u.wheres...), preds...)
	return &cp, nil
}

// Exec runs the UPDATE.
func (u *UpdateBuilder) Exec(ctx context.Context) (drops.Result, error) {
	if len(u.sets) == 0 && !u.table.hasUpdateHooks() {
		return nil, ErrNoUpdateAssignments
	}
	sql, args, err := u.ToSQLCtx(ctx)
	if err != nil {
		return nil, err
	}
	return u.db.Exec(ctx, sql, args...)
}

// All executes the UPDATE and scans the RETURNING rows into dest.
func (u *UpdateBuilder) All(ctx context.Context, dest any) error {
	if len(u.returning) == 0 {
		return ErrReturningRequired
	}
	sql, args, err := u.ToSQLCtx(ctx)
	if err != nil {
		return err
	}
	rows, err := u.db.Query(ctx, sql, args...)
	if err != nil {
		return err
	}
	return scanAll(rows, dest)
}

// One executes the UPDATE and scans the first RETURNING row into dest.
func (u *UpdateBuilder) One(ctx context.Context, dest any) error {
	if len(u.returning) == 0 {
		return ErrReturningRequired
	}
	sql, args, err := u.ToSQLCtx(ctx)
	if err != nil {
		return err
	}
	rows, err := u.db.Query(ctx, sql, args...)
	if err != nil {
		return err
	}
	return scanOne(rows, dest)
}
