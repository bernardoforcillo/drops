package mysql

import (
	"context"
	"errors"
	"fmt"
	"reflect"

	"github.com/bernardoforcillo/drops"
)

// Multi-tenant SaaS without explicit data isolation is a leak waiting
// to happen — one forgotten WHERE tenantId = ? and rows cross
// customers. ScopeByTenant + WithTenant make the isolation a property
// of the TABLE rather than of the call site: every statement that reads
// or writes it takes the tenant from ctx and carries the predicate.
// Forgetting to set the ctx errors out — the bad code path fails
// closed, not open.
//
// "Of the table" is load-bearing. Declared on the table, the axis is
// applied by whichever executor runs the statement, which is the same
// list for a root query, a joined table, a CTE body, a subquery
// operand, an UPDATE and a DELETE. A predicate injected by the Entity
// methods instead would reach the queries those methods build and
// nothing else — so db.Select().From(Posts), the spelling the readme
// shows first, would carry nothing at all, on a ctx with no tenant,
// without refusing.
//
// The axis is per table, because the predicate names a column: an
// entity on `users` cannot say what a row of `posts` belongs to. A
// schema is scoped when each tenant-owning table declares its own axis
// — through its entity, or directly with
// Posts.ContextFilter(mysql.TenantFilter(PostTenantID)).
//
//	var Projects = mysql.NewEntity[Project](projects).
//	    ScopeByTenant(projects.Col("tenantId"))
//
//	ctx = mysql.WithTenant(ctx, currentTenant)
//	got, err := Projects.Get(db, ctx, projectID)
//	// SELECT … WHERE (`id` = ?) AND (`tenantId` = ?)
//
// Create stamps the tenant on the row automatically before insert (or
// rejects a row that already carries a different one), so a stray
// background job cannot silently insert into the wrong tenant.
//
// # Where the automatic scoping stops
//
// These predicates are not an isolation boundary the way a server-side
// policy is. MySQL has no row-level security to put underneath them —
// its nearest equivalent is a definer-rights view, which is a schema
// object drops does not manage — so the list below is not a footnote,
// it is the whole of what is left when the predicates do not reach:
//
//   - a raw statement, through [DB.Exec] or [DB.Query], carries what
//     the caller wrote and nothing else;
//   - a [drops.Raw] fragment, or a caller's own [drops.ExprFunc], is
//     text drops never parses — including one used as a subquery body,
//     a CTE body, a FROM source passed to [SelectBuilder.FromExpr], or
//     a filter's predicate;
//   - a view: the server evaluates the body the CREATE VIEW stored, and
//     drops sees only the view's name;
//   - a statement said Unscoped(), which is the point of the method;
//   - an INSERT into a table that declared a read filter and no write
//     column — see [Table.ScopeWritesByTenant] for why drops will not
//     guess the column;
//   - the outbox, the event store and the idempotency store, which
//     issue their own hand-written SQL against their own tables;
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
// view body as scoped when it is not.

