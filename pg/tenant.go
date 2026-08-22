package pg

import (
	"context"
	"errors"
	"fmt"
	"reflect"

	"github.com/bernardoforcillo/drops"
)

// Multi-tenant SaaS without explicit data isolation is a leak waiting
// to happen — one forgotten WHERE tenantId = $1 and rows cross
// customers. ScopeByTenant + WithTenant make the isolation a property
// of the table rather than the call site: every statement that reads or
// writes it takes the tenant from ctx and carries the predicate.
// Forgetting to set the ctx errors out — bad code path fails closed,
// not open.
//
// "Of the table" is load-bearing and was learned the hard way. While
// the predicate was injected by the Entity methods it reached the
// queries those methods built and nothing else, so an eager-loaded
// relation — whose child query is built by the relation loader, with no
// Entity anywhere in the call — came back holding every tenant's rows.
// Declared on the table, the axis is applied by whatever executor runs
// the statement, which is the same list for a root query and for a
// relation edge.
//
// The axis is per table, because the predicate names a column: an
// entity on `users` cannot say what a row of `posts` belongs to. A
// schema is scoped when each tenant-owning table declares its own axis
// — through its entity, or directly with
// Posts.ContextFilter(pg.TenantFilter(PostTenantID)).
//
//	var Projects = pg.NewAutoEntity[Project]("projects").
//	    ScopeByTenant(ProjectsCols.TenantID)
//
//	ctx = pg.WithTenant(ctx, currentTenant)
//	got, err := Projects.Get(db, ctx, projectID)
//	// SELECT ... WHERE id = $1 AND "tenantId" = $2
//
// Create stamps the tenant on r automatically before insert (or
// rejects if r already carries a different tenant) so a stray
// background job can't silently insert into the wrong tenant.
//
// Three things a reader of this file has to take away with it.
//
// These predicates are not the isolation boundary. PostgreSQL
// row-level security is, and this layer is what makes the common path
// fast, legible and correct on top of it — see "The predicates are not
// the boundary" in the package doc for why no amount of further work
// here changes that, and for what to declare instead.
//
// The predicate reaches every statement drops composed, to any depth —
// a CTE body, a subquery operand, a set-operation operand, an
// eager-loaded edge, the predicate another table's filter answers with.
// Where it stops is listed in the package doc, under "Where the
// automatic scoping stops"; the entries there are the shapes that stay
// the caller's to scope, and a reviewer who has not read them will read
// a raw fragment or a view body as scoped when it is not.
//
// And it is no longer only pg. drops/sqlite, drops/mysql and
// drops/clickhouse now declare the axis on the table and resolve it in
// the executors, on the same terms and in the same words — sqlite's
// Entity-injected predicate, the shape described above as the one
// learned the hard way, is gone. What differs between the four is
// surface rather than mechanism, and the package doc's "Where the
// automatic scoping stops" says where, per dialect. Read the list in
// the dialect you are using: clickhouse in particular has no UPDATE,
// no DELETE, no upsert and no relations for an axis to reach into.
//
// What does NOT port is the boundary. PostgreSQL row-level security is
// the isolation boundary these predicates sit on top of, and the other
// three dialects have no equivalent to sit on. There the predicates are
// the whole of what there is.

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
// A ctx carries a tenant when the value on it is not nil. WithTenant
// takes an `any`, so a nil of some type — a (*string)(nil) read out of
// a request struct — arrives inside an interface that is not itself
// nil, and a check for the interface being nil reports a tenant that
// is not there. Stamped onto a row, it wrote NULL, which no tenant
// predicate matches: the row belonged to nobody, was invisible to
// every later request including the one that wrote it, and was
// reported as written. A nil of any type is no tenant, and every path
// that needs one refuses with [ErrTenantMissing] rather than binding
// it.
//
// A zero that is not a nil — an empty string, a zero int — IS a
// tenant. The schema can store it and it addresses the same rows on
// the way back out, which is the whole difference: it is
// self-consistent where a NULL is not.
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
// tenant, whatever the conversion reports.
//
// What that guard rules out is not the integer, which is what it was
// described as being for through eleven rounds of this phase. Go
// converts an integer to a string as a rune, but nothing converts a
// string back to an integer, so the round trip above already refuses
// 65 and "A" and the guard is never reached for that pair. What it
// rules out is []byte and []rune: those convert onto a string and back
// losing nothing, so without it []byte("acme") and "acme" are the same
// tenant. They are the same CHARACTERS. A schema holding its tenant as
// bytes on one table and as text on another is reporting a type
// confusion, exactly as a numeric ctx tenant on a text column is, and
// it is refused on the same grounds.
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
// field naming another tenant. Update does the same. Both stamp
// BEFORE the validators run, so a validator reads the row as it will
// be written rather than as the caller happened to build it — handed
// the row as built, a validator that checks the tenant is checking a
// field the statement is about to replace. A struct whose tenant
// field is zero — one built from a form, or from a decoded request
// body — is stamped rather than allowed to write that zero over a row
// and hand it to no tenant at all; a struct carrying somebody else's
// tenant is [ErrTenantMismatch] rather than a transfer of ownership.
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
// Validators are the mirror image: only pg and clickhouse register
// them. sqlite and mysql have no Entity.Validate, so the ordering rule
// above is about those two, and in the other two there is nothing for
// a stamp to run before.
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

