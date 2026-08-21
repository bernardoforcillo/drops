package pg

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"

	"github.com/bernardoforcillo/drops"
	"github.com/bernardoforcillo/drops/internal/drift"
)

// Entity binds a Go struct T to a Table and precomputes the metadata
// the CRUD shortcuts need: the column-to-field index path, the
// primary-key column, and the PK's field path inside T. It is the
// entry point for type-safe CRUD operations that scan into T directly,
// without the caller having to declare scan targets.
//
// Declare an Entity once at package level alongside its table:
//
//	type User struct {
//	    ID    int64  `drop:"id"`
//	    Name  string `drop:"name"`
//	    Email string `drop:"email"`
//	}
//
//	var (
//	    Users      = pg.NewTable("users")
//	    UserID     = pg.Add(Users, pg.BigSerial("id").PrimaryKey())
//	    UserName   = pg.Add(Users, pg.Text("name").NotNull())
//	    UserEmail  = pg.Add(Users, pg.Text("email").NotNull().Unique())
//	    UserEntity = pg.NewEntity[User](Users)
//	)
//
// All Entity methods take *DB as their first argument so the same
// entity can be reused across transactions and connection pools:
//
//	u, err := UserEntity.Get(db, ctx, 42)
//	err  = UserEntity.Create(db, ctx, &u)
//	err  = UserEntity.Update(db, ctx, &u)
//	err  = UserEntity.Save(db, ctx, &u) // INSERT or UPDATE depending on PK
//	res, err := UserEntity.Delete(db, ctx, 42)
//
// Entity composes with Phase-1 features: lifecycle hooks (Timestamps,
// SoftDelete, …) registered on the table fire normally because every
// operation routes through the underlying Insert / Update / Delete
// builders.
type Entity[T any] struct {
	table *Table

	// pk / pkField describe the primary key when it is a single
	// column and are nil for a composite one, so the paths that
	// cannot generalise can branch on them. pks / pkFields are
	// always populated, in the table's declaration order.
	pk       *Column
	pkField  []int
	pks      []*Column
	pkFields [][]int

	colFields    []entityColField // columns that map to a struct field
	validators   []Validator[T]
	versionCol   *Column // optimistic-locking version column, nil if none
	versionField []int   // field path on T for the version value

	// fastScan is the optional zero-reflection row scanner — usually
	// supplied by cmd/dropsgen-emitted Register<T>() at init time.
	// When set, the SELECT executors (Get / Query.All / Query.One)
	// skip the reflection path entirely.
	//
	// fastCols is the column list that scanner was generated for, and
	// every executor taking the fast path renders it instead of
	// letting the SELECT default to "*". A scanner that decodes by
	// position against "*" is bound to the table's physical column
	// order, which belongs to the database and not to the generated
	// file: one ALTER TABLE ADD COLUMN in the middle of the table
	// shifts every following value one field sideways, and nothing in
	// the scan can notice. Naming the columns makes the row's order
	// the generator's order.
	fastScan func(Scanner, *T) error
	fastCols []drops.Expression

	// cache, when set via WithCache, makes Get / Query.All /
	// Query.One read-through and Update / Save / Delete
	// write-invalidate. Provides single-flight protection for the
	// PK-by-cache-miss path so a thundering herd resolves to one DB
	// query.
	cache *EntityCache

	// budget, when configured via WithBudget, caps the cost of each
	// Entity operation (max args, max rows, max duration). The zero
	// Budget disables every limit.
	budget Budget

	// audit, when attached via WithAudit, records who-changed-what
	// rows in the same transaction as every Create / Update /
	// Delete.
	audit *auditWiring

	// tenantCol / tenantField, when set by ScopeByTenant, makes
	// the entity tenant-aware: every Get / Query / Update / Delete
	// auto-injects WHERE tenantCol = $ctx-tenant; Create stamps
	// the tenant onto r before binding.
	tenantCol   *Column
	tenantField []int

	// guard, when set by AuthorizeWith, injects an
	// authorisation predicate into every Get / Query / Update /
	// Delete. The subject lives on ctx (WithSubject); missing
	// subject errors out.
	guard Guard

	// rowType is T with pointers stripped. It names the entity in
	// the keys under which the tenant axis and the guard register
	// their context filters on the table.
	rowType reflect.Type
}

// hasRowScope reports whether anything restricts which rows an
// operation on this entity may touch: a tenant axis or an
// authorisation guard declared through the entity, or any context
// filter registered straight onto the table.
//
// The table half is the half that bites, and it is why this predicate
// cannot be a field test on the Entity. P0-1 moved the tenant axis off
// the Entity and onto the Table precisely so it would reach the
// statements no Entity builds, and the spelling this package
// recommends — Posts.ContextFilter(pg.TenantFilter(PostTenantID)) —
// never sets tenantCol at all. An entity over such a table is fully
// scoped and used to answer "unscoped" to whoever only asked the
// Entity.
//
// The invariant the answer guards is the strong one: the PK cache
// never *holds* a scoped row, not merely "a scoped Get does not read
// it". That key is the primary key and nothing else — it has no room
// for the tenant, the subject, or whatever else a filter consults — so
// a scoped row sitting in that namespace is a row waiting to be handed
// to the next caller who asks for that id, whoever they are, without a
// statement ever being sent and therefore without the filter ever
// running or ErrTenantMissing ever firing. Gating only the read leaves
// the namespace poisoned by every write and puts the leak one refactor
// away; so the read in Get and the writes in insertRow and Update are
// all gated on this one predicate. The writes ask through refreshPK,
// which also deletes the entry the gate refuses to overwrite — an entry
// a scoped write leaves in place is an entry nothing will ever correct.
func (e *Entity[T]) hasRowScope() bool {
	return e.tenantCol != nil || e.guard != nil || e.table.hasContextFilters()
}

// Scanner mirrors the subset of drops.Rows the fast scan helpers
// need: one Scan call per row. Generated code is written against
// this narrower interface so it does not depend on drops internals.
type Scanner interface {
	Scan(dest ...any) error
}

// Validator is called before Create / Update / Save with a pointer
// to the candidate row. Returning a non-nil error aborts the
// operation before any SQL is issued. Validators compose: register
// as many as you need, the first to fail wins.
type Validator[T any] func(*T) error

// entityColField bundles a column with its field-index path inside T.
type entityColField struct {
	col   *Column
	field []int
}