// ==== THE TENANT POLICIES — NORMATIVE ====
//
// This block is byte-identical in pg/tenant.go, sqlite/tenant.go,
// mysql/tenant.go and clickhouse/tenant.go, and a root-level test
// fails when one of the four drifts by a word, by whitespace, or by
// reordering. Edit it in all four or not at all.
//
// It exists because every divergence this phase turned up was a policy
// question that no file owned. Each dialect answered it where the code
// happened to need an answer, the answers disagreed, and the
// disagreement was found by reading four files side by side rather
// than by anything that could fail. resolve.go closed that class for
// the WALK — normalise the dialect name and the four files are one
// file. This closes it for the POLICIES.
//
// What a dialect cannot do is named here rather than written
// differently there, so that one set of words is true in four packages
// instead of four sets each true in one.
//
// --- 1. WHAT COUNTS AS THE SAME TENANT ---
//
// A tenant value bound to a column and the tenant carried on ctx name
// the same tenant when they are equal, or when they convert onto each
// other's type losing nothing in EITHER direction.
//
// The comparison is a round trip — convert, compare, convert back,
// compare again — because a one-way conversion calls a truncating pair
// equal. int64(1<<32|77) and int32(77) convert onto each other's type
// and match in whichever direction throws the high bits away, so a
// check that converts only one way accepts the pair and the statement
// goes out carrying a value the ctx never named. Only a conversion
// that loses nothing both ways names the same tenant.
//
// A string on one side and a non-string on the other is never the same
// tenant, whatever the conversion reports. Go converts an integer to a
// string as a rune, so without that guard a ctx tenant of 65 and a
// TEXT tenant column holding "A" compare equal, and a numeric tenant
// is accepted as the owner of a text column's row.
//
// Nothing here reaches for strconv. A column whose type disagrees with
// the ctx tenant's is the schema reporting a type confusion, and
// inventing a conversion at this point would accept it silently.
//
// This rule is sameTenant, and it is the only definition. Comparing
// with reflect.DeepEqual alone is a bug rather than a stricter
// version of the same thing: int64(77) on the column and int(77) on
// ctx are the same tenant, and DeepEqual fires the refusal on a match.
//
// --- 2. WHAT MAY ASSIGN THE AXIS ---
//
// The tenant column is an axis, never an assignment. It is what
// addresses a row. It is not a field a caller's data may set, and no
// value arriving from a caller is ever read as an instruction to move
// a row to another tenant.
//
// Create stamps the axis from ctx onto a zero field and refuses a
// field naming another tenant. Update does the same, and its stamp
// runs before the validators so a validator reads the row as it will
// be written rather than as the caller happened to build it. A struct
// whose tenant field is zero — one built from a form, or from a
// decoded request body — is stamped rather than allowed to write that
// zero over a row and hand it to no tenant at all; a struct carrying
// somebody else's tenant is [ErrTenantMismatch] rather than a
// transfer of ownership.
//
// Patch refuses ANY op naming the axis, including an op assigning the
// ctx tenant's own value. That op is a no-op only by coincidence of
// the value, and a rule with an exception in it is one a caller can be
// talked into satisfying. Patch is the one write that never reads the
// row first, so nothing downstream notices what it did, and its op
// list is exactly what a handler builds out of the fields a request
// named. The refusal reads the axis off the table, not off a struct
// field bound to it: a patch never touches the struct, and asking for
// the field would skip precisely the entities that cannot stamp
// themselves.
//
// Which row is addressed is a separate question from what is written
// to it, and the table's context filter answers it: the WHERE clause
// carries the ctx tenant like every other statement's. Both halves
// have to be right, and a statement that assigns the axis while its
// WHERE clause still addresses the ctx tenant is not saved by the
// second half. It is confined to the caller's own rows and gives one
// of them away — the half a review checks is correct and the other
// half is the leak.
//
// Dialect surface: clickhouse models neither UPDATE nor DELETE. A
// mutation there is an ALTER TABLE … UPDATE/DELETE, asynchronous and
// not transactional, which this package does not model — so that
// dialect has no Update and no Patch, and the write half of the axis
// is stamping and refusal alone. The other three carry all of it.
//
// --- 3. WHAT UNSCOPED MEANS AT EACH LEVEL ---
//
// Three levels, three meanings. The differences are deliberate and
// none of them is a shorthand for another.
//
// On a statement builder — the SELECT, and the UPDATE, DELETE and
// INSERT the dialect has — [SelectBuilder.Unscoped] and its siblings
// are STATEMENT-WIDE: they drop the DefaultFilter and ContextFilter
// lists of the FROM table and of every joined table alike.
//
// Statement-wide rather than per table, because a caller who says
// Unscoped is describing this query's authority, and a flag that
// unscoped the FROM table while a joined one kept its tenant axis
// would answer with a silently narrowed slice of the rows that were
// asked for. The context filters go too, because a half-scoped
// statement is the worse of the two answers: a caller who reaches for
// Unscoped to read soft-deleted rows and instead gets
// [ErrTenantMissing] has learned nothing about the row they were
// after, and one who gets a tenant predicate they did not ask for
// silently reads a subset. A query that genuinely has to span tenants
// says so in its own WHERE clause, where the intent is on the page.
//
// On an entity query, [EntityQuery.Unscoped] is DEFAULTS-ONLY: it
// drops the default filters and keeps the context ones. The two are
// not the same kind of thing — a default filter is a default scope, a
// context filter is a row-visibility boundary — and their failures are
// not symmetric. Widening a default scope when the caller asked to
// widen it costs nothing; dropping the boundary hands this request
// every tenant's rows, and it would do so on the one method a caller
// reaches for while thinking about soft-deleted rows rather than about
// tenancy. So the tenant axis survives there, and a ctx with no tenant
// is still refused.
//
// At EVERY level, Unscoped stops at the edge of the statement it was
// said on. It does not reach into a statement written inside that one:
// a CTE body, a subquery operand, a subquery bound as a value or
// written in a RETURNING term is a statement of its own and keeps its
// own scoping. That is also how to unscope one relation of a query and
// no other — say Unscoped on that relation's builder.
//
// On an INSERT, [InsertBuilder.Unscoped] additionally means the ctx
// tenant is neither stamped onto the rows nor required, a ctx with no
// tenant is not an error, and the dialect's upsert or replace branch
// is left exactly as it was written. Say which tenant each row belongs
// to by binding the column yourself. It is the escape hatch a
// migration, a backfill, a seed loader or an admin tool needs — the
// statements that legitimately write rows for tenants other than the
// one on the ctx, or for no tenant at all — and it says so at the call
// site, where a reviewer reads it, which is the whole difference
// between this and a package-level switch nobody sees in review.
//
// Dialect surface: the relation-level opt-out, RelConfig.Unscoped, is
// pg's alone. It unscopes one eager-loaded edge and leaves the rest of
// the query scoped. sqlite loads relations but exposes no per-relation
// opt-out; mysql and clickhouse declare no relations for one to apply
// to. In those three, the nesting rule above is the whole of how one
// part of a query is unscoped and no other.
//
//
// --- 4. WHICH HANDLE NAMES THE AXIS ---
//
// A column handle names the tenant axis when it RENDERS as the axis,
// not when it IS the axis. Those two differ, and the difference is a
// leak.
//
// A handle for the same column name taken off a DIFFERENT table object
// renders as the bare column name — which is what the axis renders as
// — and the server applies it to the row the statement addresses.
// Handle identity says the two are unrelated: it collapses alias
// copies onto the column they were declared as and has nothing to say
// about a stranger. So a guard that asks "is this the axis?" by
// identity answers no, and lets through the handle the renderer
// answers yes for. An INSERT then stamped the ctx tenant BESIDE the
// caller's binding instead of checking it, a dialect's upsert branch
// kept the assignment it exists to drop, and a patch assigned the axis
// under a WHERE clause still addressing the ctx tenant.
//
// So the write paths match the axis by rendered column name, and check
// EVERY occurrence rather than stopping at the first. Name equality is
// the weaker test and the right one here: identity implies it, so
// nothing that matched before stops matching, and column names are
// unique within a table, so no column of the table itself can collide.
//
// Where a statement can refuse earlier, it does: an op naming a column
// of another table is refused as exactly that, whatever the column is,
// because the statement it renders addresses a relation the query does
// not name. Patch does this, and the axis case stops being reachable
// rather than being caught.
//
// Dialect surface: clickhouse has no Patch, so there the earlier
// refusal has no op list to apply to and the rendered-name rule is the
// whole of it.
// ==== END OF THE TENANT POLICIES ====