// WithTenant returns a context that carries tenant. Pass anything
// drivers can bind — a string id, int64 user id, UUID, or struct
// implementing driver.Valuer.
func WithTenant(ctx context.Context, tenant any) context.Context {
	return context.WithValue(ctx, tenantKey, tenant)
}

// TenantFrom returns the tenant on ctx (ok=false when absent).
//
// A nil tenant is an absent one. WithTenant takes an `any`, so a nil of
// some type — a (*string)(nil) read out of a request struct, a nil map
// from a header lookup — arrives inside an interface that is not itself
// nil, and asking whether the interface is nil answered that the ctx
// carries a tenant. It carries none: what reached the statement was
// NULL, which no tenant predicate matches. See section 1 of the
// normative policy block above for why that is refused rather than
// bound.
func TenantFrom(ctx context.Context) (any, bool) {
	v := ctx.Value(tenantKey)
	if v == nil || isNilTenant(v) {
		return nil, false
	}
	return v, true
}

// isNilTenant reports whether v is a nil held inside a non-nil
// interface, which is the one shape a comparison against nil cannot
// see.
//
// The kinds listed are every kind that HAS a nil. A zero of any other
// kind — an empty string, a zero int — is a value the schema can store
// and address rows by, so it is a tenant like any other and is not
// this function's business.
func isNilTenant(v any) bool {
	switch rv := reflect.ValueOf(v); rv.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map,
		reflect.Pointer, reflect.Slice, reflect.UnsafePointer:
		return rv.IsNil()
	}
	return false
}

// ErrTenantMissing is returned when a statement reads or writes a table
// that is scoped by tenant and the ctx carries no tenant. Surfacing
// this rather than silently running a cross-tenant query is the whole
// point of the feature.
//
// It names the table rather than the entity, and the difference is not
// pedantry. While the axis lived on the Entity, an entity was the only
// thing that could produce this error; since it moved onto the Table
// the most surprising producer is a bare db.Select().From(Posts) with
// no Entity anywhere in the call — and "entity is tenant-scoped" sent
// that caller looking for an entity they never wrote.
//
// The producers wrap it with the table and column that refused, because
// that is the only diagnostic a caller gets: nothing exported asks a
// *Table which CONTEXT filters it carries, so in a schema where four
// tables are scoped an unwrapped sentence would say the same thing
// whichever one of them stopped the query. Match it with errors.Is.
var ErrTenantMissing = errors.New("drops/pg: table is tenant-scoped but ctx has no tenant")

// ErrTenantMismatch is returned when a row or a binding carries a
// tenant value that disagrees with the ctx tenant. Catches the
// "background job stamped the wrong tenant" class of bug.
var ErrTenantMismatch = errors.New("drops/pg: row tenant disagrees with ctx tenant")

// TenantFilter is the canonical [ContextFilterFunc]: it reads the
// tenant off ctx and renders "<col> = $tenant".
//
//	Posts.ContextFilter(pg.TenantFilter(PostTenantID))
//
// It fails closed. A ctx with no tenant produces [ErrTenantMissing] and
// no statement at all, rather than a query that quietly spans every
// customer — which is the only defensible default for a filter whose
// absence is invisible in the result set. A background job that legally
// has no tenant says so with Unscoped() at the query, where a reviewer
// can see it.
//
// col is rendered as given, so pass the handle belonging to the table
// the filter is registered on: an alias handle would qualify with an
// alias the query has no FROM entry for.
//
// The refusal is wrapped as "<table>.<column>", resolved once here
// rather than per call, so a caller who forgot the tenant on one
// request is told which table refused. See [ErrTenantMissing] for why
// that wrapping is the whole diagnostic.
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
//
// "The same name" is [identKey]'s question rather than a byte
// comparison, because which spellings the server resolves to one
// column is a property of the dialect and not of Go.
func namesAxis(c, axis *Column) bool {
	if c == nil || axis == nil {
		return false
	}
	return identKey(c.Name()) == identKey(axis.Name())
}

