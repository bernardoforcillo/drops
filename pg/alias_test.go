package pg_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/bernardoforcillo/drops"
	"github.com/bernardoforcillo/drops/pg"
)

// Aliasing is a query-scope rename: a handle taken off (*Table).As has
// to render with the alias and mean the same column everywhere else.
// The tests below split along that seam — the first group checks the
// rendering, the rest check that identity survived it.

// ----------------------------------------------------------------------
// Rendering: the alias reaches the columns.
// ----------------------------------------------------------------------

func TestAliasColumnsQualifyByAlias(t *testing.T) {
	u := users.As("u")
	checkExpr(t, u.Col("id"), `"u"."id"`)
	// The declared handle is untouched — both sides of a self-join
	// have to be addressable at once.
	checkExpr(t, userID, `"users"."id"`)
	if u.Col("id") == userID.Column {
		t.Error("alias must hand out its own column handle, not the declared one")
	}
}

// A self-join is the whole reason As exists, and it cannot be written
// unless the alias's columns qualify with the alias.
func TestAliasSelfJoinRenders(t *testing.T) {
	db := pg.New(nil)
	mgr := pg.NewTable("staff")
	staffID := pg.Add(mgr, pg.BigSerial("id").PrimaryKey())
	staffBoss := pg.Add(mgr, pg.BigInt("bossId"))

	m := mgr.As("m")
	q := db.Select(mgr.Col("id"), m.Col("id")).
		From(mgr).
		Join(m, pg.Eq(staffBoss, m.Col("id"))).
		Where(staffID.Gt(0))
	check(t, q,
		`SELECT "staff"."id", "m"."id" FROM "staff" `+
			`INNER JOIN "staff" AS "m" ON ("staff"."bossId" = "m"."id") `+
			`WHERE ("staff"."id" > $1)`,
		int64(0),
	)
}

// As on an already-aliased table chains the origin to the declared
// column. Were it to chain to the intermediate copy, the second alias
// would be a stranger to the table again.
func TestAliasOfAliasChainsToDeclaredColumn(t *testing.T) {
	db := pg.New(nil)
	tbl := pg.NewTable("chain")
	pg.Add(tbl, pg.BigSerial("id").PrimaryKey())
	name := pg.Add(tbl, pg.Text("name").NotNull())
	org := pg.Add(tbl, pg.Text("org"))

	v := tbl.As("u").As("v")
	ins := db.Insert(tbl).
		Row(name.Val("base"), org.Val("x")).
		Row(aliasBind[string](v.Col("name")).Val("twice"),
			aliasBind[string](v.Col("org")).Val("y"))
	sql, args := ins.ToSQL()
	if strings.Contains(sql, "DEFAULT") {
		t.Fatalf("doubly-aliased handles lost their bindings: %s", sql)
	}
	if len(args) != 4 {
		t.Fatalf("expected 4 args, got %d: %v (%s)", len(args), args, sql)
	}
}

// ----------------------------------------------------------------------
// Identity: INSERT column list and row alignment (inventory A1, A2).
// ----------------------------------------------------------------------

// aliasBind re-wraps a type-erased alias column as a typed handle, the
// way a caller outside the package reaches a value binding for a
// column it got from Table.Col.
func aliasBind[T any](c *pg.Column) *pg.Col[T] { return &pg.Col[T]{Column: c} }

// The column list is fixed by the first Row. A second row bound
// through an alias must land in the same columns — matching by handle
// instead fills the row with DEFAULT, which PostgreSQL accepts and
// which is silent on any nullable column.
func TestAliasInsertRowBoundThroughAliasKeepsItsValues(t *testing.T) {
	db := pg.New(nil)
	tbl := pg.NewTable("mix")
	pg.Add(tbl, pg.BigSerial("id").PrimaryKey())
	name := pg.Add(tbl, pg.Text("name"))
	org := pg.Add(tbl, pg.Text("org"))

	u := tbl.As("u")
	sql, args := db.Insert(tbl).
		Row(name.Val("real"), org.Val("set")).
		Row(aliasBind[string](u.Col("name")).Val("aliased"),
			aliasBind[string](u.Col("org")).Val("also")).
		ToSQL()

	if strings.Contains(sql, "DEFAULT") {
		t.Errorf("second row was filled with DEFAULT instead of its bindings: %s", sql)
	}
	want := []any{"real", "set", "aliased", "also"}
	if len(args) != len(want) {
		t.Fatalf("got args %v, want %v (%s)", args, want, sql)
	}
	for i := range want {
		if args[i] != want[i] {
			t.Fatalf("got args %v, want %v (%s)", args, want, sql)
		}
	}
}

// The mirror image: the first row is the aliased one, so columnsOf
// builds the list from the alias's handles and the declared handles
// have to match it.
func TestAliasInsertColumnListFixedByAliasedRow(t *testing.T) {
	db := pg.New(nil)
	tbl := pg.NewTable("mix2")
	pg.Add(tbl, pg.BigSerial("id").PrimaryKey())
	name := pg.Add(tbl, pg.Text("name"))

	u := tbl.As("u")
	sql, args := db.Insert(tbl).
		Row(aliasBind[string](u.Col("name")).Val("first")).
		Row(name.Val("second")).
		ToSQL()
	if strings.Contains(sql, "DEFAULT") {
		t.Errorf("declared handle did not match the aliased column list: %s", sql)
	}
	if len(args) != 2 {
		t.Fatalf("got args %v (%s)", args, sql)
	}
}

// ----------------------------------------------------------------------
// Identity: hooks (inventory A3, A4).
// ----------------------------------------------------------------------