type tenantCtxKey int

const tenantKey tenantCtxKey = 1

// WithTenant returns a context carrying tenant. Pass anything the driver
// can bind — a string id, int64, UUID string, etc.
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
// pedantry: since the axis lives on the Table the most surprising
// producer is a bare db.Select().From(Posts) with no Entity anywhere in
// the call, and "entity is tenant-scoped" would send that caller
// looking for an entity they never wrote.
//
// The producers wrap it with the table and column that refused, because
// that is the only diagnostic a caller gets: nothing exported asks a
// *Table which filters it carries, so in a schema where four tables are
// scoped an unwrapped sentence would say the same thing whichever one
// of them stopped the query. Match it with errors.Is.
var ErrTenantMissing = errors.New("drops/mysql: table is tenant-scoped but ctx has no tenant")

// ErrTenantMismatch is returned when a row or a binding carries a
// tenant value that disagrees with the ctx tenant. Catches the
// "background job stamped the wrong tenant" class of bug.
var ErrTenantMismatch = errors.New("drops/mysql: row tenant disagrees with ctx tenant")

// TenantFilter is the canonical [ContextFilterFunc]: it reads the
// tenant off ctx and renders "<col> = ?".
//
//	Posts.ContextFilter(mysql.TenantFilter(PostTenantID))
//
// It fails closed. A ctx with no tenant produces [ErrTenantMissing] and
// no statement at all, rather than a query that quietly spans every
// customer — which is the only defensible default for a filter whose
// absence is invisible in the result set. A background job that legally
// has no tenant says so with Unscoped() at the query, where a reviewer
// can see it.
//
// col is rendered as given, so pass the handle belonging to the table
// the filter is registered on; a handle taken off an alias would
// qualify with an alias the query has no FROM entry for. A statement
// that names the table under an alias is handled the other way round,
// by Table.resolveFilterExprs.
//
// The refusal is wrapped as "<table>.<column>", resolved once here
// rather than per call, so a caller who forgot the tenant on one
// request is told which table refused.
func TenantFilter(col ColRef) ContextFilterFunc {
	c := col.col()
	where := columnPath(c)
	return func(ctx context.Context) (drops.Expression, error) {
		t, ok := TenantFrom(ctx)
		if !ok {
			return nil, fmt.Errorf("%w: %s", ErrTenantMissing, where)
		}
		return Eq(c, t), nil
	}
}

