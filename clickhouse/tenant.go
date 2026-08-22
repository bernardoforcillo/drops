package clickhouse

import (
	"context"
	"errors"
	"fmt"
	"reflect"

	"github.com/bernardoforcillo/drops"
)

// Multi-tenant analytics without explicit data isolation is a leak
// waiting to happen — one forgotten WHERE tenantId = ? and rows cross
// customers. [Table.ContextFilter] with [TenantFilter], plus
// [WithTenant], make the isolation a property of the TABLE rather than
// of the call site: every statement that reads or writes it takes the
// tenant from ctx and carries the predicate. Forgetting to set the ctx
// errors out — the bad code path fails closed, not open.
//
// "Of the table" is load-bearing. Declared on the table, the axis is
// applied by whichever executor runs the statement, which is the same
// list for a root query, a joined table, a CTE body, a subquery
// operand and an INSERT. A predicate injected by one entry point
// instead reaches the statements somebody remembered to build with it,
// and this dialect has more entry points than its size suggests:
// db.Select().From(Events) is one, Entity.Query is another,
// VectorStore.Search builds a third, and each of the analytics helpers
// composes a SELECT of its own.
//
// The axis is per table, because the predicate names a column: an
// entity on `events` cannot say what a row of `sessions` belongs to. A
// schema is scoped when each tenant-owning table declares its own axis
// — through its entity, or directly with
// Events.ContextFilter(clickhouse.TenantFilter(EventTenantID)).
//
//	var Events = clickhouse.NewEntity[Event](events).
//	    ScopeByTenant(events.Col("tenantId"))
//
//	ctx = clickhouse.WithTenant(ctx, currentTenant)
//	rows, err := Events.Query(db).Where(…).All(ctx)
//	// SELECT … WHERE … AND ("events"."tenantId" = ?)
//
// Create stamps the tenant on the row automatically before insert (or
// rejects a row that already carries a different one), so a stray
// background job cannot silently insert into the wrong tenant.
//
// # What is different because this is ClickHouse
//
// Three things, and none of them is cosmetic.
//
// There is no UPDATE and no DELETE. A mutation is an
// ALTER TABLE … UPDATE/DELETE, asynchronous and not transactional, and
// this package does not model one — so the predicate has three
// destinations here rather than five, and the write-side half of the
// axis is stamping alone. What PostgreSQL guards with a gated
// ON CONFLICT DO UPDATE has no statement to attach to here, and the
// equivalent hazard has moved into the ENGINE: see
// [ErrTenantNotInSortingKey].
//
// There are two predicate clauses. A filter written into PREWHERE
// rather than WHERE is evaluated before the FINAL merge, which changes
// which row version the guard is tested against — see
// [SelectBuilder.Prewhere]. Nothing drops adds automatically goes into
// PREWHERE.
//
// A table may be Distributed or Replicated, in which case the predicate
// is evaluated on each shard. That is what makes it work rather than a
// problem: the guard travels with the query. The shape to watch is a
// filter whose predicate embeds a subquery over a Distributed table,
// because ClickHouse evaluates a plain IN (subquery) per shard against
// that shard's local data. Write such a filter with GLOBAL IN — drops
// cannot know which of your tables is Distributed and will not guess.
//
// # Where the automatic scoping stops
//
// These predicates are not an isolation boundary the way a server-side
// policy is, and ClickHouse has no equivalent of PostgreSQL row-level
// security to put underneath them. So the list below is not a footnote,
// it is the whole of what is left when the predicates do not reach:
//
//   - a raw statement, through [DB.Exec] or [DB.Query], carries what
//     the caller wrote and nothing else;
//   - a [drops.Raw] fragment, or a caller's own [drops.ExprFunc], is
//     text drops never parses — including one used as a subquery body,
//     a CTE body, or a filter's predicate;
//   - a materialised view: ClickHouse evaluates the SELECT the
//     CREATE MATERIALIZED VIEW stored, on INSERT into the source table,
//     with no request and no ctx anywhere near it. A view over a scoped
//     table carries whatever its stored body carries;
//   - a statement said Unscoped(), which is the point of the method;
//   - an INSERT into a table that declared a read filter and no write
//     column — see [Table.ScopeWritesByTenant] for why drops will not
//     guess the column;
//   - a scoped table INNER- or LEFT-joined BEFORE a RIGHT JOIN keeps its
//     guard in the WHERE clause, and the RIGHT JOIN NULL-extends the left
//     side — so the guard is false for exactly the rows the RIGHT JOIN
//     exists to preserve. fromFilterJoin moves the FROM table's guard
//     into the first RIGHT JOIN's ON clause; a table joined at position i
//     takes its placement from its own join kind alone, and nothing looks
//     at the kinds after it. This LOSES rows rather than leaking them —
//     fail-closed, which is why nine rounds of adversarial review did not
//     surface it — and the fix is to make filterPlacement consult the
//     join kinds after position i. It is written down rather than fixed
//     because moving where a guard lands is a change to the mechanism
//     this section describes, and it belongs in a round that can verify
//     it in its own right.
//
// A reviewer who has not read that list will read a raw fragment or a
// materialised view as scoped when it is not.