// NewEntity validates that T has a field bound to every primary-key
// column on t and precomputes the column ↔ field mapping. It panics
// on misalignment because schemas are typically declared in package
// init blocks — bad config should fail at startup, not at the first
// query.
//
// Field matching rules mirror the row scanner: `drop:"colname"` tag
// wins; otherwise the field name and its snake_case form are tried.
// Fields tagged `drop:"-"` are skipped.
func NewEntity[T any](t *Table, opts ...EntityOption) *Entity[T] {
	var cfg entityConfig
	for _, o := range opts {
		o(&cfg)
	}

	var zero T
	rt := reflect.TypeOf(zero)
	for rt.Kind() == reflect.Ptr {
		rt = rt.Elem()
	}
	if rt.Kind() != reflect.Struct {
		panic(fmt.Sprintf("drops/pg: NewEntity requires T to be a struct, got %s", rt.Kind()))
	}
	fields := fieldMap(rt)

	pks := t.primaryKeyColumns()
	if len(pks) == 0 {
		panic(fmt.Sprintf("drops/pg: NewEntity[%s]: table %q has no PRIMARY KEY column; CRUD shortcuts require one", rt.Name(), t.Name()))
	}
	pkFields := make([][]int, len(pks))
	for i, c := range pks {
		idx, ok := lookupField(fields, c.Name())
		if !ok {
			panic(fmt.Sprintf("drops/pg: NewEntity[%s]: no struct field bound to PK column %q on table %q", rt.Name(), c.Name(), t.Name()))
		}
		pkFields[i] = idx
	}
	// pk / pkField stay set only for a single-column key, so the
	// paths that genuinely cannot generalise — an ON CONFLICT target,
	// the cache's per-row key — can branch on them rather than
	// pretending a composite key is one value.
	var pk *Column
	var pkField []int
	if len(pks) == 1 {
		pk, pkField = pks[0], pkFields[0]
	}

	colFields := make([]entityColField, 0, len(t.Columns()))
	var versionCol *Column
	var versionField []int
	for _, c := range t.Columns() {
		idx, ok := lookupField(fields, c.Name())
		if !ok {
			continue
		}
		colFields = append(colFields, entityColField{col: c, field: idx})
		if c.IsOptimisticVersion() {
			if versionCol != nil {
				panic(fmt.Sprintf("drops/pg: NewEntity[%s]: table %q declares more than one OptimisticLock column", rt.Name(), t.Name()))
			}
			versionCol = c
			versionField = idx
		}
	}
	if versionCol == nil {
		// Catch the misconfiguration where the version column exists
		// but has no matching struct field — the loop above would
		// silently skip it.
		for _, c := range t.Columns() {
			if c.IsOptimisticVersion() {
				panic(fmt.Sprintf("drops/pg: NewEntity[%s]: OptimisticLock column %q has no matching struct field", rt.Name(), c.Name()))
			}
		}
	}

	if err := checkDrift(rt, t, colFields, cfg); err != nil {
		panic(err.Error())
	}

	return &Entity[T]{
		table:        t,
		rowType:      rt,
		pk:           pk,
		pkField:      pkField,
		pks:          pks,
		pkFields:     pkFields,
		colFields:    colFields,
		versionCol:   versionCol,
		versionField: versionField,
	}
}

// EntityOption configures [NewEntity].
type EntityOption func(*entityConfig)

type entityConfig struct {
	allowUnmapped map[string]bool
	allowAny      bool
}

// AllowUnmappedColumns exempts the named columns from the check that
// every column has a struct field.
//
// Use it for columns the database owns and the application never
// writes — a trigger-maintained tsvector, a generated column, a
// counter another service updates. Naming them is the point: an
// exemption you have to type is one you have thought about, unlike
// the silent skip this replaces.
func AllowUnmappedColumns(names ...string) EntityOption {
	return func(c *entityConfig) {
		if c.allowUnmapped == nil {
			c.allowUnmapped = map[string]bool{}
		}
		for _, n := range names {
			c.allowUnmapped[n] = true
		}
	}
}

// AllowAnyUnmappedColumn disables the check entirely. It exists for
// migrating an existing codebase that has too many gaps to name at
// once; prefer [AllowUnmappedColumns], which keeps the check working
// for the columns you have not exempted.
func AllowAnyUnmappedColumn() EntityOption {
	return func(c *entityConfig) { c.allowAny = true }
}

// checkDrift reports columns that no struct field is bound to.
//
// Such a column is dropped from every INSERT and UPDATE the entity
// builds, so a renamed field or a mistyped `drop:` tag stops
// persisting it while everything else keeps working. This panics
// rather than returning an error, matching how NewEntity already
// treats a missing primary key: schemas are declared in package init
// blocks, and startup is where bad configuration should surface.
func checkDrift(rt reflect.Type, t *Table, colFields []entityColField, cfg entityConfig) error {
	if cfg.allowAny {
		return nil
	}
	mapped := make(map[string]bool, len(colFields))
	bound := make(map[string]bool, len(colFields))
	for _, cf := range colFields {
		mapped[cf.col.Name()] = true
		bound[drift.FieldKey(cf.field)] = true
	}
	var missing []string
	for _, c := range t.Columns() {
		if mapped[c.Name()] || c.IsManaged() || cfg.allowUnmapped[c.Name()] {
			continue
		}
		missing = append(missing, c.Name())
	}
	return drift.Report("drops/pg", rt.Name(), t.Name(), missing,
		drift.SpareFields(rt, bound), "pg.AllowUnmappedColumns")
}

// Validate registers a validator that runs before Create / Update /
// Save. Validators are invoked in registration order; the first to
// return a non-nil error aborts the operation. Returns the entity so
// the call can be chained next to NewEntity.
func (e *Entity[T]) Validate(v Validator[T]) *Entity[T] {
	e.validators = append(e.validators, v)
	return e
}