// columnPath renders a column as "table.column" for an error message,
// or as the bare column name for a handle that has not been added to a
// table — which is a declaration mistake, and one this message should
// describe rather than panic over.
//
// The table it names is the one the HANDLE was declared on rather than
// the one the statement runs against, and that is what makes it worth
// having beyond the tenant errors: a refusal reading "posts.tenantId"
// under a statement about "users" says which mistake was made, where
// the bare "tenantId" would read as the right column.
func columnPath(c *Column) string {
	if c == nil {
		return "?"
	}
	if t := c.Table(); t != nil {
		return t.Name() + "." + c.Name()
	}
	return c.Name()
}

// namesAxis reports whether a bound column handle will RENDER as the
// tenant axis in the statement being built.
//
// The comparison is on the rendered column name rather than on
// [Column.key], because those two questions have different answers for
// a handle obtained from a different table object — and the renderer
// asks the first one. An INSERT column list, an UPDATE SET target and
// an upsert assignment all write the bare name, so a foreign
// OtherTable.TenantID and this table's tenant column are one column as
// far as the server is concerned while key calls them strangers. A
// check that compares by key therefore reads such a handle as "not the
// axis" and lets the statement bind it anyway: on an INSERT the ctx
// stamp was then APPENDED ALONGSIDE it, and a server that accepts a
// duplicate column and keeps the first — SQLite does — wrote the row
// under a tenant the ctx never named.
//
// Name equality is the weaker of the two tests and that is the point:
// key equality implies it, since an alias copy keeps the declared
// name, so nothing that matched before stops matching. Within one
// table names are unique, so a column of the entity's own table that
// is not the axis cannot collide with it here.
func namesAxis(c, axis *Column) bool {
	if c == nil || axis == nil {
		return false
	}
	return c.Name() == axis.Name()
}

// ScopeByTenant marks col as the entity's tenant axis. Every subsequent
// Get / Query / Update / Delete reads the tenant from ctx (via
// WithTenant) and AND-s "<col> = ?" into the predicate; Create stamps
// the tenant onto the row.
//
// The axis is installed as a [Table.ContextFilter] rather than injected
// by each Entity method, and that is what makes it a defence rather
// than a habit — see the file comment.
//
// The consequence worth stating plainly: from here on *every* query
// against this table needs a tenant on its ctx, including one built
// straight from db.Select(). Queries that legitimately span tenants say
// so with Unscoped().
//
// Panics if col has no matching struct field — fail loudly at startup
// rather than at the first query.
//
// col is matched by Column.key, so a handle taken off a table alias
// names the same axis as the declared one. What is stored is the
// entity's own handle rather than the one passed in: the predicate has
// to qualify with the table this entity queries, and an alias handle
// would qualify with an alias no such query names.
func (e *Entity[T]) ScopeByTenant(col ColRef) *Entity[T] {
	c := col.col()
	for _, cf := range e.colFields {
		if cf.col.key() != c.key() {
			continue
		}
		e.tenantCol = cf.col
		e.tenantField = cf.field
		// The filter closes over the entity, not over the column,
		// so there is one source of truth: whatever tenantPredicate
		// answers is what the statement carries.
		e.table.setContextFilter(rowScopeFilterKey(e.rowType, "tenant"), e.tenantPredicate)
		// The write-side half of the same axis. A predicate scopes
		// the statements that have a WHERE clause; an INSERT has
		// none, so what it needs is the column to stamp — see
		// [Table.ScopeWritesByTenant] and [InsertBuilder.ToSQLCtx].
		// Declared on the table rather than kept on the entity
		// because db.Insert(e.Table()) has no entity to ask.
		e.table.setTenantAxis(cf.col)
		return e
	}
	panic("drops/mysql: ScopeByTenant column has no matching struct field on " + e.table.Name())
}

