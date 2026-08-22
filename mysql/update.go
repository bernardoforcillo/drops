package mysql

import (
	"context"
	"errors"
	"fmt"

	"github.com/bernardoforcillo/drops"
)

// UpdateBuilder composes an UPDATE statement.
type UpdateBuilder struct {
	db       *DB
	table    *Table
	sets     []ColumnValue
	wheres   []drops.Expression
	orderBys []drops.Expression
	limit    *int64
	unscoped bool

	// defaults carries the DefaultFilters of the target table, resolved
	// for one execution — see resolvedDefaults. Set by resolveCtx on the
	// per-execution copy and read by WriteSQL through defaults.of, which
	// falls back to the unresolved list so the ToSQL path renders
	// unchanged.
	defaults resolvedDefaults

	// resolved marks a builder resolveCtx has already produced.
	// Resolution is not idempotent — the target table still has its
	// context filters afterwards, so a second pass appends the tenant
	// predicate a second time and binds its value twice. Nothing fails;
	// the rows come back right and only an argument limit or a query log
	// shows it.
	resolved bool
}

// Set appends column assignments.
//
// Each assignment is restated against the handle this builder's table
// hands out for the column it names. The left-hand side of a SET is
// written bare and never needed it, but a [PatchOp] names its column
// on the right as well — "SET age = age + ?" — and qualified, so an op
// built from another handle on the same table would name a relation
// the UPDATE does not: MySQL answers 1054 whichever of the two
// handles is the odd one out. An assignment naming a column of some
// *other* table is left alone, being a deliberate cross-table
// reference rather than a second handle.
func (u *UpdateBuilder) Set(values ...ColumnValue) *UpdateBuilder {
	for _, v := range values {
		u.sets = append(u.sets, rebindValue(u.table, v))
	}
	return u
}

// SetExpr assigns a raw SQL expression to a column. The expression is
// the caller's and is emitted as given — build it from the same handle
// the statement's table uses.
func (u *UpdateBuilder) SetExpr(col ColRef, e drops.Expression) *UpdateBuilder {
	return u.Set(exprValue{col: col.col(), expr: e})
}

// Where appends predicates joined by AND.
func (u *UpdateBuilder) Where(preds ...drops.Expression) *UpdateBuilder {
	u.wheres = append(u.wheres, preds...)
	return u
}

// OrderBy and Limit bound which rows an UPDATE touches — a MySQL
// extension with no PostgreSQL equivalent, and the safe way to update
// a large table in batches.
func (u *UpdateBuilder) OrderBy(exprs ...drops.Expression) *UpdateBuilder {
	u.orderBys = append(u.orderBys, exprs...)
	return u
}

func (u *UpdateBuilder) Limit(n int64) *UpdateBuilder { u.limit = &n; return u }

// Unscoped opts out of the table's automatic predicates — its
// DefaultFilter list and its ContextFilter list alike. It is how an
// UPDATE that legitimately writes across tenants says so at the call
// site; see [SelectBuilder.Unscoped] for why it is both lists or
// neither.
func (u *UpdateBuilder) Unscoped() *UpdateBuilder { u.unscoped = true; return u }

// ErrNoAssignments is returned when an UPDATE has nothing to set.
var ErrNoAssignments = errors.New("drops/mysql: UPDATE has no assignments")

// WriteSQL renders the UPDATE.
func (u *UpdateBuilder) WriteSQL(b *drops.Builder) {
	b.WriteString("UPDATE ")
	u.table.writeFrom(b)
	b.WriteString(" SET ")
	for i, s := range u.sets {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteIdent(s.column().name)
		b.WriteString(" = ")
		s.writeValue(b)
	}
	wheres := u.wheres
	if dfs := u.autoWheres(); len(dfs) > 0 {
		wheres = append(append([]drops.Expression(nil), dfs...), wheres...)
	}
	if len(wheres) > 0 {
		b.WriteString(" WHERE ")
		writeAnd(b, wheres)
	}
	if len(u.orderBys) > 0 {
		b.WriteString(" ORDER BY ")
		b.AppendList(", ", u.orderBys)
	}
	if u.limit != nil {
		b.WriteString(" LIMIT ")
		b.AddArg(*u.limit)
	}
}