// SetFastScan registers a zero-reflection per-row scanner together
// with the column list it was generated for — the Cols<T> / Scan<T>
// pair cmd/dropsgen emits, handed over by the generated Register<T>
// helper. When set, Get / Query.One / Query.All / Page / Stream
// consume rows directly through scan instead of routing through the
// reflection scanner. Eager-loaded relations still fall back to the
// reflection path because they rely on field-map introspection of
// the loaded slice.
//
// cols is not decoration. It is what the fast-path executors render,
// so the order of the values in the row is the order the generator
// wrote the Scan<T> body in. Left implicit, the statement is a
// SELECT * and the scanner reads the table's physical column order
// instead: an ALTER TABLE ADD COLUMN in the middle of the table then
// shifts every subsequent value into the neighbouring field, and a
// positional scan has nothing to compare against — no error is
// raised, the rows just come back wrong, one field over.
//
// It panics when a name is not a column of the entity's table. The
// generated file and the schema have diverged at that point, and the
// alternative to failing at startup is scanning rows into the wrong
// fields for as long as the process runs; a scanner registered from
// an init block is exactly where bad configuration should surface,
// the same reason NewEntity panics.
func (e *Entity[T]) SetFastScan(cols []string, scan func(Scanner, *T) error) *Entity[T] {
	if len(cols) == 0 {
		panic(fmt.Sprintf(
			"drops/pg: SetFastScan on %s: a positional scanner needs the column list it was generated for; pass Cols%s()",
			e.rowType.Name(), e.rowType.Name()))
	}
	refs := make([]drops.Expression, 0, len(cols))
	for _, n := range cols {
		c := e.table.Col(n)
		if c == nil {
			panic(fmt.Sprintf(
				"drops/pg: SetFastScan on %s: table %q has no column %q; re-run go generate",
				e.rowType.Name(), e.table.Name(), n))
		}
		refs = append(refs, c)
	}
	e.fastCols, e.fastScan = refs, scan
	return e
}

// projectFast restricts sel to the fast scanner's column list. Every
// path that hands rows to fastScan goes through here or builds its
// SELECT with fastCols directly, because the projection and the
// positional decode are one decision: a row is only safe to read by
// position if this statement chose the positions.
func (e *Entity[T]) projectFast(sel *SelectBuilder) *SelectBuilder {
	sel.columns = e.fastCols
	return sel
}

// HasFastScan reports whether a zero-reflection scanner is wired up.
func (e *Entity[T]) HasFastScan() bool { return e.fastScan != nil }

// runValidators runs every registered validator in order. Returns
// the first non-nil error.
func (e *Entity[T]) runValidators(r *T) error {
	for _, v := range e.validators {
		if err := v(r); err != nil {
			return err
		}
	}
	return nil
}

// lookupField resolves a column name to a field index path using the
// scanner's fieldMap. It tries the exact name first, then the
// snake_case form, then the PascalCase form — the same triple-rule
// the scanner uses.
func lookupField(fields map[string][]int, name string) ([]int, bool) {
	if idx, ok := fields[name]; ok {
		return idx, true
	}
	return nil, false
}

// Table returns the table the entity is bound to.
func (e *Entity[T]) Table() *Table { return e.table }

// PK returns the entity's primary-key column.
func (e *Entity[T]) PK() *Column { return e.pk }

// PKs returns the primary-key columns in declaration order. For a
// single-column key it holds the one column [Entity.PK] returns; for
// a composite key PK is nil and this is the whole story.
func (e *Entity[T]) PKs() []*Column {
	out := make([]*Column, len(e.pks))
	copy(out, e.pks)
	return out
}

// ErrKeyArity is returned when a key is given the wrong number of
// values for the entity's primary key.
var ErrKeyArity = errors.New("drops/pg: wrong number of primary-key values")

// pkPredicate builds the WHERE clause addressing one row by key.
//
// The values are positional and must match [Entity.PKs] in order,
// which is the table's declaration order. Getting the count wrong is
// an error rather than a silent partial match: a composite key
// addressed by one of its two columns would happily update every row
// sharing that column.
func (e *Entity[T]) pkPredicate(key []any) (drops.Expression, error) {
	if len(key) != len(e.pks) {
		return nil, fmt.Errorf("%w: table %q has %d key column(s) (%s), got %d value(s)",
			ErrKeyArity, e.table.Name(), len(e.pks), strings.Join(colNames(e.pks), ", "), len(key))
	}
	if len(key) == 1 {
		return Eq(e.pks[0], key[0]), nil
	}
	preds := make([]drops.Expression, len(key))
	for i, c := range e.pks {
		preds[i] = Eq(c, key[i])
	}
	return And(preds...), nil
}

// pkValuesOf reads the key out of a row, in [Entity.PKs] order.
func (e *Entity[T]) pkValuesOf(r *T) []any {
	v := reflect.ValueOf(r).Elem()
	out := make([]any, len(e.pkFields))
	for i, idx := range e.pkFields {
		out[i] = v.FieldByIndex(idx).Interface()
	}
	return out
}

// pkIsZero reports whether every key field is the zero value — the
// test Save uses to decide between insert and update.
func (e *Entity[T]) pkIsZero(r *T) bool {
	v := reflect.ValueOf(r).Elem()
	for _, idx := range e.pkFields {
		if !v.FieldByIndex(idx).IsZero() {
			return false
		}
	}
	return true
}

// isKeyColumn reports whether c is part of the primary key.
//
// Compared by Column.key, not by handle: the entity's pks can come
// from Table.PrimaryKey(cols...) while colFields come from Columns(),
// and on an aliased table those are two handles on one column. Missing
// the overlap puts the key columns into the SET list of every UPDATE
// and stops Create from letting the database fill a defaulted key.
func (e *Entity[T]) isKeyColumn(c *Column) bool {
	for _, k := range e.pks {
		if k.key() == c.key() {
			return true
		}
	}
	return false
}

func colNames(cols []*Column) []string {
	out := make([]string, len(cols))
	for i, c := range cols {
		out[i] = c.Name()
	}
	return out
}

// ----------------------------------------------------------------------
// CRUD operations
// ----------------------------------------------------------------------

// ErrPKNotSet is returned by Update / Save when r's primary-key
// field is the zero value but the operation requires it to be set.
var ErrPKNotSet = errors.New("drops/pg: primary key field is the zero value")

// ErrStaleObject is returned by Update on an entity whose table
// declares an OptimisticLock column when no row matches both the PK
// and the supplied version — another transaction has bumped the
// version, or the row was deleted between read and write.
var ErrStaleObject = errors.New("drops/pg: stale object — optimistic-lock version mismatch")