// Every shipped mixin closes over the declared handle. A caller who
// binds the same column through an alias must still win: the hook's
// Has has to answer true, or the column is named twice and PostgreSQL
// rejects the statement with 42701.
func TestAliasInsertHookSeesColumnBoundThroughAlias(t *testing.T) {
	db := pg.New(nil)
	tbl := pg.NewTable("hooked")
	pg.Add(tbl, pg.BigSerial("id").PrimaryKey())
	name := pg.Add(tbl, pg.Text("name"))
	touched := pg.Add(tbl, pg.Timestamp("touchedAt", true))

	declared := touched.Column
	tbl.OnInsert(pg.InsertHookFunc(func(ctx *pg.InsertHookCtx) {
		ctx.SetExpr(declared, drops.Raw("now()"))
	}))

	u := tbl.As("u")
	sql, _ := db.Insert(tbl).
		Row(name.Val("x"),
			aliasBind[any](u.Col("touchedAt")).Expr(drops.Raw("'2020-01-01'::timestamptz"))).
		ToSQL()
	if strings.Count(sql, `"touchedAt"`) != 1 {
		t.Errorf("hook re-added a column the caller had already bound: %s", sql)
	}
	if strings.Contains(sql, "now()") {
		t.Errorf("hook value overrode the caller's: %s", sql)
	}
}

func TestAliasUpdateHookSeesColumnBoundThroughAlias(t *testing.T) {
	db := pg.New(nil)
	tbl := pg.NewTable("hooked_upd")
	pg.Add(tbl, pg.BigSerial("id").PrimaryKey())
	name := pg.Add(tbl, pg.Text("name"))
	touched := pg.Add(tbl, pg.Timestamp("touchedAt", true))

	declared := touched.Column
	tbl.OnUpdate(pg.UpdateHookFunc(func(ctx *pg.UpdateHookCtx) {
		ctx.SetExpr(declared, drops.Raw("now()"))
	}))

	u := tbl.As("u")
	sql, _ := db.Update(tbl).
		Set(name.Val("x")).
		Set(aliasBind[any](u.Col("touchedAt")).Expr(drops.Raw("'2020-01-01'::timestamptz"))).
		ToSQL()
	if strings.Count(sql, `"touchedAt"`) != 1 {
		t.Errorf("hook produced a second assignment to a bound column: %s", sql)
	}
}

// Has is the one public pg API that takes a bare *Column and looks it
// up, so it is checked directly rather than only through its effect.
func TestAliasHookHasAcceptsEitherHandle(t *testing.T) {
	db := pg.New(nil)
	tbl := pg.NewTable("hook_has")
	pg.Add(tbl, pg.BigSerial("id").PrimaryKey())
	name := pg.Add(tbl, pg.Text("name"))

	u := tbl.As("u")
	var sawAliased, sawDeclared bool
	tbl.OnInsert(pg.InsertHookFunc(func(ctx *pg.InsertHookCtx) {
		sawDeclared = ctx.Has(name.Column)
		sawAliased = ctx.Has(u.Col("name"))
	}))
	_, _ = db.Insert(tbl).Row(name.Val("x")).ToSQL()
	if !sawDeclared {
		t.Error("Has(declared handle) answered false for a bound column")
	}
	if !sawAliased {
		t.Error("Has(alias handle) answered false for a bound column")
	}
}

// ----------------------------------------------------------------------
// Identity: Entity (inventory A5, A6).
// ----------------------------------------------------------------------

type aliasKeyed struct {
	A    int64  `drop:"a"`
	B    int64  `drop:"b"`
	Note string `drop:"note"`
}

// A table-level PRIMARY KEY is the case where the key columns and the
// column list come from two different places on the Table, so an alias
// is the first thing that makes them two different handles.
func aliasCompositeSchema() (*pg.Table, *pg.Entity[aliasKeyed]) {
	tbl := pg.NewTable("composite_alias")
	a := pg.Add(tbl, pg.BigInt("a").NotNull())
	b := pg.Add(tbl, pg.BigInt("b").NotNull())
	pg.Add(tbl, pg.Text("note"))
	tbl.PrimaryKey(a, b)
	return tbl, nil
}

func TestAliasEntityUpdateOmitsKeyColumns(t *testing.T) {
	tbl, _ := aliasCompositeSchema()
	ent := pg.NewEntity[aliasKeyed](tbl.As("u"))

	fd := &fakeDriver{handler: func(string, []any) (drops.Rows, error) {
		return &fakeRows{
			cols: []string{"a", "b", "note"},
			data: [][]any{{int64(1), int64(2), "n"}},
		}, nil
	}}
	db := pg.New(fd)
	row := aliasKeyed{A: 1, B: 2, Note: "n"}
	if err := ent.Update(db, context.Background(), &row); err != nil {
		t.Fatalf("Update: %v", err)
	}
	sql := fd.queries[0]
	set := sql[strings.Index(sql, " SET ")+5 : strings.Index(sql, " WHERE ")]
	if strings.Contains(set, `"a"`) || strings.Contains(set, `"b"`) {
		t.Errorf("key columns landed in the SET list: %s", sql)
	}
	// The predicate has to name the alias, since that is the only
	// relation the statement puts in scope. Checked against the WHERE
	// clause alone — RETURNING lists the alias's columns either way.
	where := sql[strings.Index(sql, " WHERE ")+7 : strings.Index(sql, " RETURNING ")]
	if !strings.Contains(where, `"u"."a"`) || !strings.Contains(where, `"u"."b"`) {
		t.Errorf("WHERE did not qualify with the alias: %s", sql)
	}
}

// A natural key with no DEFAULT used to be omitted from the INSERT
// only because isKeyColumn recognised it.
func TestAliasEntityCreateOmitsZeroNaturalKey(t *testing.T) {
	tbl, _ := aliasCompositeSchema()
	ent := pg.NewEntity[aliasKeyed](tbl.As("u"))

	fd := &fakeDriver{handler: func(string, []any) (drops.Rows, error) {
		return &fakeRows{
			cols: []string{"a", "b", "note"},
			data: [][]any{{int64(0), int64(0), "n"}},
		}, nil
	}}
	db := pg.New(fd)
	row := aliasKeyed{Note: "n"}
	if err := ent.Create(db, context.Background(), &row); err != nil {
		t.Fatalf("Create: %v", err)
	}
	sql := fd.queries[0]
	cols := sql[strings.Index(sql, "(")+1 : strings.Index(sql, ") VALUES")]
	if strings.Contains(cols, `"a"`) || strings.Contains(cols, `"b"`) {
		t.Errorf("zero-valued key columns were written explicitly: %s", sql)
	}
}

