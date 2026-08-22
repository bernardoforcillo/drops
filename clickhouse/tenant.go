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
// The raw builders answer the same question, because a table's
// promise cannot turn on which spelling of "write a row" the caller
// reached for. db.Insert stamps the axis onto every row that leaves
// it out and compares a binding that names it. db.Update may only
// RESTATE it: an assignment binding the tenant the ctx already
// carries renders, and anything else naming the axis is refused —
// another tenant's value, and equally an expression whose result only
// the server knows, a transfer written as arithmetic being still a
// transfer. That is asked of an UPDATE hook's assignment as much as
// of the caller's own, a hook being registered on the table and
// reaching every UPDATE against it. Unscoped is the opt-out for both
// builders, and what saying it gives up is section 3's subject.
//
// Restating is permitted where Patch refuses even that, and the
// asymmetry is deliberate rather than a drift. Update writes every
// mapped column of the row, the axis among them, having stamped it
// from ctx one call earlier, so the value it assigns is the ctx
// tenant's by construction: the rule the raw builder enforces is the
// one the entity path obeys, rather than one the entity path is
// exempt from. A patch op list never passes through a stamp — it is
// built out of the fields a request named — so nothing in it is ever
// the stamp's own output, and the stricter rule costs it nothing.
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
// On an UPDATE, Unscoped additionally means the SET list may assign
// the axis. Both halves of the statement give way together, which is
// what keeps it one flag: the WHERE clause stops being confined to
// the ctx tenant's rows in the same breath as the SET list stops
// being confined to its value, so the statement that moves a row
// between tenants — a migration, a merge of two accounts, an admin
// tool — is writable here and says so where a reviewer reads it.
// clickhouse models no UPDATE for this to be about; section 2 says
// why.
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
// "The same name" is the SERVER's question rather than Go's, so each
// dialect answers it in its own identKey. sqlite and mysql resolve a
// column name case-insensitively however it is quoted, so a handle
// spelled TENANTID renders as the axis there, and matching on the
// bytes was the same defect one shift key further in — the guard
// answering no for a handle the renderer answers yes for. Those two
// fold case. pg and clickhouse compare a quoted identifier byte for
// byte, and drops quotes every identifier it writes, so there the two
// spellings are two columns: a differently-cased handle names a
// column the table does not have, the server refuses the statement,
// and folding here would instead refuse a schema that legitimately
// declares both.
//
// A hook is asked the same question. The bound set that makes
// "user-supplied values win" true is keyed by the name a column
// renders as, so a hook holding another handle for a column the
// caller already bound sees that binding rather than adding a second
// one. What a second one costs is the server's to decide — a
// duplicate column or a duplicate assignment is an error in one
// dialect and last-wins in another — and the axis can be the column
// it is about.
//
// The axis handle itself is one of the table's own columns, and a
// handle that is not is refused where it is declared.
// Table.ScopeWritesByTenant used to take whatever it was given, and
// what it is given reaches the INSERT column list and every axis
// check the package makes — so a handle from another table could name
// a column this table does not have, leaving the stamp to render a
// name the server refuses and a refusal about the axis to contradict
// itself. ScopeByTenant already panicked for a column with no
// matching struct field; both setters fail that way now.
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
// can bind — a string id, uint64, UUID string, etc.
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
// pedantry: the most surprising producer is a bare
// db.Select().From(Events) with no Entity anywhere in the call, and
// "entity is tenant-scoped" would send that caller looking for an
// entity they never wrote.
//
// The producers wrap it with the table and column that refused, because
// that is the only diagnostic a caller gets: nothing exported asks a
// *Table which CONTEXT filters it carries, so in a schema where four
// tables are scoped an unwrapped sentence would say the same thing
// whichever one of them stopped the query. Match it with errors.Is.
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
// Here it is the name itself. ClickHouse identifiers are
// case-sensitive, quoted or not, so "tenantId" and "TenantId" are two
// columns and a handle spelled the second way names a column a table
// declaring the first does not have. The statement is then refused by
// the server as UNKNOWN_IDENTIFIER, which is the fail-loud answer, and
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
			e.table.setContextFilter(entityFilterKey(e), e.tenantPredicate)
			// The write-side half of the same axis. A predicate scopes
			// the statements that have a WHERE clause; an INSERT has
			// none, so what it needs is the column to stamp — see
			// [Table.ScopeWritesByTenant] and [InsertBuilder.ToSQLCtx].
			// Declared on the table rather than kept on the entity
			// because db.Insert(e.Table()) has no entity to ask.
			e.table.setTenantAxis(cf.col)
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
		return nil, fmt.Errorf("%w: %s", ErrTenantMissing, columnPath(e.tenantCol))
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