// Get fetches the row addressed by key. Returns ErrNoRows if no row
// matches, and [ErrKeyArity] if the number of values does not match
// the table's primary key.
//
// key is variadic to carry a composite primary key, whose values are
// positional and follow [Entity.PKs] — the table's declaration order.
// A single-column key reads exactly as it always did:
//
//	u, err := UserEntity.Get(db, ctx, 42)
//	m, err := MembershipEntity.Get(db, ctx, orgID, userID)
//
// When a cache is attached via WithCache, Get serves hits from the
// cache and dedupes concurrent cache misses via single-flight so a
// thundering herd resolves to one DB query. An entity whose rows are
// scoped — by a tenant axis, a guard, or a context filter on the table
// — skips that path entirely: the cache is keyed by primary key alone,
// so answering from it would hand one caller a row another caller
// cached, without a statement ever being sent. See hasRowScope.
func (e *Entity[T]) Get(db *DB, ctx context.Context, key ...any) (T, error) {
	pred, err := e.pkPredicate(key)
	if err != nil {
		return *new(T), err
	}
	ctx, cancel := e.budgetCtx(ctx)
	defer cancel()
	if e.cache != nil && !e.hasRowScope() {
		return e.getCached(db, ctx, key, pred)
	}
	var out T
	if e.fastScan != nil {
		err := e.scanOneFast(ctx, db.Select(e.fastCols...).From(e.table).Where(pred), &out)
		return out, err
	}
	err = db.Find(e.table).Where(pred).One(ctx, &out)
	return out, err
}

// getCached is the cache-aware implementation of Get.
func (e *Entity[T]) getCached(db *DB, ctx context.Context, pkValues []any, pred drops.Expression) (T, error) {
	var out T
	key := e.pkKey(pkValues)

	// 1. Cache lookup.
	if hit, err := e.cache.readPK(ctx, key, &out); err == nil && hit {
		return out, nil
	}
	// 2. Single-flight to dedupe concurrent misses on the same key.
	v, err := e.cache.sf.do(key, func() (any, error) {
		// Re-check the cache: another caller may have populated it
		// while we were queued.
		var t T
		if hit, err := e.cache.readPK(ctx, key, &t); err == nil && hit {
			return t, nil
		}
		var err error
		if e.fastScan != nil {
			err = e.scanOneFast(ctx, db.Select(e.fastCols...).From(e.table).Where(pred), &t)
		} else {
			err = db.Find(e.table).Where(pred).One(ctx, &t)
		}
		if err != nil {
			return t, err
		}
		_ = e.cache.writeKey(ctx, key, t)
		return t, nil
	})
	if err != nil {
		return out, err
	}
	return v.(T), nil
}

// scanOneFast runs sel and decodes the first row via fastScan.
// Returns ErrNoRows when sel produces no rows.
func (e *Entity[T]) scanOneFast(ctx context.Context, sel *SelectBuilder, dest *T) error {
	rows, err := sel.Rows(ctx)
	if err != nil {
		return err
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return err
		}
		return ErrNoRows
	}
	if err := e.fastScan(rows, dest); err != nil {
		return err
	}
	return rows.Err()
}

// scanAllFast runs sel and appends every row to dest via fastScan.
func (e *Entity[T]) scanAllFast(db *DB, ctx context.Context, sel *SelectBuilder, dest *[]T) error {
	rows, err := sel.Rows(ctx)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var r T
		if err := e.fastScan(rows, &r); err != nil {
			return err
		}
		*dest = append(*dest, r)
	}
	return rows.Err()
}

// CreateMany INSERTs every row in rs in a single statement. Compared
// to looping over Create, this batches the round-trip cost — large
// payloads stay one network hop. RETURNING is not used (refreshing N
// rows is rarely what callers want), so generated PKs and
// hook-supplied values do not flow back into rs; use Create when you
// need the post-INSERT row.
//
// The tenant is stamped onto every row first, exactly as Create stamps
// its one row, and rs is written through: a row whose tenant field is
// zero comes back carrying the ctx tenant. This is not a convenience.
// A tenant-scoped entity that skipped the stamp bound tenantId = 0 for
// the whole batch — a value no tenant predicate matches, so the rows
// landed outside every tenant including the one that wrote them,
// invisible to the very next SELECT and reported as a successful
// insert. Missing the ctx tenant is [ErrTenantMissing] and no
// statement at all; a row carrying a different tenant than ctx is
// [ErrTenantMismatch]. Either aborts the whole batch before the first
// binding is collected, because a partially stamped INSERT is a batch
// nobody can reason about.
func (e *Entity[T]) CreateMany(db *DB, ctx context.Context, rs []T) (drops.Result, error) {
	if len(rs) == 0 {
		return nil, ErrNoRowsToInsert
	}
	for i := range rs {
		if err := e.stampTenant(ctx, &rs[i]); err != nil {
			return nil, err
		}
		if err := e.runValidators(&rs[i]); err != nil {
			return nil, err
		}
	}
	ins := db.Insert(e.table)
	for i := range rs {
		v := reflect.ValueOf(&rs[i]).Elem()
		ins.Row(e.collectInsertBindings(v)...)
	}
	return ins.Exec(ctx)
}