type aliasVersioned struct {
	ID   int64  `drop:"id"`
	Note string `drop:"note"`
	Ver  int32  `drop:"ver"`
}

// The optimistic-lock column is skipped in the field loop and then
// added once as "ver = ver + 1". Failing to recognise it assigns the
// column twice, which PostgreSQL rejects with 42601.
func TestAliasEntityUpdateAssignsVersionColumnOnce(t *testing.T) {
	tbl := pg.NewTable("versioned_alias")
	pg.Add(tbl, pg.BigSerial("id").PrimaryKey())
	pg.Add(tbl, pg.Text("note"))
	pg.Add(tbl, pg.Integer("ver").NotNull().Default("0").OptimisticLock())
	ent := pg.NewEntity[aliasVersioned](tbl.As("u"))

	fd := &fakeDriver{handler: func(string, []any) (drops.Rows, error) {
		return &fakeRows{
			cols: []string{"id", "note", "ver"},
			data: [][]any{{int64(1), "n", int32(2)}},
		}, nil
	}}
	db := pg.New(fd)
	row := aliasVersioned{ID: 1, Note: "n", Ver: 1}
	if err := ent.Update(db, context.Background(), &row); err != nil {
		t.Fatalf("Update: %v", err)
	}
	sql := fd.queries[0]
	set := sql[strings.Index(sql, " SET ")+5 : strings.Index(sql, " WHERE ")]
	if strings.Count(set, `"ver"`) != 2 {
		// One name for the assignment target, one inside "ver" + 1.
		t.Errorf("version column assigned more than once: %s", sql)
	}
}

// ----------------------------------------------------------------------
// Identity: tenant axis and cursor (inventory A7, A8).
// ----------------------------------------------------------------------

type aliasPaged struct {
	ID   int64  `drop:"id"`
	Name string `drop:"name"`
}

type aliasTenanted struct {
	ID    int64  `drop:"id"`
	OrgID int64  `drop:"orgId"`
	Name  string `drop:"name"`
}

// ScopeByTenant takes a ColRef, so an alias handle enters legally. It
// used to panic at declaration time — process start — on a schema that
// had loaded fine the moment before.
func TestAliasScopeByTenantAcceptsAliasHandle(t *testing.T) {
	tbl := pg.NewTable("tenanted_alias")
	pg.Add(tbl, pg.BigSerial("id").PrimaryKey())
	pg.Add(tbl, pg.BigInt("orgId").NotNull())
	pg.Add(tbl, pg.Text("name"))
	ent := pg.NewEntity[aliasTenanted](tbl)

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("ScopeByTenant panicked on an alias handle: %v", r)
		}
	}()
	ent = ent.ScopeByTenant(tbl.As("u").Col("orgId"))

	fd := &fakeDriver{handler: func(string, []any) (drops.Rows, error) {
		return &fakeRows{
			cols: []string{"id", "orgId", "name"},
			data: [][]any{{int64(1), int64(7), "n"}},
		}, nil
	}}
	db := pg.New(fd)
	ctx := pg.WithTenant(context.Background(), int64(7))
	if _, err := ent.Get(db, ctx, int64(1)); err != nil {
		t.Fatalf("Get: %v", err)
	}
	// The entity queries the un-aliased table, so the guard has to
	// qualify with the table name even though the alias handle named
	// the axis.
	if !strings.Contains(fd.queries[0], `"tenanted_alias"."orgId"`) {
		t.Errorf("tenant guard did not qualify with the queried relation: %s", fd.queries[0])
	}
}

// An ordering column is rendered, not just matched: it goes into the
// ORDER BY and into the cursor guard's row comparison. Accepting the
// alias handle without restating it as the entity's own leaves a
// statement that no error in Go complains about and PostgreSQL rejects
// with 42P01 — which is why the assertion here is on the SQL and not
// on err.
func TestAliasPageCursorAcceptsAliasHandle(t *testing.T) {
	tbl, ent := entUsersSchema()
	fd := &fakeDriver{handler: func(string, []any) (drops.Rows, error) {
		return &fakeRows{
			cols: []string{"id", "name", "email"},
			data: [][]any{
				{int64(1), "A", "a@x"},
				{int64(2), "B", "b@x"},
			},
		}, nil
	}}
	db := pg.New(fd)
	page, err := ent.Page(db).
		OrderBy(pg.Asc(tbl.As("u").Col("id"))).
		Limit(1).
		All(context.Background())
	if err != nil {
		t.Fatalf("Page: %v", err)
	}
	if !page.HasMore || page.NextCursor == "" {
		t.Fatalf("expected a cursor at the page boundary, got HasMore=%v cursor=%q",
			page.HasMore, page.NextCursor)
	}
	if strings.Contains(fd.queries[0], `"u"."id"`) {
		t.Errorf("ORDER BY names an alias the query has no FROM entry for: %s", fd.queries[0])
	}
	if !strings.Contains(fd.queries[0], `ORDER BY "users"."id" ASC`) {
		t.Errorf("ORDER BY did not qualify with the queried relation: %s", fd.queries[0])
	}

	// The guard the cursor builds is rendered too, and from the same
	// handles.
	fd.queries = nil
	if _, err := ent.Page(db).
		OrderBy(pg.Asc(tbl.As("u").Col("id"))).
		Limit(1).
		After(page.NextCursor).
		All(context.Background()); err != nil {
		t.Fatalf("second page: %v", err)
	}
	if strings.Contains(fd.queries[0], `"u"."id"`) {
		t.Errorf("cursor guard names the alias: %s", fd.queries[0])
	}
}