type tenantCtxKey int

const tenantKey tenantCtxKey = 1

// WithTenant returns a context carrying tenant. Pass anything the driver
// can bind — a string id, uint64, UUID string, etc.
func WithTenant(ctx context.Context, tenant any) context.Context {
	return context.WithValue(ctx, tenantKey, tenant)
}

// TenantFrom returns the tenant on ctx (ok=false when absent).
func TenantFrom(ctx context.Context) (any, bool) {
	v := ctx.Value(tenantKey)
	return v, v != nil
}

// ErrTenantMissing is returned when a statement reads or writes a table
// that is scoped by tenant and the ctx carries no tenant. Surfacing
// this rather than silently running a cross-tenant query is the whole
// point of the feature.
//
// It names the table rather than the entity, and the difference is not
// pedantry: the most surprising producer is a bare
// db.Select().From(Events) with no Entity anywhere in the call, and
// "entity is tenant-scoped" would send that caller looking for an
// entity they never wrote.
//
// The producers wrap it with the table and column that refused, because
// that is the only diagnostic a caller gets: nothing exported asks a
// *Table which filters it carries, so in a schema where four tables are
// scoped an unwrapped sentence would say the same thing whichever one
// of them stopped the query. Match it with errors.Is.
var ErrTenantMissing = errors.New("drops/clickhouse: table is tenant-scoped but ctx has no tenant")

// ErrTenantMismatch is returned when a row or a binding carries a
// tenant value that disagrees with the ctx tenant. Catches the
// "background job stamped the wrong tenant" class of bug.
var ErrTenantMismatch = errors.New("drops/clickhouse: row tenant disagrees with ctx tenant")

// TenantFilter is the canonical [ContextFilterFunc]: it reads the
// tenant off ctx and renders "<col> = ?".
//
//	Events.ContextFilter(clickhouse.TenantFilter(EventTenantID))
//
// It fails closed. A ctx with no tenant produces [ErrTenantMissing] and
// no statement at all, rather than a query that quietly spans every
// customer — which is the only defensible default for a filter whose
// absence is invisible in the result set. A background job that legally
// has no tenant says so with Unscoped() at the query, where a reviewer
// can see it.
//
// col is rendered as given, so pass the handle belonging to the table
// the filter is registered on; an alias handle would qualify with an
// alias the query has no FROM entry for. A statement that names the
// table under an alias is handled the other way round, by
// Table.resolveFilterExprs.
//
// The refusal is wrapped as "<table>.<column>", resolved once here
// rather than per call, so a caller who forgot the tenant on one
// request is told which table refused.
func TenantFilter(col ColRef) ContextFilterFunc {
	c := col.col()
	where := tenantAxisName(c)
	return func(ctx context.Context) (drops.Expression, error) {
		t, ok := TenantFrom(ctx)
		if !ok {
			return nil, fmt.Errorf("%w: %s", ErrTenantMissing, where)
		}
		return Eq(c, t), nil
	}
}

// tenantAxisName renders a tenant column as "table.column" for an error
// message, or as the bare column name for a handle that has not been
// added to a table — which is a declaration mistake, and one this
// message should describe rather than panic over.
func tenantAxisName(c *Column) string {
	if c == nil {
		return "?"
	}
	if t := c.Table(); t != nil {
		return t.Name() + "." + c.Name()
	}
	return c.Name()
}