// UpsertMany INSERTs rs and, on PK conflict, updates every non-PK
// column with the new row's value (ON CONFLICT (pk) DO UPDATE SET
// col = EXCLUDED.col). Returns the underlying Result so callers can
// inspect rows-affected; row values are not refreshed.
//
// Useful for idempotent ingestion: the same set of rows can be
// replayed safely without producing duplicates.
//
// The tenant is stamped onto every row before binding, on the same
// terms as [Entity.CreateMany]: no tenant on ctx is [ErrTenantMissing]
// and no statement, a disagreeing row is [ErrTenantMismatch].
//
// A tenant-scoped entity also changes the shape of the conflict
// branch, and the reasoning is worth spelling out because the
// straightforward version is a cross-tenant write. A primary key is
// unique across the whole table, not per tenant, so the row an INSERT
// collides with may well belong to somebody else. Setting every non-PK
// column from EXCLUDED then rewrites that row's data *and* its
// "tenantId", which is to say tenant A takes ownership of tenant B's
// row by guessing an id — silently, reported as one row affected.
// Refusing is the only defensible answer: the tenant column is left
// out of the SET list, so a row's owner is never rewritten by an
// upsert, and the update is gated on WHERE <tenant> = EXCLUDED.<tenant>
// so a collision with another tenant's row updates nothing at all
// rather than overwriting its data. Moving a row between tenants is a
// deliberate act and belongs in a statement that says so.
//
// The caller sees that refusal as a shortfall in rows-affected, which
// is the most an ON CONFLICT branch can report — SQL has no way to
// raise from a WHERE that did not match. Ingestion that must know
// whether every row landed should compare the count.
//
// That shape is not applied here any more, and the difference is the
// point rather than a tidy-up: it is applied by the builder, to every
// INSERT into a table that named a tenant column — see
// [InsertBuilder.ToSQLCtx]. This method used to be the only place the
// reasoning existed, so db.Insert(t).OnConflictUpdate(pk).Set(...),
// the same upsert written by hand, rewrote another tenant's row and
// its owner. Two places building the same gate would eventually
// disagree, and the way they disagree is that one of them stops being
// applied to a path somebody added later.
//
// The gate needs a tenant column to name, so it reaches the tables
// that named one — with [Entity.ScopeByTenant] or with
// [Table.ScopeWritesByTenant]. A table scoped only by
// [Table.ContextFilter] keeps the plain conflict branch: the filter is
// a predicate, and an INSERT has no WHERE for it to reach.
func (e *Entity[T]) UpsertMany(db *DB, ctx context.Context, rs []T) (drops.Result, error) {
	if len(rs) == 0 {
		return nil, ErrNoRowsToInsert
	}
	for i := range rs {
		if err := e.stampTenant(ctx, &rs[i]); err != nil {
			return nil, err
		}
		if err := e.runValidators(&rs[i]); err != nil {
			return nil, err
		}
	}
	ins := db.Insert(e.table)
	for i := range rs {
		v := reflect.ValueOf(&rs[i]).Elem()
		ins.Row(e.collectInsertBindings(v)...)
	}
	keyCols := make([]ColRef, len(e.pks))
	for i, c := range e.pks {
		keyCols[i] = c
	}
	sets := make([]ColumnValue, 0, len(e.colFields))
	for _, cf := range e.colFields {
		if e.isKeyColumn(cf.col) {
			continue
		}
		sets = append(sets, &exprBinding{col: cf.col, expr: Excluded(cf.col)})
	}
	if len(sets) == 0 {
		// Every column is part of the key, so there is nothing a
		// conflict could rewrite even before the tenant axis has its
		// say. DO NOTHING says that in SQL; "DO UPDATE SET" with an
		// empty list is a syntax error the server would report
		// instead. The builder makes the same substitution when
		// dropping the tenant assignment is what empties the list.
		return ins.OnConflictDoNothing(keyCols...).Exec(ctx)
	}
	return ins.OnConflictUpdate(keyCols...).Set(sets...).Done().Exec(ctx)
}

// EntityQuery is the typed counterpart of FindBuilder — same shape,
// but its executors return ([]T, error) and (T, error) directly.
type EntityQuery[T any] struct {
	e  *Entity[T]
	fb *FindBuilder
}

// Stream iterates the matching rows one at a time, invoking fn for
// each. Memory stays bounded — Stream never buffers the full result
// set — which matters for batch jobs and exports. Returning an error
// from fn aborts the iteration and propagates the error to the
// caller. Eager-loaded relations are not supported in Stream
// (relation loaders need the populated parent slice).
func (q *EntityQuery[T]) Stream(ctx context.Context, fn func(*T) error) error {
	if q.fb.HasEagerLoads() {
		return errors.New("drops/pg: Stream is incompatible with eager-loaded relations; use Query.All instead")
	}
	fast := q.useFast()
	rows, err := q.fb.Select().Rows(ctx)
	if err != nil {
		return err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return err
	}
	var sample T
	fields := fieldMap(reflect.TypeOf(sample))
	for rows.Next() {
		var t T
		if fast {
			if err := q.e.fastScan(rows, &t); err != nil {
				return err
			}
		} else {
			if err := scanRowInto(rows, reflect.ValueOf(&t).Elem(), cols, fields); err != nil {
				return err
			}
		}
		if err := fn(&t); err != nil {
			return err
		}
	}
	return rows.Err()
}

// Create INSERTs r and refreshes it from the RETURNING row — useful
// for picking up generated PKs, server-side defaults, and hook-added
// values (e.g. createdAt = now()).
//
// Columns whose Go field is the zero value are omitted from the
// INSERT when the column has a declared DEFAULT or is the primary
// key, so the server fills them in. That inference cannot tell "not
// set" from "set to the zero value", so a field left at false, "" or
// the zero time on a column with a DEFAULT is stored as the default,
// silently. Three ways to say the value is meant, none of which
// guesses: mark the column with [Col.AlwaysInsert] so every Create
// binds it, make the field a pointer so a non-nil pointer to false is
// bound and a nil one is not, or name the columns for one call with
// [Entity.CreateCols].
func (e *Entity[T]) Create(db *DB, ctx context.Context, r *T) error {
	if err := e.stampTenant(ctx, r); err != nil {
		return err
	}
	if err := e.runValidators(r); err != nil {
		return err
	}
	v := reflect.ValueOf(r).Elem()
	bindings := e.collectInsertBindings(v)
	if len(bindings) == 0 {
		return errors.New("drops/pg: Create has nothing to insert")
	}
	return e.insertRow(db, ctx, r, bindings)
}

// CreateCols INSERTs r binding exactly cols, whatever their values —
// the zero-value skip rule Create applies is not consulted at all. It
// is the per-call form of [Col.AlwaysInsert], for the caller that
// knows which columns this particular write is responsible for:
//
//	err := UserEntity.CreateCols(db, ctx, &u, UserName, UserEmail, UserActive)
//
// Columns not named are left out of the statement entirely, so the
// database fills them from their DEFAULT — including a primary key
// from its sequence. Everything else behaves as Create does: the
// tenant is stamped, validators run, the row is refreshed from
// RETURNING, the audit row is written in the same transaction and the
// PK cache is populated.
//
// It returns an error, rather than skipping quietly, when a column is
// not on the entity's table or has no struct field bound to it: the
// caller has named a column this row cannot supply a value for, and
// writing the other ones would produce a row nobody asked for. A
// tenant-scoped entity must include its tenant column for the same
// reason — the row would otherwise land outside every tenant, visible
// to none of them.
func (e *Entity[T]) CreateCols(db *DB, ctx context.Context, r *T, cols ...ColRef) error {
	if len(cols) == 0 {
		return errors.New("drops/pg: CreateCols requires at least one column")
	}
	if err := e.stampTenant(ctx, r); err != nil {
		return err
	}
	if err := e.runValidators(r); err != nil {
		return err
	}
	v := reflect.ValueOf(r).Elem()
	bindings := make([]ColumnValue, 0, len(cols))
	tenantNamed := e.tenantCol == nil
	for _, ref := range cols {
		c := ref.col()
		if c.Table() != nil && c.Table().Name() != e.table.Name() {
			return fmt.Errorf("drops/pg: CreateCols on %s: column %q belongs to table %q, not %q",
				e.rowType.Name(), c.Name(), c.Table().Name(), e.table.Name())
		}
		field, ok := e.fieldFor(c)
		if !ok {
			return fmt.Errorf("drops/pg: CreateCols on %s: no struct field bound to column %q on table %q",
				e.rowType.Name(), c.Name(), e.table.Name())
		}
		if e.tenantCol != nil && c.key() == e.tenantCol.key() {
			tenantNamed = true
		}
		bindings = append(bindings, insertBinding(c, v.FieldByIndex(field).Interface()))
	}
	if !tenantNamed {
		return fmt.Errorf("drops/pg: CreateCols on %s: tenant column %q is missing from the column list; the row would be written outside every tenant",
			e.rowType.Name(), e.tenantCol.Name())
	}
	return e.insertRow(db, ctx, r, bindings)
}

