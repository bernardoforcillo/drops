package pg_test

import (
	"context"
	"errors"
	"go/ast"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/bernardoforcillo/drops"
	"github.com/bernardoforcillo/drops/pg"
)

// ----------------------------------------------------------------------
// The RELATION census, enforced against the package source
//
//	Every relation a statement names carries that relation's scoping,
//	however the relation got into the statement.
//
// The four checks that came before are all about EXPRESSIONS — an
// operand must not be an opaque closure, a rendered expression list
// must not be invisible to the resolver, the resolver must dispatch on
// what a value can do. Each was built to see the shape that had just
// bitten, and each was blind to this one, because the resolver walks
// expressions and a table is not one of the things it walks. Two doors
// were open at once:
//
//   - FromExpr takes a drops.Expression and a *Table is one, so the
//     documented way to comma-join two tables handed the resolver a
//     RELATION on the expression path. It matched no arm, was rendered
//     as a bare name, and read every tenant's rows on a ctx with no
//     tenant at all.
//   - Table.As handed back a relation carrying a SNAPSHOT of its
//     table's scoping, so an alias taken before the table was scoped —
//     which is what a package-level var pair produces, since Go
//     initialises those before it runs init — named a relation nothing
//     would ever scope. It rendered DELETE FROM "users" AS "u".
//
// So the census has two halves, and they are the two ways a relation
// reaches a statement:
//
//   - a PARAMETER position on a statement builder that can accept a
//     *Table (relationEntryPoints, first half);
//   - a *Table-returning method, which hands a caller a second handle
//     on the same table to use later (second half).
//
// Both halves are enumerated from the package source, in both
// directions, so a case that no longer exists fails as loudly as a door
// that has no case — the pattern round 5 established.
//
// The VERDICT is derived rather than written down, which is the part
// that matters. Each case renders its statement and the check reads the
// rendered SQL for the relations it declares: if the scoped table is
// one of them, the statement must carry that table's tenant predicate
// or must have refused outright. If it is not — a table handed to
// Where, or to a select list, is an expression there and not a
// relation — the case asserts nothing, because that is round 6's
// invariant and not this one's. Nothing about which doors are relation
// doors is hand-listed, so a method that starts rendering its argument
// as a relation starts being checked as one on the same day.

// ----------------------------------------------------------------------
// Fixture
// ----------------------------------------------------------------------

// relFixture is the scenery every case builds on. Everything in it is
// unscoped except relFixture.scoped, so a tenant predicate in the
// rendered SQL can only have come from the relation under test.
type relFixture struct {
	db     *pg.DB
	plain  *pg.Table
	other  *pg.Table
	scoped *pg.Table
	id     *pg.Col[int64]
	oid    *pg.Col[int64]
	sid    *pg.Col[int64]
}

// relTenant is the tenant these cases put on ctx.
const relTenant = int64(31)

func relCtx() context.Context { return pg.WithTenant(context.Background(), relTenant) }

func newRelFixture(name string) *relFixture {
	f := &relFixture{
		db:     pg.New(nil),
		plain:  relPlain("rc_plain_" + name),
		other:  relPlain("rc_other_" + name),
		scoped: relScoped("rc_scoped_" + name),
	}
	f.id = wbind[int64](f.plain.Col("id"))
	f.oid = wbind[int64](f.other.Col("id"))
	f.sid = wbind[int64](f.scoped.Col("id"))
	return f
}

// late is the second half's fixture: it applies decl to a table that is
// NOT yet scoped, scopes it afterwards, and hands back whatever decl
// returned. That ordering is the whole defect — a handle taken before
// the axis was declared has to carry the axis anyway — and every case
// in the second half is built through it so no case can pass by taking
// its handle at a convenient moment.
func (f *relFixture) late(decl func(*pg.Table) *pg.Table) *pg.Table {
	base := pg.NewTable("rc_late_" + strings.ReplaceAll(f.plain.Name(), "rc_plain_", ""))
	pg.Add(base, pg.BigSerial("id").PrimaryKey())
	tenant := pg.Add(base, pg.BigInt("tenantId").NotNull())
	handle := decl(base)
	pg.ApplyMixins(base, &pg.SoftDeleteMixin{})
	base.ContextFilter(pg.TenantFilter(tenant))
	base.ScopeWritesByTenant(tenant)
	f.scoped = base
	return handle
}