// ScopeByTenant marks col as the entity's tenant axis. Every subsequent
// Query reads the tenant from ctx (via WithTenant) and AND-s
// "<col> = ?" into the predicate; Create and CreateMany stamp the
// tenant onto the row.
//
// The axis is installed as a [Table.ContextFilter] rather than injected
// by each Entity method, and that is what makes it a defence rather
// than a habit: db.Select().From(e.Table()) has no entity in the call
// and is scoped exactly as Entity.Query is, and so is the SELECT that
// [VectorStore] builds over the same table.
//
// The consequence worth stating plainly: from here on *every* query
// against this table needs a tenant on its ctx, including one built
// straight from db.Select(). Queries that legitimately span tenants say
// so with Unscoped().
//
// Panics if col has no matching struct field — fail loudly at startup
// rather than at the first query.
func (e *Entity[T]) ScopeByTenant(col ColRef) *Entity[T] {
	c := col.col()
	for _, cf := range e.colFields {
		if cf.col.key() == c.key() {
			e.tenantCol = c
			e.tenantField = cf.field
			// The filter closes over the entity, not over the column,
			// so there is one source of truth: whatever tenantPredicate
			// answers is what the statement carries.
			e.table.setContextFilter(entityFilterKey(e), e.tenantPredicate)
			// The write-side half of the same axis. A predicate scopes
			// the statements that have a WHERE clause; an INSERT has
			// none, so what it needs is the column to stamp — see
			// [Table.ScopeWritesByTenant] and [InsertBuilder.ToSQLCtx].
			// Declared on the table rather than kept on the entity
			// because db.Insert(e.Table()) has no entity to ask.
			e.table.setTenantAxis(c)
			return e
		}
	}
	panic("drops/clickhouse: ScopeByTenant column has no matching struct field on " + e.table.Name())
}

// entityFilterKey names the slot on the table that this entity's own
// context filter occupies, so an entity rebuilt per request replaces
// its filter instead of stacking another copy onto the shared table.
// See ctxFilter for why that is a pattern worth making idempotent.
func entityFilterKey[T any](e *Entity[T]) string {
	var zero T
	return "entity:" + reflect.TypeOf(zero).String() + ":tenant"
}

// tenantPredicate returns "tenantCol = <ctx tenant>" when the entity is
// scoped, nil when it isn't, or ErrTenantMissing when scoped without a
// ctx tenant.
//
// It is registered on the table as a context filter by ScopeByTenant
// and called by the executors; nothing injects it directly. Two places
// that build the same predicate would eventually disagree — and the way
// they disagree is that one of them stops being applied to a path
// somebody added later.
func (e *Entity[T]) tenantPredicate(ctx context.Context) (drops.Expression, error) {
	if e.tenantCol == nil {
		return nil, nil
	}
	t, ok := TenantFrom(ctx)
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrTenantMissing, tenantAxisName(e.tenantCol))
	}
	return Eq(e.tenantCol, t), nil
}

// tenantWriteAxis returns the column and struct field a write stamps
// the ctx tenant onto, and whether this entity has one.
//
// The entity's own axis comes first; failing that the table's, because
// a tenant column is a property of the table — the same reasoning that
// put the read-side predicate on the table. A schema that scopes its
// reads with Table.ContextFilter and names its write column with
// [Table.ScopeWritesByTenant] has an axis this entity never declared,
// and a Create that ignored it would bind the struct's zero tenant to a
// column the builder is about to stamp — two answers for one row.
//
// A table axis with no matching field on T is not an error and not a
// gap: the row simply cannot carry a tenant, and the stamping happens
// on the binding instead, in [InsertBuilder.resolveCtx].
func (e *Entity[T]) tenantWriteAxis() (*Column, []int, bool) {
	if e.tenantCol != nil {
		return e.tenantCol, e.tenantField, true
	}
	c := e.table.tenantAxis()
	if c == nil {
		return nil, nil, false
	}
	for _, cf := range e.colFields {
		if cf.col.key() == c.key() {
			return c, cf.field, true
		}
	}
	return nil, nil, false
}