// fieldFor resolves a column to its field-index path on T, comparing
// by column identity so a handle taken off an alias resolves to the
// same field as the declared one.
func (e *Entity[T]) fieldFor(c *Column) ([]int, bool) {
	for _, cf := range e.colFields {
		if cf.col.key() == c.key() {
			return cf.field, true
		}
	}
	return nil, false
}

// insertRow issues the INSERT for one row and refreshes r from the
// RETURNING clause. Create and CreateCols differ only in which
// bindings they hand over, so everything downstream of that decision
// — the audit transaction, the cache write — lives here once.
func (e *Entity[T]) insertRow(db *DB, ctx context.Context, r *T, bindings []ColumnValue) error {
	doCreate := func(tx *DB) error {
		ins := tx.Insert(e.table).Row(bindings...)
		for _, c := range e.table.Columns() {
			ins.Returning(c)
		}
		if err := ins.One(ctx, r); err != nil {
			return err
		}
		return e.recordAudit(tx, ctx, "create", r, e.pkValue(r))
	}
	var err error
	if e.audit != nil {
		err = db.InTx(ctx, doCreate)
	} else {
		err = doCreate(db)
	}
	if err != nil {
		return err
	}
	// Populate the PK cache with the freshly-inserted row so the next
	// Get hits immediately. A scoped entity writes nothing and clears
	// the key instead: it carries the primary key and no scope at all,
	// so the entry would be served to every other tenant and subject
	// that asks for this id. See hasRowScope for the invariant and
	// refreshPK for why the clearing is not optional.
	e.refreshPK(ctx, e.pkValuesOf(r), *r)
	return nil
}

// Update writes r's current field values to the row whose PK equals
// r's PK and refreshes r from the RETURNING row. ErrPKNotSet is
// returned if r's PK is the zero value.
//
// All non-PK columns mapped to fields on T are included in the SET
// list — the typical "blind UPDATE" semantics. Change-tracking is
// out of scope for now; callers needing finer control use db.Update
// directly.
//
// The tenant column is an axis, never an assignment: Create stamps it,
// Update stamps it, both refuse a mismatch, and neither ever takes the
// value from the struct as an instruction.
//
// On a tenant-scoped entity the tenant column is one of those non-key
// columns, so the row's own tenant is stamped from ctx before the
// assignments are taken. Without that an Update of a struct whose
// tenant field is zero — one built from a form, or from a decoded
// request body — would write that zero over a row it is otherwise
// allowed to touch, and hand it to no tenant at all; a struct carrying
// somebody else's tenant is [ErrTenantMismatch] rather than a
// transfer of ownership. Which row is addressed is a separate
// question, and the table's context filter answers it: the WHERE
// clause carries the ctx tenant like every other statement's.
//
// The stamp runs before the validators, as it does in Create, so a
// validator reading the tenant field sees the row as it will be
// written rather than as the caller happened to build it.
func (e *Entity[T]) Update(db *DB, ctx context.Context, r *T) error {
	if e.pkIsZero(r) {
		return ErrPKNotSet
	}
	if err := e.stampTenant(ctx, r); err != nil {
		return err
	}
	if err := e.runValidators(r); err != nil {
		return err
	}
	v := reflect.ValueOf(r).Elem()
	pred, err := e.pkPredicate(e.pkValuesOf(r))
	if err != nil {
		return err
	}
	doUpdate := func(tx *DB) error {
		upd := tx.Update(e.table)
		wroteSet := false
		for _, cf := range e.colFields {
			if e.isKeyColumn(cf.col) {
				continue
			}
			if e.versionCol != nil && cf.col.key() == e.versionCol.key() {
				continue
			}
			val := v.FieldByIndex(cf.field).Interface()
			var expr drops.Expression
			if cf.col.IsPII() {
				expr = PIIParam{Value: val}
			} else {
				expr = drops.Param{Value: val}
			}
			upd.Set(&exprBinding{col: cf.col, expr: expr})
			wroteSet = true
		}
		if e.versionCol != nil {
			upd.Set(&exprBinding{
				col: e.versionCol,
				expr: drops.ExprFunc(func(b *drops.Builder) {
					b.WriteIdent(e.versionCol.Name())
					b.WriteString(" + 1")
				}),
			})
			wroteSet = true
		}
		if !wroteSet && !e.table.hasUpdateHooks() {
			return errors.New("drops/pg: Update has no fields to set")
		}
		upd.Where(pred)
		if e.versionCol != nil {
			curVer := v.FieldByIndex(e.versionField).Interface()
			upd.Where(Eq(e.versionCol, curVer))
		}
		for _, c := range e.table.Columns() {
			upd.Returning(c)
		}
		upderr := upd.One(ctx, r)
		if upderr == ErrNoRows && e.versionCol != nil {
			return ErrStaleObject
		}
		if upderr != nil {
			return upderr
		}
		return e.recordAudit(tx, ctx, "update", r, e.pkValue(r))
	}
	if e.audit != nil {
		err = db.InTx(ctx, doUpdate)
	} else {
		err = doUpdate(db)
	}
	if err == nil {
		// Same rule as insertRow: a scoped row must not enter a
		// namespace keyed by the primary key alone, and an entry a
		// scoped write leaves in place is an entry nothing will ever
		// correct. See hasRowScope and refreshPK.
		e.refreshPK(ctx, e.pkValuesOf(r), *r)
	}
	return err
}