// identKey returns the form in which two rendered column names are one
// column to the server that reads the statement.
//
// Here it is the name itself. Every identifier drops writes goes out
// quoted — see drops.Builder.WriteIdent — and PostgreSQL compares a
// quoted identifier byte for byte, so "tenantId" and "TenantId" are
// two columns and a handle spelled the second way names a column a
// table declaring the first does not have. The statement is then
// refused by the server as 42703, which is the fail-loud answer, and
// folding case here would instead refuse a schema that legitimately
// declares both.
//
// It exists as a named function, doing nothing, because the answer
// differs by dialect and the difference is a leak. sqlite and mysql
// resolve a quoted column name case-insensitively: there "TENANTID"
// IS the axis to the server while an exact-bytes comparison calls the
// two strangers, which is [namesAxis]'s original defect one step
// further in — a guard that asks "is this the axis?" answering no for
// a handle the renderer answers yes for. Their identKey folds case for
// that reason. Asking the question in all four packages is what stops
// one dialect's answer from being carried into another by a reader who
// only saw the comparison.
func identKey(name string) string { return name }

// ScopeByTenant marks col as the entity's tenant axis. Every
// subsequent Get / Query / Update / Delete reads the tenant from
// ctx (via WithTenant) and AND-s "<col> = $tenant" into the
// predicate. Create stamps the tenant onto r automatically.
//
// The axis is installed as a [Table.ContextFilter] rather than injected
// by each Entity method, and that is what makes it a defence rather
// than a habit. An eager-loaded relation has no Entity to ask: its
// child query is built as db.Select().From(rel.To), so a predicate that
// only the entity methods knew about filtered the parents and loaded
// every tenant's children. Registered on the table, the axis is applied
// by whichever executor runs the statement — including the relation
// loaders, the per-parent-limit rewrite, Page and Stream.
//
// The consequence worth stating plainly: from here on *every* query
// against this table needs a tenant on its ctx, including one built
// straight from db.Select(). Queries that legitimately span tenants say
// so with Unscoped().
//
// Panics if col has no matching struct field — fail loudly at
// startup rather than at the first query.
//
// col is matched by Column.key, so a handle taken off a table alias
// names the same axis as the declared one. What is stored is the
// entity's own handle rather than the one passed in: the predicate has
// to qualify with the table this entity queries, and an alias handle
// would qualify with an alias no such query names.
func (e *Entity[T]) ScopeByTenant(col ColRef) *Entity[T] {
	c := col.col()
	for _, cf := range e.colFields {
		if cf.col.key() == c.key() {
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
			// Declared on the table rather than kept on the entity for
			// the reason the filter is: db.Insert(e.Table()) has no
			// entity to ask, and it is the spelling the readme shows.
			e.table.setTenantAxis(cf.col)
			return e
		}
	}
	panic("drops/pg: ScopeByTenant column has no matching struct field on " + e.table.Name())
}

// tenantPredicate returns "tenantCol = $ctx-tenant" when the
// entity is scoped, or nil when it isn't. Returns ErrTenantMissing
// when scoped but no tenant is on ctx.
//
// It is registered on the table as a context filter by ScopeByTenant
// and called by the executors; nothing injects it directly any more.
// Two places that build the same predicate would eventually disagree —
// and the way they disagree is that one of them stops being applied to
// a path somebody added later.
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
// moved the read-side predicate off the entity. A schema that scopes
// its reads with Table.ContextFilter and names its write column with
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
	if f, ok := e.fieldFor(c); ok {
		return c, f, true
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
// pairing would reject the very rows the entity methods stamp.
//
// The string guard is not decoration, and it is not what stops a
// numeric tenant owning a text column's row — the round trip below
// does that, because nothing converts a string back to an integer.
// What the guard stops is []byte and []rune, which DO convert onto a
// string and back losing nothing: without it a ctx tenant of
// []byte("acme") is accepted as the owner of a row whose text tenant
// column holds "acme". It asks the KIND rather than the type, so a
// caller's own named string type still names the same tenant as the
// string a column binds.
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