// stampTenant ensures r's tenant field matches ctx — assigns a zero
// value, rejects a mismatching one.
func (e *Entity[T]) stampTenant(ctx context.Context, r *T) error {
	col, field, ok := e.tenantWriteAxis()
	if !ok {
		return nil
	}
	t, ok := TenantFrom(ctx)
	if !ok {
		return fmt.Errorf("%w: %s", ErrTenantMissing, tenantAxisName(col))
	}
	fv := reflect.ValueOf(r).Elem().FieldByIndex(field)
	if fv.IsZero() {
		// Assign — set via reflection. Fields must be settable.
		ctxTenant := reflect.ValueOf(t)
		if !ctxTenant.Type().AssignableTo(fv.Type()) {
			// A tenant sourced as an int and a column typed int64
			// are the same tenant, so the conversion is worth
			// making — but only when what comes out names the
			// tenant that went in, which is [sameTenant]'s
			// question and not ConvertibleTo's. Converting on
			// ConvertibleTo alone stamped a ctx tenant of 65 into
			// a text column as the rune "A" while the WHERE
			// clause still addressed 65: one statement assigning
			// one tenant and addressing another, which hands the
			// row to whoever owns "A".
			//
			// A column whose type disagrees with the ctx tenant's
			// is the schema saying these are not the same kind of
			// tenant, so this refuses rather than reaching for
			// strconv: a conversion invented here would silently
			// accept the type confusion the schema is reporting,
			// and the caller would never learn that the tenant it
			// thinks it wrote under is not the one on the row.
			if !ctxTenant.Type().ConvertibleTo(fv.Type()) {
				return fmt.Errorf("%w: %s cannot hold the ctx tenant",
					ErrTenantMismatch, tenantAxisName(col))
			}
			conv := ctxTenant.Convert(fv.Type())
			if !sameTenant(conv.Interface(), t) {
				return fmt.Errorf("%w: %s cannot hold the ctx tenant",
					ErrTenantMismatch, tenantAxisName(col))
			}
			ctxTenant = conv
		}
		fv.Set(ctxTenant)
		return nil
	}
	// r already carries a tenant, and it must be this one. Compared
	// with [sameTenant] rather than reflect.DeepEqual, which is a
	// type comparison as much as a value one: int64(77) on the
	// column and int(77) on ctx were a mismatch, so the refusal
	// fired on a match and Update was unusable for every caller
	// whose tenant does not round-trip through its transport as the
	// column's exact type.
	if !sameTenant(fv.Interface(), t) {
		return fmt.Errorf("%w: %s carries another tenant's value",
			ErrTenantMismatch, tenantAxisName(col))
	}
	return nil
}

// sameTenant reports whether a bound tenant value and the ctx tenant
// name the same tenant.
//
// The conversion mirrors [Entity.stampTenant]: a tenant sourced as an
// int and a column typed int64 are the same tenant, and refusing that
// pairing would reject the very rows the entity methods stamp. The
// string guard is not decoration — Go converts an integer to a string
// as a rune, so without it tenant 65 and tenant "A" would compare
// equal, and a numeric tenant would be accepted as the owner of a text
// tenant column's row.
//
// The comparison is a round trip — convert, compare, convert back,
// compare again — because a one-way conversion calls a truncating pair
// equal. int64(1<<32|77) and int32(77) convert onto each other's type
// and match in whichever direction throws the high bits away, so a
// check that converts only one way accepts the pair and the statement
// goes out carrying a value the ctx never named. Only a conversion
// that loses nothing in either direction names the same tenant.
func sameTenant(bound, want any) bool {
	if reflect.DeepEqual(bound, want) {
		return true
	}
	bv, wv := reflect.ValueOf(bound), reflect.ValueOf(want)
	if !bv.IsValid() || !wv.IsValid() {
		return false
	}
	bt, wt := bv.Type(), wv.Type()
	if (bt.Kind() == reflect.String) != (wt.Kind() == reflect.String) {
		return false
	}
	if !bt.ConvertibleTo(wt) || !wt.ConvertibleTo(bt) {
		return false
	}
	conv := bv.Convert(wt)
	if !reflect.DeepEqual(conv.Interface(), want) {
		return false
	}
	return reflect.DeepEqual(conv.Convert(bt).Interface(), bound)
}