// autoWheres gathers the render-time predicates the statement carries
// on its own account — the DefaultFilters of the target table — ahead
// of the caller's own, so a statement reads scoping first and intent
// second.
//
// The filters are taken through resolvedDefaults.of rather than read
// off the table's own list, which is what walks the statements written
// inside them for the ctx being resolved.
//
// PostgreSQL's twin also gathers the FROM tables' filters, because
// UPDATE … FROM states its join condition in the WHERE clause and an
// unfiltered joined relation there lets another tenant's rows decide
// which of this tenant's rows get written. MySQL spells that
// UPDATE t JOIN u ON …, which this builder does not expose: an UPDATE
// here names exactly one relation. If it ever gains a join, its tables
// belong in this list and in resolveCtx's context-filter resolution,
// and the ON-versus-WHERE reasoning in joinKind.filterPlacement applies
// there too.
func (u *UpdateBuilder) autoWheres() []drops.Expression {
	if u.unscoped {
		return nil
	}
	return u.defaults.of(u.table)
}

// checkAxisAssignment refuses an UPDATE whose SET list assigns the
// tenant axis to anything but the tenant the ctx already names.
//
// This is the raw builder's half of section 2 of the normative policy
// block in tenant.go — "the tenant column is an axis, never an
// assignment" — and it was the half that did not exist. Every other
// write path had it: Create and Update stamp the axis from ctx and
// refuse a struct naming another tenant, Patch refuses an op naming
// the axis at all, the INSERT stamps every row and the ON DUPLICATE
// KEY UPDATE branch gates its assignments. The builder the readme
// shows alongside them rendered
//
//	UPDATE `posts` SET `tenantId` = ? WHERE `id` = ? AND `tenantId` = ?
//	args: [999 7 77]
//
// which is word for word the shape the policy block describes: the
// WHERE clause is correctly confined to the caller's own rows, the SET
// list gives one of them away, and the half a review checks is the
// correct one. It needs no foreign handle and no unusual import —
// db.Update(tbl).Set(TenantCol.Val(999)), the table's own handle and
// the obvious spelling. In this dialect the predicates are the whole
// of the boundary, so nothing underneath refuses it either.
//
// What is refused, and what is not:
//
//   - An assignment binding the tenant the ctx already carries is a
//     restatement of where the row already is, and renders. That is
//     not an exception carved out for a caller who asks nicely:
//     [Entity.Update] writes every mapped column of the row, the axis
//     among them, having stamped it from ctx one call earlier — so the
//     rule the raw builder enforces is the one the entity path obeys,
//     rather than a rule the entity path is exempt from. The value is
//     compared with [sameTenant], the definition section 1 gives.
//   - Anything else naming the axis is [ErrTenantMismatch]: another
//     tenant's value, and equally an expression this cannot read — a
//     scalar subquery, `tenantId` + 1, and the arithmetic a [PatchOp]
//     builds. What such an expression evaluates to is the server's
//     answer and not one a check on the way out can have, and a
//     transfer written as arithmetic is still a transfer.
//   - A ctx with no tenant is [ErrTenantMissing]. On a table whose
//     read scoping would refuse the statement anyway this is the same
//     answer one clause earlier; on a table that named a write axis
//     with [Table.ScopeWritesByTenant] and scopes its reads some other
//     way, it is the only one.
//
// [UpdateBuilder.Unscoped] is the opt-out, and it is deliberate that
// there is one. The raw builder is the documented escape hatch, and
// the statements that legitimately move a row between tenants — a
// migration, a merge of two accounts, an admin tool — have to be
// writable in this package rather than in hand-written SQL beside it.
// Unscoped already says "this statement's authority is not the ctx
// tenant's" for the WHERE clause; saying it for the SET list too keeps
// one flag meaning one thing, at the call site where a reviewer reads
// it. That is [InsertBuilder.Unscoped]'s precedent, which likewise
// turns off the stamping and the requirement together.
//
// Patch refuses the axis outright, including an op assigning the ctx
// tenant's own value, and the difference from this is not a drift. A
// patch op list is built out of the fields a request named and never
// touches the row's struct, so nothing there is ever the stamp's own
// output; a SET list is also what Entity.Update composes.
//
// It runs from resolveCtx, so it is the ctx paths — Exec and ToSQLCtx
// — that refuse. [UpdateBuilder.ToSQL] renders without a ctx and has
// no error to return, which is the same boundary the INSERT's stamping
// has and the same one ToSQL already states about the context filters.
func checkAxisAssignment(ctx context.Context, axis *Column, sets []ColumnValue) error {
	if axis == nil {
		return nil
	}
	for _, s := range sets {
		if !namesAxis(s.column(), axis) {
			continue
		}
		t, ok := TenantFrom(ctx)
		if !ok {
			return fmt.Errorf("%w: %s", ErrTenantMissing, columnPath(axis))
		}
		bound, kind := classifyBinding(s.valueExpr())
		if kind == bindingLiteral && sameTenant(bound, t) {
			continue
		}
		if kind == bindingLiteral {
			return fmt.Errorf("%w: %s is an axis, not an assignment: this UPDATE assigns another tenant's value",
				ErrTenantMismatch, columnPath(axis))
		}
		return fmt.Errorf("%w: %s is an axis, not an assignment: this UPDATE assigns an expression drops cannot compare with the ctx tenant; drop the assignment, or say Unscoped",
			ErrTenantMismatch, columnPath(axis))
	}
	return nil
}