// The mirror case, and the one a caller hits by accident: the entity
// sits on an alias and the package-level column is the handle in
// scope at the call site.
func TestAliasPageCursorAcceptsDeclaredHandleOnAnAliasedEntity(t *testing.T) {
	tbl := pg.NewTable("paged_alias")
	id := pg.Add(tbl, pg.BigSerial("id").PrimaryKey())
	pg.Add(tbl, pg.Text("name"))
	ent := pg.NewEntity[aliasPaged](tbl.As("u"))

	fd := &fakeDriver{handler: func(string, []any) (drops.Rows, error) {
		return &fakeRows{
			cols: []string{"id", "name"},
			data: [][]any{{int64(1), "A"}, {int64(2), "B"}},
		}, nil
	}}
	db := pg.New(fd)
	page, err := ent.Page(db).OrderBy(pg.Asc(id)).Limit(1).All(context.Background())
	if err != nil {
		t.Fatalf("Page: %v", err)
	}
	if strings.Contains(fd.queries[0], `"paged_alias"."id" ASC`) {
		t.Errorf("ORDER BY named the un-aliased table against FROM ... AS \"u\": %s", fd.queries[0])
	}
	if !strings.Contains(fd.queries[0], `ORDER BY "u"."id" ASC`) {
		t.Errorf("ORDER BY did not qualify with the alias: %s", fd.queries[0])
	}
	if page.NextCursor == "" {
		t.Error("expected a cursor at the page boundary")
	}
}

// A column with no struct field can never yield a cursor value, so the
// page must not be fetched first and blamed on the boundary.
func TestAliasPageRejectsAnUnmappedOrderingColumnBeforeQuerying(t *testing.T) {
	tbl := pg.NewTable("paged_unmapped")
	pg.Add(tbl, pg.BigSerial("id").PrimaryKey())
	pg.Add(tbl, pg.Text("name"))
	spare := pg.Add(tbl, pg.Text("spare"))
	ent := pg.NewEntity[aliasPaged](tbl, pg.AllowUnmappedColumns("spare"))

	fd := &fakeDriver{handler: func(string, []any) (drops.Rows, error) {
		return &fakeRows{cols: []string{"id", "name"}}, nil
	}}
	db := pg.New(fd)
	if _, err := ent.Page(pg.New(fd)).OrderBy(pg.Asc(spare)).Limit(1).All(context.Background()); err == nil {
		t.Fatal("expected an error for an ordering column with no struct field")
	}
	if len(fd.queries) != 0 {
		t.Errorf("the query ran before the spec was checked: %v", fd.queries)
	}
	_ = db
}

// ----------------------------------------------------------------------
// Identity: DDL and snapshot (inventory A9, A10).
// ----------------------------------------------------------------------

// The key columns come from compositePK, the column definitions from
// Columns(). If the two stop agreeing the CREATE TABLE silently loses
// its PRIMARY KEY — and PostgreSQL accepts it.
func TestAliasCreateTableKeepsSingleColumnTableLevelKey(t *testing.T) {
	tbl := pg.NewTable("ddl_alias")
	id := pg.Add(tbl, pg.BigSerial("id"))
	pg.Add(tbl, pg.Text("name").NotNull())
	tbl.PrimaryKey(id)

	sql, _ := drops.String(pg.CreateTable(tbl.As("u")))
	if !strings.Contains(sql, `"id" bigserial PRIMARY KEY`) {
		t.Errorf("aliased table lost its PRIMARY KEY:\n%s", sql)
	}
}

// For a genuinely composite key the table-level PRIMARY KEY clause
// renders from the key list directly and so survives on its own. What
// key membership still decides is the inline UNIQUE: PostgreSQL would
// accept a redundant one, but it becomes a second index nobody asked
// for and a constraint the next Introspect reads back and diffs
// against.
func TestAliasCreateTableKeepsCompositeKeyMembership(t *testing.T) {
	tbl := pg.NewTable("ddl_alias2")
	a := pg.Add(tbl, pg.BigInt("a").NotNull())
	b := pg.Add(tbl, pg.BigInt("b").NotNull().Unique())
	pg.Add(tbl, pg.Text("note"))
	tbl.PrimaryKey(a, b)

	sql, _ := drops.String(pg.CreateTable(tbl.As("u")))
	if !strings.Contains(sql, `PRIMARY KEY ("a", "b")`) {
		t.Errorf("aliased table lost its composite PRIMARY KEY:\n%s", sql)
	}
	if strings.Contains(sql, "UNIQUE") {
		t.Errorf("UNIQUE on a key member should stay suppressed:\n%s", sql)
	}
}

// A snapshot is durable input to Diff and Push, so a key column
// recorded nullable makes every push emit a DROP NOT NULL the next
// push undoes.
func TestAliasSnapshotKeepsKeyMembership(t *testing.T) {
	tbl := pg.NewTable("snap_alias")
	id := pg.Add(tbl, pg.BigSerial("id"))
	pg.Add(tbl, pg.Text("name").NotNull())
	tbl.PrimaryKey(id)

	snap := pg.BuildSnapshot(pg.NewSchema(tbl.As("u")))
	ts := snap.Tables["public.snap_alias"]
	if ts == nil {
		for k := range snap.Tables {
			t.Logf("snapshot has table %q", k)
		}
		t.Fatal("snapshot has no entry for the aliased table")
	}
	col := ts.Columns["id"]
	if col == nil {
		t.Fatal("snapshot has no entry for the key column")
	}
	if !col.PrimaryKey {
		t.Error("snapshot lost PrimaryKey on the key column")
	}
	if !col.NotNull {
		t.Error("snapshot recorded the key column as nullable")
	}
}