// relationCase is one door a relation can come through, with the
// statement that puts the fixture's scoped table behind it.
//
// name is the censused door — "<receiver>.<method>", or a bare function
// name — optionally followed by a space and a note.
type relationCase struct {
	name  string
	build func(f *relFixture) ctxSQLable
}

// relationEntryPoints is the table the census enforces.
//
// The first half is a parameter position on a statement builder; the
// second is a handle a *Table hands out, taken through relFixture.late
// so that the table is scoped only afterwards.
func relationEntryPoints() []relationCase {
	return []relationCase{
		// ---- parameter positions -------------------------------------
		{"DB.Select", func(f *relFixture) ctxSQLable {
			return f.db.Select(f.scoped).From(f.plain)
		}},
		{"DB.Insert", func(f *relFixture) ctxSQLable {
			return f.db.Insert(f.scoped).Row(f.sid.Val(1))
		}},
		{"DB.Update", func(f *relFixture) ctxSQLable {
			return f.db.Update(f.scoped).Set(f.sid.Val(1))
		}},
		{"DB.Delete", func(f *relFixture) ctxSQLable {
			return f.db.Delete(f.scoped)
		}},
		{"DB.Find", func(f *relFixture) ctxSQLable {
			return f.db.Find(f.scoped).Select()
		}},
		{"SelectBuilder.From", func(f *relFixture) ctxSQLable {
			return f.db.Select(f.sid).From(f.scoped)
		}},
		{"SelectBuilder.FromExpr", func(f *relFixture) ctxSQLable {
			return f.db.Select(f.id).From(f.plain).FromExpr(f.scoped)
		}},
		{"SelectBuilder.Join", func(f *relFixture) ctxSQLable {
			return f.db.Select(f.id).From(f.plain).Join(f.scoped, nil)
		}},
		{"SelectBuilder.LeftJoin", func(f *relFixture) ctxSQLable {
			return f.db.Select(f.id).From(f.plain).LeftJoin(f.scoped, nil)
		}},
		{"SelectBuilder.RightJoin", func(f *relFixture) ctxSQLable {
			return f.db.Select(f.id).From(f.plain).RightJoin(f.scoped, nil)
		}},
		{"SelectBuilder.FullJoin refuses", func(f *relFixture) ctxSQLable {
			return f.db.Select(f.id).From(f.plain).FullJoin(f.scoped, nil)
		}},
		{"SelectBuilder.Where", func(f *relFixture) ctxSQLable {
			return f.db.Select(f.id).From(f.plain).Where(f.scoped)
		}},
		{"SelectBuilder.GroupBy", func(f *relFixture) ctxSQLable {
			return f.db.Select(f.id).From(f.plain).GroupBy(f.scoped)
		}},
		{"SelectBuilder.Having", func(f *relFixture) ctxSQLable {
			return f.db.Select(f.id).From(f.plain).GroupBy(f.id).Having(f.scoped)
		}},
		{"SelectBuilder.OrderBy", func(f *relFixture) ctxSQLable {
			return f.db.Select(f.id).From(f.plain).OrderBy(f.scoped)
		}},
		{"SelectBuilder.DistinctOn", func(f *relFixture) ctxSQLable {
			return f.db.Select(f.id).From(f.plain).DistinctOn(f.scoped)
		}},
		{"UpdateBuilder.From", func(f *relFixture) ctxSQLable {
			return f.db.Update(f.plain).Set(f.id.Val(1)).From(f.scoped)
		}},
		{"UpdateBuilder.Where", func(f *relFixture) ctxSQLable {
			return f.db.Update(f.plain).Set(f.id.Val(1)).Where(f.scoped)
		}},
		{"UpdateBuilder.Returning", func(f *relFixture) ctxSQLable {
			return f.db.Update(f.plain).Set(f.id.Val(1)).Returning(f.scoped)
		}},
		{"DeleteBuilder.Using", func(f *relFixture) ctxSQLable {
			return f.db.Delete(f.plain).Using(f.scoped)
		}},
		{"DeleteBuilder.Where", func(f *relFixture) ctxSQLable {
			return f.db.Delete(f.plain).Where(f.scoped)
		}},
		{"DeleteBuilder.Returning", func(f *relFixture) ctxSQLable {
			return f.db.Delete(f.plain).Returning(f.scoped)
		}},
		{"InsertBuilder.Returning", func(f *relFixture) ctxSQLable {
			return f.db.Insert(f.plain).Row(f.id.Val(1)).Returning(f.scoped)
		}},
		{"ConflictUpdate.Where", func(f *relFixture) ctxSQLable {
			return f.db.Insert(f.plain).Row(f.id.Val(1)).
				OnConflictUpdate(f.id).Set(f.id.Val(2)).Where(f.scoped).Done()
		}},

		// ---- handles a *Table hands out ------------------------------
		//
		// Every one of these is taken BEFORE the table is scoped. Only
		// As returns a handle that is not the receiver, which is
		// exactly why only As was wrong; the rest are here so that the
		// next method to hand back a derived handle is covered on the
		// day it is written.
		{"Table.As", func(f *relFixture) ctxSQLable {
			return f.db.Delete(f.late(func(t *pg.Table) *pg.Table { return t.As("u") }))
		}},
		{"Table.AddCheck", func(f *relFixture) ctxSQLable {
			return f.db.Delete(f.late(func(t *pg.Table) *pg.Table { return t.AddCheck("ck", "id > 0") }))
		}},
		{"Table.AddIndex", func(f *relFixture) ctxSQLable {
			return f.db.Delete(f.late(func(t *pg.Table) *pg.Table {
				return t.AddIndex(pg.NewIndex("ix", t, t.Col("id")))
			}))
		}},
		{"Table.AddPolicy", func(f *relFixture) ctxSQLable {
			return f.db.Delete(f.late(func(t *pg.Table) *pg.Table { return t.AddPolicy(pg.NewPolicy("pol")) }))
		}},
		{"Table.AddUnique", func(f *relFixture) ctxSQLable {
			return f.db.Delete(f.late(func(t *pg.Table) *pg.Table { return t.AddUnique("uq", t.Col("id")) }))
		}},
		{"Table.EnableRLS", func(f *relFixture) ctxSQLable {
			return f.db.Delete(f.late(func(t *pg.Table) *pg.Table { return t.EnableRLS() }))
		}},
		{"Table.PrimaryKey", func(f *relFixture) ctxSQLable {
			return f.db.Delete(f.late(func(t *pg.Table) *pg.Table { return t.PrimaryKey(t.Col("id")) }))
		}},
		{"Table.ForeignKey", func(f *relFixture) ctxSQLable {
			return f.db.Delete(f.late(func(t *pg.Table) *pg.Table {
				return t.ForeignKey(t.Col("tenantId"), f.other.Col("id"))
			}))
		}},
		{"Table.ForeignKeyN", func(f *relFixture) ctxSQLable {
			return f.db.Delete(f.late(func(t *pg.Table) *pg.Table {
				return t.ForeignKeyN([]pg.ColRef{t.Col("tenantId")}, f.other, []pg.ColRef{f.other.Col("id")})
			}))
		}},
		{"Table.DefaultFilter", func(f *relFixture) ctxSQLable {
			return f.db.Delete(f.late(func(t *pg.Table) *pg.Table { return t.DefaultFilter(pg.IsNotNull(t.Col("id"))) }))
		}},
		{"Table.ContextFilter", func(f *relFixture) ctxSQLable {
			return f.db.Delete(f.late(func(t *pg.Table) *pg.Table {
				return t.ContextFilter(func(context.Context) (drops.Expression, error) { return nil, nil })
			}))
		}},
		{"Table.ScopeWritesByTenant", func(f *relFixture) ctxSQLable {
			return f.db.Delete(f.late(func(t *pg.Table) *pg.Table { return t.ScopeWritesByTenant(t.Col("tenantId")) }))
		}},
		{"Table.OnInsert", func(f *relFixture) ctxSQLable {
			return f.db.Delete(f.late(func(t *pg.Table) *pg.Table {
				return t.OnInsert(pg.InsertHookFunc(func(*pg.InsertHookCtx) {}))
			}))
		}},
		{"Table.OnUpdate", func(f *relFixture) ctxSQLable {
			return f.db.Delete(f.late(func(t *pg.Table) *pg.Table {
				return t.OnUpdate(pg.UpdateHookFunc(func(*pg.UpdateHookCtx) {}))
			}))
		}},
		{"Table.OnDelete", func(f *relFixture) ctxSQLable {
			return f.db.Delete(f.late(func(t *pg.Table) *pg.Table {
				return t.OnDelete(pg.DeleteHookFunc(func(*pg.DeleteBuilder) drops.Expression { return nil }))
			}))
		}},
		{"ApplyMixins", func(f *relFixture) ctxSQLable {
			return f.db.Delete(f.late(func(t *pg.Table) *pg.Table { return pg.ApplyMixins(t, &pg.TimestampsMixin{}) }))
		}},
	}
}