// Save inserts r if its primary-key field is the zero value, or
// updates it otherwise. Compared to a single ON CONFLICT statement,
// this incurs an extra branch in Go but keeps the generated SQL
// straightforward; switch to a hand-written upsert when the
// race-window between the read and the write matters.
func (e *Entity[T]) Save(db *DB, ctx context.Context, r *T) error {
	// Validators run inside Create/Update, no double-call needed here.
	if e.pkIsZero(r) {
		return e.Create(db, ctx, r)
	}
	return e.Update(db, ctx, r)
}

// Delete removes the row whose primary key equals id. The table's
// DeleteHooks (e.g. SoftDelete) fire normally — so on a soft-deleted
// table this rewrites to UPDATE deletedAt = now() instead.
func (e *Entity[T]) Delete(db *DB, ctx context.Context, key ...any) (drops.Result, error) {
	pred, err := e.pkPredicate(key)
	if err != nil {
		return nil, err
	}
	var res drops.Result
	doDelete := func(tx *DB) error {
		r, derr := tx.Delete(e.table).Where(pred).Exec(ctx)
		if derr != nil {
			return derr
		}
		res = r
		return e.recordAudit(tx, ctx, "delete", nil, auditKey(key))
	}
	if e.audit != nil {
		err = db.InTx(ctx, doDelete)
	} else {
		err = doDelete(db)
	}
	if err == nil {
		e.invalidatePK(ctx, key)
	}
	return res, err
}

// auditKey renders a key for the audit trail's single rowID column.
// A composite key is joined so the trail stays queryable by a
// human-readable value rather than losing all but the first column.
func auditKey(values []any) any {
	if len(values) == 1 {
		return values[0]
	}
	parts := make([]string, len(values))
	for i, v := range values {
		parts[i] = fmt.Sprintf("%v", v)
	}
	return strings.Join(parts, "|")
}

// collectInsertBindings extracts column values from r. Columns whose
// Go field is the zero value are omitted when they have a DEFAULT or
// are the primary key — letting the DB fill them in.
//
// This is the one inference drops makes about intent, and it is worth
// naming what it costs. A Go zero value is indistinguishable from a
// field nobody assigned, so false on a column declared DEFAULT true
// is skipped and comes back true; so is "" on a column defaulting to
// 'pending', and a zero time.Time on one defaulting to now(). Nothing
// errors. The rule earns its place on createdAt and on a serial key,
// where the field genuinely has no value yet, and there are three
// ways to opt a value out of it:
//
//   - [Col.AlwaysInsert] on the column, when the zero value is always
//     meaningful there — the whole "active bool DEFAULT true" family;
//   - a pointer field, when "unset" and "the zero value" are two
//     different states of the same field. A nil pointer is the zero
//     value and is skipped, a non-nil one is not — so a *bool
//     pointing at false is bound, which is the convention
//     encoding/json established and Go developers already read
//     correctly;
//   - [Entity.CreateCols], which binds the named columns and consults
//     no rule at all.
//
// PII-flagged columns get their values wrapped in pg.PIIParam so any
// hook / tracer formatting them sees "<redacted>".
func (e *Entity[T]) collectInsertBindings(v reflect.Value) []ColumnValue {
	out := make([]ColumnValue, 0, len(e.colFields))
	for _, cf := range e.colFields {
		fv := v.FieldByIndex(cf.field)
		if fv.IsZero() && !cf.col.IsAlwaysInsert() &&
			(cf.col.HasDefault() || e.isKeyColumn(cf.col) || isImplicitDefault(cf.col)) {
			continue
		}
		out = append(out, insertBinding(cf.col, fv.Interface()))
	}
	return out
}

// insertBinding binds val to c, routing a PII-flagged column through
// PIIParam so a hook or tracer that formats the argument sees
// "<redacted>" instead of the value.
func insertBinding(c *Column, val any) ColumnValue {
	if c.IsPII() {
		return &exprBinding{col: c, expr: PIIParam{Value: val}}
	}
	return &exprBinding{col: c, expr: drops.Param{Value: val}}
}

// isImplicitDefault reports whether a column's SQL type implies a
// server-side default value (serial families). These columns aren't
// flagged by HasDefault() because their default lives in the type
// declaration, not in a DEFAULT clause.
func isImplicitDefault(c *Column) bool {
	switch c.Type().TypeSQL() {
	case "serial", "bigserial", "smallserial":
		return true
	}
	return false
}

// ----------------------------------------------------------------------
// Querying
// ----------------------------------------------------------------------

// Query returns a typed query builder for ad-hoc Where / OrderBy /
// Limit / Offset / With chains that scan into T or []T.
//
// When the entity is tenant-scoped the caller must use the ctx-aware
// All / One / Stream methods so the tenant predicate can be
// injected; using the builder methods without a ctx-bound tenant
// returns ErrTenantMissing.
func (e *Entity[T]) Query(db *DB) *EntityQuery[T] {
	return &EntityQuery[T]{e: e, fb: db.Find(e.table)}
}

// Where appends predicates joined by AND.
func (q *EntityQuery[T]) Where(preds ...drops.Expression) *EntityQuery[T] {
	q.fb.Where(preds...)
	return q
}

// OrderBy appends ORDER BY expressions.
func (q *EntityQuery[T]) OrderBy(exprs ...drops.Expression) *EntityQuery[T] {
	q.fb.OrderBy(exprs...)
	return q
}

// Limit sets the LIMIT.
func (q *EntityQuery[T]) Limit(n int64) *EntityQuery[T] { q.fb.Limit(n); return q }

// Offset sets the OFFSET.
func (q *EntityQuery[T]) Offset(n int64) *EntityQuery[T] { q.fb.Offset(n); return q }

// With eager-loads the named relations (see FindBuilder.With).
func (q *EntityQuery[T]) With(names ...string) *EntityQuery[T] {
	q.fb.With(names...)
	return q
}

// WithRel eager-loads a relation with per-edge configuration.
func (q *EntityQuery[T]) WithRel(name string, fn func(*RelConfig)) *EntityQuery[T] {
	q.fb.WithRel(name, fn)
	return q
}

// Load eager-loads relations given as handles — see [FindBuilder.Load]
// for why both this and the string-taking With exist.
func (q *EntityQuery[T]) Load(rels ...*Relation) *EntityQuery[T] {
	q.fb.Load(rels...)
	return q
}

// LoadRel eager-loads a relation by handle with per-edge
// configuration.
func (q *EntityQuery[T]) LoadRel(rel *Relation, fn func(*RelConfig)) *EntityQuery[T] {
	q.fb.LoadRel(rel, fn)
	return q
}