// Table.PrimaryKey takes a ColRef, so the key can legally be declared
// with a handle the table's own Columns() never hands back — an alias
// handle is the everyday way to get one. The key set and the column
// list are then two different pointers for one column, and every
// consumer that pairs them has to see through that: the CREATE TABLE
// loses its PRIMARY KEY outright, the snapshot records the key column
// as an ordinary nullable one, and the Entity puts it in the SET list
// of every UPDATE.
func TestKeyDeclaredThroughAnAliasHandleStillKeysTheTable(t *testing.T) {
	tbl := pg.NewTable("declared_via_alias")
	pg.Add(tbl, pg.BigSerial("id"))
	pg.Add(tbl, pg.Text("name").NotNull())
	tbl.PrimaryKey(tbl.As("x").Col("id"))

	sql, _ := drops.String(pg.CreateTable(tbl))
	if !strings.Contains(sql, `"id" bigserial PRIMARY KEY`) {
		t.Errorf("CREATE TABLE lost its PRIMARY KEY:\n%s", sql)
	}

	ts := pg.BuildSnapshot(pg.NewSchema(tbl)).Tables["public.declared_via_alias"]
	if ts == nil {
		t.Fatal("snapshot has no entry for the table")
	}
	if col := ts.Columns["id"]; col == nil || !col.PrimaryKey || !col.NotNull {
		t.Errorf("snapshot did not record the key column as a key: %+v", col)
	}
}

type aliasNatural struct {
	Code string `drop:"code"`
	Note string `drop:"note"`
}

// The Entity side of the same divergence. A natural key with no
// DEFAULT is left out of the INSERT only because isKeyColumn
// recognises it; unrecognised, the zero value goes to the server as an
// empty string.
func TestKeyDeclaredThroughAnAliasHandleIsOmittedFromCreate(t *testing.T) {
	tbl := pg.NewTable("natural_via_alias")
	pg.Add(tbl, pg.Text("code").NotNull())
	pg.Add(tbl, pg.Text("note"))
	tbl.PrimaryKey(tbl.As("x").Col("code"))
	ent := pg.NewEntity[aliasNatural](tbl)

	fd := &fakeDriver{handler: func(string, []any) (drops.Rows, error) {
		return &fakeRows{
			cols: []string{"code", "note"},
			data: [][]any{{"generated", "n"}},
		}, nil
	}}
	row := aliasNatural{Note: "n"}
	if err := ent.Create(pg.New(fd), context.Background(), &row); err != nil {
		t.Fatalf("Create: %v", err)
	}
	cols := fd.queries[0][strings.Index(fd.queries[0], "(")+1 : strings.Index(fd.queries[0], ") VALUES")]
	if strings.Contains(cols, `"code"`) {
		t.Errorf("zero-valued key column was written explicitly: %s", fd.queries[0])
	}
}

// versionCol is nil on the overwhelming majority of tables, and the
// comparison that finds it now dereferences both sides.
func TestEntityUpdateWithoutAVersionColumn(t *testing.T) {
	_, ent := entUsersSchema()
	fd := &fakeDriver{handler: func(string, []any) (drops.Rows, error) {
		return &fakeRows{
			cols: []string{"id", "name", "email"},
			data: [][]any{{int64(1), "A", "a@x"}},
		}, nil
	}}
	row := entUser{ID: 1, Name: "A", Email: "a@x"}
	if err := ent.Update(pg.New(fd), context.Background(), &row); err != nil {
		t.Fatalf("Update on a table with no version column: %v", err)
	}
}

// ----------------------------------------------------------------------
// The alias is a separate table, not a second name for the same maps.
// ----------------------------------------------------------------------

func TestAliasDoesNotWriteThroughToTheBaseTable(t *testing.T) {
	base := pg.NewTable("isolated")
	id := pg.Add(base, pg.BigSerial("id").PrimaryKey())
	pg.Add(base, pg.Text("name"))
	child := pg.NewTable("isolated_child")
	childFK := pg.Add(child, pg.BigInt("parentId"))

	// The base has to carry one of each already: the lazily-created
	// maps are only shared once they exist, so a table with no checks
	// at all would hide the leak.
	base.AddCheck("on_base", "id > 0")
	base.AddUnique("uniq_on_base", id)
	base.AddPolicy(pg.NewPolicy("policy_on_base"))

	u := base.As("u")
	pg.NewRelations(u).HasMany("kids", child, u.Col("id"), childFK)
	u.AddCheck("only_on_alias", "id > 0")
	u.AddUnique("uniq_on_alias", u.Col("name"))
	u.AddPolicy(pg.NewPolicy("policy_on_alias"))
	pg.Add(u, pg.Text("extra"))

	if base.Relation("kids") != nil {
		t.Error("a relation declared on the alias leaked into the base table")
	}
	if _, ok := base.Checks()["only_on_alias"]; ok {
		t.Error("a CHECK declared on the alias leaked into the base table")
	}
	if _, ok := base.CompositeUniques()["uniq_on_alias"]; ok {
		t.Error("a UNIQUE declared on the alias leaked into the base table")
	}
	if _, ok := base.Policies()["policy_on_alias"]; ok {
		t.Error("a policy declared on the alias leaked into the base table")
	}
	if base.Col("extra") != nil {
		t.Error("a column added to the alias leaked into the base table")
	}
	if len(base.Columns()) != 2 {
		t.Errorf("base table column count changed: %d", len(base.Columns()))
	}
}