// exemptRelationEntryPoints names a censused door that deliberately
// carries no scoping, with the reason. It is empty, and that is the
// point: every relation a caller can put into one of these statements
// is scoped or refused. An entry here has to say why scoping the
// relation would be WRONG, not merely inconvenient.
var exemptRelationEntryPoints = map[string]string{}

// ----------------------------------------------------------------------
// The verdict, derived from the rendered SQL
// ----------------------------------------------------------------------

// relationClauseRe matches a clause that DECLARES relations, and
// captures the comma-separated list that follows it. Those are the
// positions in which a name means "a relation this statement reads or
// writes" rather than "a value" — which is the distinction the whole
// check turns on, and the reason it can be asked of the SQL instead of
// being written down per method.
var relationClauseRe = regexp.MustCompile(
	`(?:\bFROM|\bJOIN|\bUSING|\bUPDATE|\bINTO)\s+` +
		`((?:"[^"]*"\.)?"[^"]*"(?:\s+AS\s+"[^"]*")?` +
		`(?:\s*,\s*(?:"[^"]*"\.)?"[^"]*"(?:\s+AS\s+"[^"]*")?)*)`)

var quotedIdentRe = regexp.MustCompile(`"([^"]*)"`)

// relationPositions returns the relations a rendered statement declares
// — its FROM and USING lists, its JOIN targets, its UPDATE target and
// its INSERT INTO target, at any nesting depth — mapped from the
// relation's name to the identifier its columns qualify with.
//
// The two differ exactly when the statement introduced the relation
// under an alias, and the difference is load-bearing here: an aliased
// relation's own predicates qualify with the alias, so a check that
// looked for the table name would read a correctly scoped aliased
// statement as unscoped. That is not a hypothetical — the alias is one
// of the two doors this file was written for.
//
// A FROM source that is a subquery or a function call declares no
// relation and contributes nothing.
func relationPositions(sql string) map[string]string {
	out := map[string]string{}
	for _, m := range relationClauseRe.FindAllStringSubmatch(sql, -1) {
		for _, item := range strings.Split(m[1], ",") {
			half := strings.SplitN(item, " AS ", 2)
			parts := quotedIdentRe.FindAllStringSubmatch(half[0], -1)
			if len(parts) == 0 {
				continue
			}
			// The last quoted part of the relation is the table name;
			// a first one, when present, is the schema.
			name := parts[len(parts)-1][1]
			qualifier := name
			if len(half) == 2 {
				if alias := quotedIdentRe.FindStringSubmatch(half[1]); alias != nil {
					qualifier = alias[1]
				}
			}
			out[name] = qualifier
		}
	}
	return out
}