// ToSQL renders the statement and its arguments.
//
// A table's context filters are resolved by the executors and do not
// appear here — see [SelectBuilder.ToSQL] for why, and use
// [UpdateBuilder.ToSQLCtx] for the statement a given ctx would send.
func (u *UpdateBuilder) ToSQL() (string, []any) { return render(u) }

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
// when there was nothing to resolve, otherwise a shallow copy carrying
// the resolved predicates and the resolved subqueries.
//
// The copy is what keeps a builder executable twice; see
// [SelectBuilder.resolveCtx].
//
// The SET list is walked as well as the WHERE clause, and it is the
// half that decides what gets written rather than which rows do:
// SET `ownerId` = (SELECT …) reads as much as any SELECT, and a
// [PatchOp] or a [SetExpr] is the ordinary way to write one.
func (u *UpdateBuilder) resolveCtx(ctx context.Context) (*UpdateBuilder, error) {
	if u.resolved {
		return u, nil
	}
	cp := *u
	changed := false

	lists := []struct {
		src []drops.Expression
		dst *[]drops.Expression
	}{
		{u.wheres, &cp.wheres},
		{u.orderBys, &cp.orderBys},
	}
	for _, l := range lists {
		resolved, err := resolveExprs(ctx, l.src)
		if err != nil {
			return nil, err
		}
		if resolved != nil {
			*l.dst, changed = resolved, true
		}
	}

	resolvedSets, err := resolveSets(ctx, u.sets)
	if err != nil {
		return nil, err
	}
	if resolvedSets != nil {
		cp.sets, changed = resolvedSets, true
	}

	if !u.unscoped {
		// The SET list first, before any of the scoping is resolved: a
		// statement that assigns the axis is refused whatever its WHERE
		// clause would have carried, and cp.sets is by now the list the
		// statement will render.
		if err := checkAxisAssignment(ctx, u.table.tenantAxis(), cp.sets); err != nil {
			return nil, err
		}
		defaults, err := resolveTableDefaults(ctx, u.table)
		if err != nil {
			return nil, err
		}
		if defaults != nil {
			cp.defaults, changed = defaults, true
		}
		preds, err := u.table.resolveContextFilters(ctx)
		if err != nil {
			return nil, err
		}
		if len(preds) > 0 {
			// After the caller's own predicates, so the rendered clause
			// reads defaults, intent, scoping — the order a SELECT
			// renders in too.
			cp.wheres = append(append([]drops.Expression(nil), cp.wheres...), preds...)
			changed = true
		}
	}

	if !changed {
		return u, nil
	}
	cp.resolved = true
	return &cp, nil
}

// resolveStatement implements [ctxResolvable]: it is resolveCtx behind
// the interface resolveExpr dispatches on, so an UPDATE written as a
// CTE body or a subquery operand is resolved as the statement it is
// rather than rendered blind.
func (u *UpdateBuilder) resolveStatement(ctx context.Context) (drops.Expression, bool, error) {
	r, err := u.resolveCtx(ctx)
	if err != nil {
		return nil, false, err
	}
	return r, r != u, nil
}

// Exec runs the UPDATE.
func (u *UpdateBuilder) Exec(ctx context.Context) (drops.Result, error) {
	if len(u.sets) == 0 {
		return nil, ErrNoAssignments
	}
	sql, args, err := u.ToSQLCtx(ctx)
	if err != nil {
		return nil, err
	}
	return u.db.Exec(ctx, sql, args...)
}