// The table-level slices are copied, not just re-sliced. A copy taken
// at full capacity lets an append through the alias land in the base
// table's spare capacity, and the base's next append overwrite it —
// three indexes leave exactly one spare slot for that to happen in.
func TestAliasSlicesDoNotShareSpareCapacity(t *testing.T) {
	base := pg.NewTable("cap_share")
	id := pg.Add(base, pg.BigSerial("id").PrimaryKey())
	for _, n := range []string{"i1", "i2", "i3"} {
		base.AddIndex(pg.NewIndex(n, base, id))
	}

	u := base.As("u")
	u.AddIndex(pg.NewIndex("on_alias", u, u.Col("id")))
	base.AddIndex(pg.NewIndex("on_base", base, id))

	if got := u.Indexes()[3].Name(); got != "on_alias" {
		t.Errorf("the base table's append overwrote the alias's index: got %q", got)
	}
	if len(base.Indexes()) != 4 || base.Indexes()[3].Name() != "on_base" {
		t.Errorf("base index list disturbed: %d entries, last %q",
			len(base.Indexes()), base.Indexes()[len(base.Indexes())-1].Name())
	}
}

// The hook lists and the default filters are SHARED with the base
// table rather than copied, so this is the same hazard read the other
// way round: an append through either handle has to be visible from
// both, and has to displace nothing already there.
//
// The sharing is the point. While these lists were copied, an alias was
// a snapshot of the table's scoping at the instant As was called — and
// an alias declared as a package-level var beside its table is taken
// before any init that scopes it, so it stayed unscoped for ever. Three
// entries of each are registered before the alias is taken, so the copy
// this test used to pin would have exactly one spare slot for a later
// append to collide in; each side then registers one more, against a
// column of its own so that "user values win" cannot hide a loss.
func TestAliasSharesHookAndFilterListsWithItsTable(t *testing.T) {
	base := pg.NewTable("cap_hooks")
	id := pg.Add(base, pg.BigSerial("id").PrimaryKey())
	viaAlias := pg.Add(base, pg.Text("viaAlias"))
	viaBase := pg.Add(base, pg.Text("viaBase"))

	mark := func(c *pg.Col[string], tag string) pg.InsertHook {
		return pg.InsertHookFunc(func(ctx *pg.InsertHookCtx) {
			ctx.SetExpr(c.Column, drops.Raw("'"+tag+"'"))
		})
	}
	markUpd := func(c *pg.Col[string], tag string) pg.UpdateHook {
		return pg.UpdateHookFunc(func(ctx *pg.UpdateHookCtx) {
			ctx.SetExpr(c.Column, drops.Raw("'"+tag+"'"))
		})
	}
	for i := 0; i < 3; i++ {
		base.OnInsert(pg.InsertHookFunc(func(*pg.InsertHookCtx) {}))
		base.OnUpdate(pg.UpdateHookFunc(func(*pg.UpdateHookCtx) {}))
		base.OnDelete(pg.DeleteHookFunc(func(*pg.DeleteBuilder) drops.Expression { return nil }))
		base.DefaultFilter(pg.IsNotNull(id))
	}

	// Registered THROUGH the alias, but over the DECLARED handles: the
	// list is shared, so the base table renders these too, and an
	// alias handle would qualify with a relation its statement never
	// names. That is the rule TenantFilter already states.
	u := base.As("u")
	u.OnInsert(mark(viaAlias, "alias"))
	u.OnUpdate(markUpd(viaAlias, "alias"))
	u.DefaultFilter(pg.Eq(viaAlias, "alias"))
	base.OnInsert(mark(viaBase, "base"))
	base.OnUpdate(markUpd(viaBase, "base"))
	base.DefaultFilter(pg.Eq(viaBase, "base"))

	db := pg.New(nil)
	// Both handles run both hooks: neither append displaced the other,
	// and the alias sees the registration that came after it.
	for _, tc := range []struct {
		name  string
		table *pg.Table
	}{{"alias", u}, {"base", base}} {
		t.Run(tc.name, func(t *testing.T) {
			sql, _ := db.Insert(tc.table).Row(id.Val(1)).ToSQL()
			for _, want := range []string{"'alias'", "'base'"} {
				if !strings.Contains(sql, want) {
					t.Errorf("INSERT lost the %s OnInsert: %s", want, sql)
				}
			}
			sql, _ = db.Update(tc.table).Set(id.Val(1)).ToSQL()
			for _, want := range []string{"'alias'", "'base'"} {
				if !strings.Contains(sql, want) {
					t.Errorf("UPDATE lost the %s OnUpdate: %s", want, sql)
				}
			}
		})
	}
	// A DefaultFilter registered through either handle reaches both,
	// and each renders it qualified with the relation ITS statement
	// names — the rename resolveFilterExprs applies.
	sql, _ := db.Select(id).From(u).ToSQL()
	for _, want := range []string{`"u"."viaAlias"`, `"u"."viaBase"`} {
		if !strings.Contains(sql, want) {
			t.Errorf("SELECT from the alias lost the filter on %s: %s", want, sql)
		}
	}
	sql, _ = db.Select(id).From(base).ToSQL()
	for _, want := range []string{`"cap_hooks"."viaAlias"`, `"cap_hooks"."viaBase"`} {
		if !strings.Contains(sql, want) {
			t.Errorf("SELECT from the base table lost the filter on %s: %s", want, sql)
		}
	}
}

// A composite FK's local columns belong to this table, so they move to
// the alias with everything else; the target's do not.
func TestAliasRebindsCompositeForeignKeyLocalColumns(t *testing.T) {
	target := pg.NewTable("cfk_target")
	tA := pg.Add(target, pg.BigInt("a").NotNull())
	tB := pg.Add(target, pg.BigInt("b").NotNull())
	target.PrimaryKey(tA, tB)

	src := pg.NewTable("cfk_src")
	sA := pg.Add(src, pg.BigInt("a").NotNull())
	sB := pg.Add(src, pg.BigInt("b").NotNull())
	src.ForeignKeyN([]pg.ColRef{sA, sB}, target, []pg.ColRef{tA, tB})

	fk := src.As("s").CompositeForeignKeys()[0]
	for _, c := range fk.Columns {
		if c.Table().Alias() != "s" {
			t.Errorf("local column %q was not rebound to the alias", c.Name())
		}
	}
	for _, c := range fk.TargetColumns {
		if c.Table().Alias() != "" {
			t.Errorf("target column %q should not carry this table's alias", c.Name())
		}
	}
	if src.CompositeForeignKeys()[0].Columns[0] != sA.Column {
		t.Error("rebinding the alias mutated the base table's composite FK")
	}
}