// carriesTenantAxis reports whether a rendered statement carries the
// tenant axis of the relation it declares under this qualifier.
//
// There are two shapes and the package documents why there are two. A
// statement with a WHERE clause carries a PREDICATE, qualified with the
// relation the statement named. An INSERT has no WHERE clause for a
// predicate to reach, so what it carries is the tenant COLUMN, stamped
// from ctx — see [pg.Table.ScopeWritesByTenant] and
// InsertBuilder.ToSQLCtx. Accepting only the first would report every
// correctly stamped INSERT as a leak; accepting the second everywhere
// would accept a bare mention of the column in any statement at all,
// which is why it is admitted for INSERT and nowhere else.
func carriesTenantAxis(sql, qualifier string) bool {
	if strings.Contains(normalisePlaceholders(sql), `"`+qualifier+`"."tenantId" = $?`) {
		return true
	}
	return strings.HasPrefix(sql, "INSERT ") && strings.Contains(sql, `"tenantId"`)
}

// TestRelationEntryPointsScopeTheRelationsTheyName is the check.
//
// It renders each case for a ctx that HAS a tenant, asks the rendered
// SQL which relations the statement declares, and requires the scoped
// one — when it is among them — to be restricted. A refusal counts:
// [pg.ErrFullJoinScoped] is what a FULL JOIN answers because there is
// no clause a tenant predicate could go in, and refusing is the correct
// answer rather than a missing one.
//
// A case whose table did NOT land in a relation position asserts
// nothing here on purpose. A *Table handed to Where or to a select list
// is an expression in that position, and what happens to expressions is
// the invariant TestNoRenderedExpressionListIsInvisibleToTheResolver
// keeps. Writing those cases down as "expected unscoped" would be the
// hand-list this check exists to avoid: the moment one of them starts
// rendering its argument as a relation, it starts being checked.
func TestRelationEntryPointsScopeTheRelationsTheyName(t *testing.T) {
	for _, tc := range relationEntryPoints() {
		t.Run(tc.name, func(t *testing.T) {
			f := newRelFixture(caseKey(tc.name))
			stmt := tc.build(f)
			name := f.scoped.Name()

			sql, args, err := stmt.ToSQLCtx(relCtx())
			if err != nil {
				if !errors.Is(err, pg.ErrFullJoinScoped) {
					t.Fatalf("ToSQLCtx: %v", err)
				}
				if sql != "" {
					t.Errorf("refused with %v but still rendered %q", err, sql)
				}
				return
			}
			qualifier, named := relationPositions(sql)[name]
			if !named {
				return // an expression here, not a relation.
			}
			if !carriesTenantAxis(sql, qualifier) {
				t.Errorf("the statement names %q as a relation and carries none of its tenant axis\n  got  = %v\n  want a predicate on %q.\"tenantId\", or the column stamped",
					name, sql, qualifier)
			}
			if !containsArg(args, relTenant) {
				t.Errorf("args = %v, want them to bind the tenant %v", args, relTenant)
			}
		})
	}
}