// Unscoped opts out of the table's DEFAULT filters for this query —
// the declaration-time ones, a soft-delete guard above all. Without it
// a soft-deleted row is unreachable through the entity at all, which
// makes an audit or a restore flow impossible to write.
//
// It does NOT drop the table's context filters: the tenant axis and the
// authorization guard survive it, and a ctx with no tenant is still
// refused. That is a deliberate difference from
// [SelectBuilder.Unscoped], which is statement-wide, and it is the
// difference the four dialects state in the same words. The two lists
// are not the same kind of thing — a default filter is a default scope,
// a context filter is a row-visibility boundary — and the failures of
// conflating them are not symmetric. Widening a default scope when the
// caller asked to widen it costs nothing. Dropping the boundary hands
// this request every tenant's rows, or every subject's, and it does so
// on the one method a caller reaches for while thinking about
// soft-deleted rows rather than about tenancy.
//
// A query that genuinely has to span tenants is written on the raw
// builder, db.Select().From(t).Unscoped(), where a reviewer reading the
// call sees the whole of what was given up.
func (q *EntityQuery[T]) Unscoped() *EntityQuery[T] {
	q.fb.Select().unscopeDefaults()
	return q
}

// All executes the query and returns the matching rows as a typed
// slice. Uses the fast-scan path (zero reflection) when available
// and no eager-loaded relations are queued. When the entity has a
// cache attached and the query has no eager-loaded relations, the
// result is cached under sha256(SQL+args) with the cache's TTL.
func (q *EntityQuery[T]) All(ctx context.Context) ([]T, error) {
	ctx, cancel := q.e.budgetCtx(ctx)
	defer cancel()
	// Project before anything renders the statement: the budget's
	// argument count and the cache key are both taken from the SQL,
	// and they have to describe the statement that actually runs.
	fast := q.useFast()
	if q.e.budget.MaxRows > 0 {
		// Apply the row-cap LIMIT before rendering. Honour the
		// user's tighter Limit by leaving it alone.
		applyBudgetLimit(q.fb.Select(), q.e.budget.MaxRows)
	}
	if q.e.budget.MaxArgs > 0 {
		// Rendered with the ctx: the tenant axis and the guard bind
		// arguments too, and a budget that counted only the ones the
		// caller wrote would pass a statement the server rejects.
		_, args, err := q.fb.Select().ToSQLCtx(ctx)
		if err != nil {
			return nil, err
		}
		if err := q.e.checkArgs(args); err != nil {
			return nil, err
		}
	}
	if q.cacheable() {
		return q.allCached(ctx, fast)
	}
	if fast {
		var out []T
		err := q.e.scanAllFast(q.fb.db, ctx, q.fb.Select(), &out)
		return out, err
	}
	var out []T
	err := q.fb.All(ctx, &out)
	return out, err
}

// One executes the query and returns the first matching row. Returns
// ErrNoRows if the query produces no rows. Honours the entity cache
// the same way All does.
func (q *EntityQuery[T]) One(ctx context.Context) (T, error) {
	ctx, cancel := q.e.budgetCtx(ctx)
	defer cancel()
	fast := q.useFast()
	if q.cacheable() {
		return q.oneCached(ctx, fast)
	}
	if fast {
		var out T
		err := q.e.scanOneFast(ctx, q.fb.Select(), &out)
		return out, err
	}
	var out T
	err := q.fb.One(ctx, &out)
	return out, err
}

// cacheable reports whether this query can be served from cache —
// the entity must have a cache, and the query must not pull in
// eager-loaded relations (those need the reflection-populated slice
// for stitching).
//
// A scoped entity stays cacheable, unlike Get: the key is built from
// the statement the ctx resolves to, so the tenant and the subject are
// part of it and two tenants asking the same question get two entries.
func (q *EntityQuery[T]) cacheable() bool {
	return q.e.cache != nil && !q.fb.HasEagerLoads()
}

// useFast reports whether this query can hand its rows to the
// generated scanner and, when it can, projects the statement onto the
// column list that scanner was generated for.
//
// Deciding and projecting are one call on purpose. The fast scanner
// reads by position, so the only statement it may consume is one this
// entity chose the columns of; splitting the two invites a path that
// takes the scanner and forgets the projection, which is the
// SELECT * bug the column list exists to close.
func (q *EntityQuery[T]) useFast() bool {
	if q.e.fastScan == nil || q.fb.HasEagerLoads() {
		return false
	}
	q.e.projectFast(q.fb.Select())
	return true
}

func (q *EntityQuery[T]) allCached(ctx context.Context, fast bool) ([]T, error) {
	sql, args, err := q.fb.Select().ToSQLCtx(ctx)
	if err != nil {
		return nil, err
	}
	key := queryKey(q.e.table.Name(), sql, args)
	var out []T
	if hit, err := q.e.cache.readPK(ctx, key, &out); err == nil && hit {
		return out, nil
	}
	v, err := q.e.cache.sf.do(key, func() (any, error) {
		var hits []T
		if hit, err := q.e.cache.readPK(ctx, key, &hits); err == nil && hit {
			return hits, nil
		}
		var rs []T
		var rerr error
		if fast {
			rerr = q.e.scanAllFast(q.fb.db, ctx, q.fb.Select(), &rs)
		} else {
			rerr = q.fb.All(ctx, &rs)
		}
		if rerr != nil {
			return rs, rerr
		}
		_ = q.e.cache.writeKey(ctx, key, rs)
		return rs, nil
	})
	if err != nil {
		return out, err
	}
	return v.([]T), nil
}

func (q *EntityQuery[T]) oneCached(ctx context.Context, fast bool) (T, error) {
	var out T
	sql, args, err := q.fb.Select().ToSQLCtx(ctx)
	if err != nil {
		return out, err
	}
	key := queryKey(q.e.table.Name(), sql, args) + ":one"
	if hit, err := q.e.cache.readPK(ctx, key, &out); err == nil && hit {
		return out, nil
	}
	v, err := q.e.cache.sf.do(key, func() (any, error) {
		var t T
		if hit, err := q.e.cache.readPK(ctx, key, &t); err == nil && hit {
			return t, nil
		}
		var rerr error
		if fast {
			rerr = q.e.scanOneFast(ctx, q.fb.Select(), &t)
		} else {
			rerr = q.fb.One(ctx, &t)
		}
		if rerr != nil {
			return t, rerr
		}
		_ = q.e.cache.writeKey(ctx, key, t)
		return t, nil
	})
	if err != nil {
		return out, err
	}
	return v.(T), nil
}