// The near side of a relation — the end that belongs to this table —
// moves to the alias so an eager load through the alias joins the
// relation it names. The far side stays where it is.
func TestAliasRelationRebindsOnlyTheNearSide(t *testing.T) {
	parent := pg.NewTable("rel_parent")
	parentID := pg.Add(parent, pg.BigSerial("id").PrimaryKey())
	child := pg.NewTable("rel_child")
	childFK := pg.Add(child, pg.BigInt("parentId"))
	pg.NewRelations(parent).HasMany("kids", child, parentID, childFK)

	rel := parent.As("p").Rel("kids")
	if rel.ParentKey.Table().Alias() != "p" {
		t.Errorf("near side was not rebound to the alias: %q", rel.ParentKey.Table().Alias())
	}
	if rel.ChildKey != childFK.Column {
		t.Error("far side must be left alone")
	}
	if parent.Rel("kids").ParentKey != parentID.Column {
		t.Error("rebinding the alias mutated the base table's relation")
	}
}

// A self-referential relation names this table on both ends. Only the
// From side is the aliased one — rebinding both would erase the
// distinction the alias exists to draw.
func TestAliasSelfReferentialRelationRebindsOneEnd(t *testing.T) {
	tbl := pg.NewTable("tree")
	id := pg.Add(tbl, pg.BigSerial("id").PrimaryKey())
	parentID := pg.Add(tbl, pg.BigInt("parentId"))
	pg.NewRelations(tbl).HasMany("children", tbl, id, parentID)

	rel := tbl.As("t").Rel("children")
	if rel.ParentKey.Table().Alias() != "t" {
		t.Error("the From-side key should carry the alias")
	}
	if rel.ChildKey.Table().Alias() != "" {
		t.Error("the To-side key should stay on the un-aliased table")
	}
}

// As is a snapshot, so a relation declared afterwards reaches the base
// table and not the alias. The panic has to say which handle it is
// talking about: from an empty relation list, "you aliased too early"
// and "you never declared it" look the same.
func TestAliasRelPanicNamesTheAlias(t *testing.T) {
	tbl := pg.NewTable("late_relations")
	id := pg.Add(tbl, pg.BigSerial("id").PrimaryKey())
	child := pg.NewTable("late_children")
	childFK := pg.Add(child, pg.BigInt("parentId"))

	u := tbl.As("u")
	pg.NewRelations(tbl).HasMany("kids", child, id, childFK)

	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("a relation declared after As should not reach the alias")
		}
		msg, _ := r.(string)
		if !strings.Contains(msg, `alias "u"`) {
			t.Errorf("panic does not name the alias: %v", r)
		}
	}()
	u.Rel("kids")
}

// Every other identifier entry point in pg fails loudly at
// declaration; As used to accept anything and render AS "".
func TestAliasRejectsEmptyIdentifier(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Error("As(\"\") should panic like every other identifier entry point")
		}
	}()
	users.As("")
}

// ----------------------------------------------------------------------
// Scoping declared AFTER the alias was taken
// ----------------------------------------------------------------------

// An alias used to be a snapshot of its table's SCOPING as well as of
// its shape, and the ordering that loses is the one this package
// invites: Go initialises package-level variables before it runs init,
// so an alias declared beside its table is taken before any init or
// constructor that scopes the table. The alias stayed unscoped for ever
// — and the worst reachable shape was a DELETE, rendered on
// context.Background() through the exported API with no predicate at
// all and no refusal:
//
//	DELETE FROM "users" AS "u"
//
// Every kind of automatic predicate is covered below because each is
// registered by a different call and a fix for one is not a fix for the
// others: DefaultFilter is render-time, ContextFilter is request-time,
// Entity.ScopeByTenant and Entity.AuthorizeWith go through the keyed
// registration, and ScopeWritesByTenant is a column rather than a
// predicate.

// aliasLateScoped builds a table, takes an alias of it, and only then
// runs decl — the ordering a package-level var pair produces.
func aliasLateScoped(name string, decl func(*pg.Table)) (base, alias *pg.Table) {
	base = pg.NewTable(name)
	pg.Add(base, pg.BigSerial("id").PrimaryKey())
	pg.Add(base, pg.BigInt("orgId").NotNull())
	pg.Add(base, pg.Text("name"))
	alias = base.As("u")
	decl(base)
	return base, alias
}

type aliasLate struct {
	ID    int64  `drop:"id"`
	OrgID int64  `drop:"orgId"`
	Name  string `drop:"name"`
}

// The severe case, stated on its own: a DELETE against the alias of a
// table scoped after the alias was taken must not render, because what
// it rendered was an unpredicated cross-tenant delete.
func TestAliasDeleteRefusesWhenTheTableWasScopedLater(t *testing.T) {
	_, u := aliasLateScoped("late_delete", func(base *pg.Table) {
		base.ContextFilter(pg.TenantFilter(base.Col("orgId")))
	})

	db := pg.New(nil)
	sql, _, err := db.Delete(u).ToSQLCtx(context.Background())
	if !errors.Is(err, pg.ErrTenantMissing) {
		t.Fatalf("got = %v (sql %q), want %v", err, sql, pg.ErrTenantMissing)
	}
	if sql != "" {
		t.Errorf("got sql = %q, want no statement at all", sql)
	}
	checkCtx(t, pg.WithTenant(context.Background(), int64(7)), db.Delete(u),
		`DELETE FROM "late_delete" AS "u" WHERE ("u"."orgId" = $1)`, int64(7))
}