// TestRelationEntryPointsFailClosedWithNoTenant is the other half. A
// relation the statement declares and cannot scope must stop the
// statement, not go out with the guard silently missing — which is
// precisely what FromExpr(*Table) and a stale alias both did.
func TestRelationEntryPointsFailClosedWithNoTenant(t *testing.T) {
	for _, tc := range relationEntryPoints() {
		t.Run(tc.name, func(t *testing.T) {
			f := newRelFixture(caseKey(tc.name))
			// The name is read AFTER the statement is built: a case in
			// the second half declares its table inside build, through
			// relFixture.late.
			stmt := tc.build(f)
			name := f.scoped.Name()

			// Whether the relation lands in a relation position is
			// decided on the ctx that works, so a statement that
			// refuses cannot be mistaken for one that never named it.
			sql, _, err := stmt.ToSQLCtx(relCtx())
			if _, named := relationPositions(sql)[name]; err != nil || !named {
				return
			}
			sql, _, err = tc.build(f).ToSQLCtx(context.Background())
			if !errors.Is(err, pg.ErrTenantMissing) {
				t.Fatalf("got = %v (sql %q), want %v", err, sql, pg.ErrTenantMissing)
			}
			if sql != "" {
				t.Errorf("got sql = %q, want no statement at all", sql)
			}
		})
	}
}

// caseKey turns a case name into something usable in an identifier, so
// each case gets tables of its own and no two cases can share state.
func caseKey(name string) string {
	key := strings.Fields(name)[0]
	key = strings.ReplaceAll(key, ".", "_")
	return strings.ToLower(key)
}

// ----------------------------------------------------------------------
// The census: which doors exist
// ----------------------------------------------------------------------

// relationReceivers are the types whose exported methods put a relation
// into a statement: the four builders, the ON CONFLICT handle that
// writes into one on the InsertBuilder's behalf, the DB that starts
// them, and Table itself — whose *Table-returning methods hand a caller
// a second handle on the same table.
var relationReceivers = map[string]bool{
	"DB":             true,
	"SelectBuilder":  true,
	"UpdateBuilder":  true,
	"DeleteBuilder":  true,
	"InsertBuilder":  true,
	"ConflictUpdate": true,
	"Table":          true,
}