// rowScopeFilterKey names the context-filter slot an entity owns on its
// table, so that rebuilding the entity replaces its filter instead of
// stacking another one — see ctxFilter.
//
// It is keyed by the ROW TYPE rather than by the entity pointer,
// because the pattern the key exists for is an entity rebuilt per
// request: two Entity[Post] values are two handles on one declaration,
// and a pointer key would give each request its own slot.
func rowScopeFilterKey(rowType reflect.Type, kind string) string {
	if rowType == nil {
		return kind + ":"
	}
	if rowType.Name() == "" || rowType.PkgPath() == "" {
		return kind + ":" + rowType.String()
	}
	return kind + ":" + rowType.PkgPath() + "." + rowType.Name()
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
		return nil, fmt.Errorf("%w: %s", ErrTenantMissing, columnPath(e.tenantCol))
	}
	return Eq(e.tenantCol, t), nil
}

// tenantWriteAxis returns the column and struct field a write stamps
// the ctx tenant onto, and whether this entity has one.
//
// The entity's own axis comes first; failing that the table's, because
// a tenant column is a property of the table — the same reasoning that
// puts the read-side predicate there. A schema that scopes its reads
// with Table.ContextFilter and names its write column with
// [Table.ScopeWritesByTenant] has an axis this entity never declared,
// and a Create that ignored it would bind the struct's zero tenant to
// a column the builder is about to stamp — two answers for one row.
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
			return cf.col, cf.field, true
		}
	}
	return nil, nil, false
}

// tenantAxisColumn returns the entity's tenant axis, whoever declared
// it — [Entity.ScopeByTenant] or the table itself.
//
// Unlike tenantWriteAxis it does not ask for a struct field bound to
// the column: a patch never touches the struct, and an entity that
// maps no field to its tenant column is still writing into a scoped
// table. Requiring the field here would make the axis check skip
// exactly the entities that cannot stamp themselves.
func (e *Entity[T]) tenantAxisColumn() (*Column, bool) {
	if e.tenantCol != nil {
		return e.tenantCol, true
	}
	if c := e.table.tenantAxis(); c != nil {
		return c, true
	}
	return nil, false
}

// stampTenant ensures r's tenant field matches ctx — assigns a zero
// value, rejects a mismatching one.
//
// It runs on the struct before the bindings are taken, so the row the
// caller handed in comes back carrying the tenant it was written under.
// The INSERT is stamped again from the binding side in
// [InsertBuilder.resolveCtx], and the two agree by construction: this
// one has already put the ctx tenant in the field the binding reads.
func (e *Entity[T]) stampTenant(ctx context.Context, r *T) error {
	col, field, ok := e.tenantWriteAxis()
	if !ok {
		return nil
	}
	t, ok := TenantFrom(ctx)
	if !ok {
		return fmt.Errorf("%w: %s", ErrTenantMissing, columnPath(col))
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
					ErrTenantMismatch, columnPath(col))
			}
			conv := ctxTenant.Convert(fv.Type())
			if !sameTenant(conv.Interface(), t) {
				return fmt.Errorf("%w: %s cannot hold the ctx tenant",
					ErrTenantMismatch, columnPath(col))
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
			ErrTenantMismatch, columnPath(col))
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