// The same table, the four ways a predicate is declared on it, across
// the three statements that carry one.
func TestAliasCarriesScopingDeclaredAfterAs(t *testing.T) {
	db := pg.New(nil)
	subject := int64(11)
	tenant := int64(7)

	tests := []struct {
		name string
		decl func(*pg.Table)
		ctx  context.Context
		// want is the predicate the alias's statements must carry.
		want string
		arg  any
		// missing is the error a ctx without the value must produce,
		// or nil for a render-time filter that needs no ctx.
		missing error
	}{
		{
			name: "Table.ContextFilter",
			decl: func(base *pg.Table) {
				base.ContextFilter(pg.TenantFilter(base.Col("orgId")))
			},
			ctx:     pg.WithTenant(context.Background(), tenant),
			want:    `("u"."orgId" = $?)`,
			arg:     tenant,
			missing: pg.ErrTenantMissing,
		},
		{
			name: "Entity.ScopeByTenant",
			decl: func(base *pg.Table) {
				pg.NewEntity[aliasLate](base).ScopeByTenant(base.Col("orgId"))
			},
			ctx:     pg.WithTenant(context.Background(), tenant),
			want:    `("u"."orgId" = $?)`,
			arg:     tenant,
			missing: pg.ErrTenantMissing,
		},
		{
			name: "Entity.AuthorizeWith",
			decl: func(base *pg.Table) {
				pg.NewEntity[aliasLate](base).
					AuthorizeWith(pg.OwnerGuard{Owner: base.Col("orgId")})
			},
			ctx:     pg.WithSubject(context.Background(), subject),
			want:    `("u"."orgId" = $?)`,
			arg:     subject,
			missing: pg.ErrSubjectMissing,
		},
		{
			name: "Table.DefaultFilter",
			decl: func(base *pg.Table) {
				base.DefaultFilter(pg.IsNull(base.Col("name")))
			},
			ctx:  context.Background(),
			want: `("u"."name" IS NULL)`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, u := aliasLateScoped("late_"+strings.ToLower(strings.ReplaceAll(tc.name, ".", "_")), tc.decl)
			uid := wbind[int64](u.Col("id"))

			stmts := map[string]ctxSQLable{
				"SELECT": db.Select(u.Col("id")).From(u),
				"UPDATE": db.Update(u).Set(uid.Val(1)),
				"DELETE": db.Delete(u),
			}
			for kind, q := range stmts {
				gotSQL, gotArgs, err := q.ToSQLCtx(tc.ctx)
				if err != nil {
					t.Fatalf("%s: ToSQLCtx: %v", kind, err)
				}
				if !strings.Contains(normalisePlaceholders(gotSQL), tc.want) {
					t.Errorf("%s: got = %q, want it to contain %q", kind, gotSQL, tc.want)
				}
				if tc.arg != nil && !containsArg(gotArgs, tc.arg) {
					t.Errorf("%s: args = %v, want them to bind %v", kind, gotArgs, tc.arg)
				}
			}
			if tc.missing == nil {
				return
			}
			// And with nothing on the ctx the alias refuses, rather
			// than sending the statement with the guard missing.
			for kind, q := range map[string]ctxSQLable{
				"SELECT": db.Select(u.Col("id")).From(u),
				"UPDATE": db.Update(u).Set(uid.Val(1)),
				"DELETE": db.Delete(u),
			} {
				sql, _, err := q.ToSQLCtx(context.Background())
				if !errors.Is(err, tc.missing) {
					t.Errorf("%s: got = %v (sql %q), want %v", kind, err, sql, tc.missing)
				}
				if sql != "" {
					t.Errorf("%s: got sql = %q, want no statement at all", kind, sql)
				}
			}
		})
	}
}

// The write-side half of the axis is a column rather than a predicate,
// so it is registered by a different call and was lost by the same
// snapshot: an INSERT through the alias stamped nothing and refused
// nothing, writing a row that belongs to no tenant.
func TestAliasStampsTenantDeclaredAfterAs(t *testing.T) {
	_, u := aliasLateScoped("late_insert", func(base *pg.Table) {
		base.ScopeWritesByTenant(base.Col("orgId"))
	})
	db := pg.New(nil)
	name := wbind[string](u.Col("name"))

	checkCtx(t, pg.WithTenant(context.Background(), int64(7)),
		db.Insert(u).Row(name.Val("a")),
		`INSERT INTO "late_insert" AS "u" ("name", "orgId") VALUES ($1, $2)`,
		"a", int64(7))

	sql, _, err := db.Insert(u).Row(name.Val("a")).ToSQLCtx(context.Background())
	if !errors.Is(err, pg.ErrTenantMissing) {
		t.Fatalf("got = %v (sql %q), want %v", err, sql, pg.ErrTenantMissing)
	}
	if sql != "" {
		t.Errorf("got sql = %q, want no statement at all", sql)
	}
}

// A soft-delete mixin applied after the alias was taken registers both
// a DefaultFilter and a DeleteHook, and losing the hook is the louder
// half: the DELETE stopped being rewritten into an UPDATE and removed
// the rows for good.
func TestAliasSoftDeleteAppliedAfterAsStillRewritesTheDelete(t *testing.T) {
	base := pg.NewTable("late_soft")
	pg.Add(base, pg.BigSerial("id").PrimaryKey())
	u := base.As("u")
	pg.ApplyMixins(base, &pg.SoftDeleteMixin{})

	db := pg.New(nil)
	check(t, db.Delete(u),
		`UPDATE "late_soft" AS "u" SET "deletedAt" = now() WHERE ("u"."deletedAt" IS NULL)`)
	// Unscoped is still the way to a hard DELETE, and it takes the
	// filter with it.
	check(t, db.Delete(u).Unscoped(), `DELETE FROM "late_soft" AS "u"`)
}