// TestEveryRelationEntryPointIsCensused reads the package source and
// requires every door a relation can come through to appear in
// relationEntryPoints — in both directions, so a case for a door that
// no longer exists fails as loudly as a door with no case.
func TestEveryRelationEntryPointIsCensused(t *testing.T) {
	covered := map[string]bool{}
	for _, tc := range relationEntryPoints() {
		covered[strings.Fields(tc.name)[0]] = true
	}

	census := censusRelationEntryPoints(t)
	for _, want := range []string{"SelectBuilder.From", "SelectBuilder.FromExpr", "Table.As", "DeleteBuilder.Using"} {
		if !hasEntry(census, want) {
			t.Fatalf("%s is no longer a relation entry point — this census has gone stale", want)
		}
	}

	var missing []string
	for _, name := range census {
		if covered[name] || exemptRelationEntryPoints[name] != "" {
			continue
		}
		missing = append(missing, name)
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("a relation can reach a statement through these and no case exercises them with a scoped table: %v", missing)
	}

	present := map[string]bool{}
	for _, name := range census {
		present[name] = true
	}
	for name := range covered {
		if !present[name] {
			t.Errorf("case %q names no relation entry point any more — drop it or fix the name", name)
		}
	}
	for name, reason := range exemptRelationEntryPoints {
		if reason == "" {
			t.Errorf("exempt entry point %q carries no reason — an exemption without one is a leak with a name", name)
		}
		if !present[name] {
			t.Errorf("exempt entry point %q is no longer a relation entry point — drop the exemption", name)
		}
	}
}

// censusRelationEntryPoints returns every door in the package source: a
// method of a relationReceiver taking a parameter that can hold a
// *Table, a method of Table handing back a *Table, and a package-level
// function that does both.
func censusRelationEntryPoints(t *testing.T) []string {
	t.Helper()
	p := loadPgSyntax(t)
	seen := map[string]bool{}
	var out []string
	add := func(name string) {
		if !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}

	for recv, ms := range p.methods {
		if !relationReceivers[recv] {
			continue
		}
		for name, fn := range ms {
			if !ast.IsExported(name) {
				continue
			}
			if recv == "Table" && returnsTable(fn.Type) {
				add(recv + "." + name)
				continue
			}
			if fn.Type.Params == nil || len(fn.Type.Params.List) == 0 {
				continue
			}
			// ctx first is the house rule that separates an executor
			// from a builder method, so it is what excludes the
			// executors mechanically rather than by name.
			if isContextType(fn.Type.Params.List[0].Type) {
				continue
			}
			for _, param := range fn.Type.Params.List {
				if takesRelation(param.Type) {
					add(recv + "." + name)
					break
				}
			}
		}
	}
	for _, fn := range p.funcs {
		if !ast.IsExported(fn.Name.Name) || !returnsTable(fn.Type) || fn.Type.Params == nil {
			continue
		}
		for _, param := range fn.Type.Params.List {
			if typeName(param.Type) == "Table" {
				add(fn.Name.Name)
				break
			}
		}
	}
	sort.Strings(out)
	return out
}

// takesRelation reports whether a parameter of this type can be handed
// a *Table.
//
// drops.Expression is in the list, and it is the entry that matters: a
// *Table IS one, which is how FromExpr took a relation on the
// expression path and nothing scoped it. `any` and the empty interface
// are here for the same reason one level wider.
func takesRelation(e ast.Expr) bool {
	switch v := e.(type) {
	case *ast.Ellipsis:
		return takesRelation(v.Elt)
	case *ast.ArrayType:
		return takesRelation(v.Elt)
	case *ast.StarExpr:
		return typeName(v.X) == "Table"
	case *ast.Ident:
		return v.Name == "any"
	case *ast.InterfaceType:
		return v.Methods == nil || len(v.Methods.List) == 0
	}
	return isExpressionType(e)
}

// returnsTable reports whether a signature hands back a *Table — a
// second handle on a table the caller can use later, which is the shape
// Table.As got wrong.
func returnsTable(ft *ast.FuncType) bool {
	if ft.Results == nil {
		return false
	}
	for _, r := range ft.Results.List {
		if typeName(r.Type) == "Table" {
			return true
		}
	}
	return false
}

// hasEntry reports whether the census produced this door.
func hasEntry(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}
