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
	"github.com/bernardoforcillo/drops/dropstest"
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
// that has no case — the pattern round 5 established. WHICH types own
// those doors is derived too, by relationReceivers below: a type that
// renders a relation, hands a handle on one out, or starts a statement
// from one. That replaced a map of seven names sitting inside the check
// whose whole point is ending lists of names, and it widened the census
// by a third — the handles Column, Entity, Index, Schema and the
// DeleteBuilder hand out, the FindBuilder's operand methods, and the
// two builders that have no renderable statement at all and had to be
// run against a recording driver to be seen.
//
// Each door is then multiplied by the two axes a relation can vary in
// without any door being involved, because that is where a per-case
// table goes stale first:
//
//   - the ALIAS axis: every door is run with the scoped relation and
//     with an alias of it, taken before the table was scoped;
//   - the DEPTH axis: every door that yields a SELECT is run again with
//     that SELECT inside a CTE body, inside a subquery and as a set
//     operand, which is where a CTE's FROM and a subquery's FROM are
//     covered without either being written down as a case.
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
	drv    *dropstest.Driver
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

// relFixtureShapes is the ALIAS axis, multiplied over every door rather
// than written into one case of it.
//
// An alias is a relation like any other — that is the invariant
// Table.As is built around — so every door that takes a relation takes
// an aliased one, and the statement it renders qualifies that
// relation's predicates with the alias. The two shapes below are the
// same table reached the two ways, and both alias handles are taken
// BEFORE the table is scoped, which is the ordering a package-level var
// pair produces and the one that used to lose everything.
func relFixtureShapes() []struct {
	name string
	new  func(string) *relFixture
} {
	return []struct {
		name string
		new  func(string) *relFixture
	}{
		{"the table itself", newRelFixture},
		{"an alias of it", newRelFixtureAliased},
	}
}

// newRelFixtureAliased is newRelFixture with the scoped relation
// replaced by an alias of a table that is scoped only afterwards.
func newRelFixtureAliased(name string) *relFixture {
	f := newRelFixture(name)
	base := pg.NewTable("rc_aliased_" + name)
	pg.Add(base, pg.BigSerial("id").PrimaryKey())
	tenant := pg.Add(base, pg.BigInt("tenantId").NotNull())
	alias := base.As("u")
	pg.ApplyMixins(base, &pg.SoftDeleteMixin{})
	base.ContextFilter(pg.TenantFilter(tenant))
	base.ScopeWritesByTenant(tenant)
	f.scoped = alias
	f.sid = wbind[int64](alias.Col("id"))
	return f
}

func newRelFixture(name string) *relFixture {
	drv := dropstest.New().AlwaysRows([]string{"id"}, []any{int64(1)})
	f := &relFixture{
		drv:    drv,
		db:     pg.New(drv),
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
	name string
	// build is the statement to render, for a door that grows one.
	build func(f *relFixture) ctxSQLable
	// exec is the alternative for a door on a type that has no
	// renderable statement of its own — an EntityQuery, a PageBuilder —
	// where the only way to see the SQL is to run it against a
	// recording driver. Exactly one of the two is set.
	exec func(f *relFixture, ctx context.Context) error
}

// relationEntryPoints is the table the census enforces.
//
// The first half is a parameter position on a statement builder; the
// second is a handle a *Table hands out, taken through relFixture.late
// so that the table is scoped only afterwards.
func relationEntryPoints() []relationCase {
	return []relationCase{
		// ---- parameter positions -------------------------------------
		{name: "DB.Select", build: func(f *relFixture) ctxSQLable {
			return f.db.Select(f.scoped).From(f.plain)
		}},
		{name: "DB.Insert", build: func(f *relFixture) ctxSQLable {
			return f.db.Insert(f.scoped).Row(f.sid.Val(1))
		}},
		{name: "DB.Update", build: func(f *relFixture) ctxSQLable {
			return f.db.Update(f.scoped).Set(f.sid.Val(1))
		}},
		{name: "DB.Delete", build: func(f *relFixture) ctxSQLable {
			return f.db.Delete(f.scoped)
		}},
		{name: "DB.Find", build: func(f *relFixture) ctxSQLable {
			return f.db.Find(f.scoped).Select()
		}},
		{name: "SelectBuilder.From", build: func(f *relFixture) ctxSQLable {
			return f.db.Select(f.sid).From(f.scoped)
		}},
		{name: "SelectBuilder.FromExpr", build: func(f *relFixture) ctxSQLable {
			return f.db.Select(f.id).From(f.plain).FromExpr(f.scoped)
		}},
		{name: "SelectBuilder.Join", build: func(f *relFixture) ctxSQLable {
			return f.db.Select(f.id).From(f.plain).Join(f.scoped, nil)
		}},
		{name: "SelectBuilder.LeftJoin", build: func(f *relFixture) ctxSQLable {
			return f.db.Select(f.id).From(f.plain).LeftJoin(f.scoped, nil)
		}},
		{name: "SelectBuilder.RightJoin", build: func(f *relFixture) ctxSQLable {
			return f.db.Select(f.id).From(f.plain).RightJoin(f.scoped, nil)
		}},
		{name: "SelectBuilder.FullJoin refuses", build: func(f *relFixture) ctxSQLable {
			return f.db.Select(f.id).From(f.plain).FullJoin(f.scoped, nil)
		}},
		{name: "SelectBuilder.Where", build: func(f *relFixture) ctxSQLable {
			return f.db.Select(f.id).From(f.plain).Where(f.scoped)
		}},
		{name: "SelectBuilder.GroupBy", build: func(f *relFixture) ctxSQLable {
			return f.db.Select(f.id).From(f.plain).GroupBy(f.scoped)
		}},
		{name: "SelectBuilder.Having", build: func(f *relFixture) ctxSQLable {
			return f.db.Select(f.id).From(f.plain).GroupBy(f.id).Having(f.scoped)
		}},
		{name: "SelectBuilder.OrderBy", build: func(f *relFixture) ctxSQLable {
			return f.db.Select(f.id).From(f.plain).OrderBy(f.scoped)
		}},
		{name: "SelectBuilder.DistinctOn", build: func(f *relFixture) ctxSQLable {
			return f.db.Select(f.id).From(f.plain).DistinctOn(f.scoped)
		}},
		{name: "UpdateBuilder.From", build: func(f *relFixture) ctxSQLable {
			return f.db.Update(f.plain).Set(f.id.Val(1)).From(f.scoped)
		}},
		{name: "UpdateBuilder.Where", build: func(f *relFixture) ctxSQLable {
			return f.db.Update(f.plain).Set(f.id.Val(1)).Where(f.scoped)
		}},
		{name: "UpdateBuilder.Returning", build: func(f *relFixture) ctxSQLable {
			return f.db.Update(f.plain).Set(f.id.Val(1)).Returning(f.scoped)
		}},
		{name: "DeleteBuilder.Using", build: func(f *relFixture) ctxSQLable {
			return f.db.Delete(f.plain).Using(f.scoped)
		}},
		{name: "DeleteBuilder.Where", build: func(f *relFixture) ctxSQLable {
			return f.db.Delete(f.plain).Where(f.scoped)
		}},
		{name: "DeleteBuilder.Returning", build: func(f *relFixture) ctxSQLable {
			return f.db.Delete(f.plain).Returning(f.scoped)
		}},
		{name: "InsertBuilder.Returning", build: func(f *relFixture) ctxSQLable {
			return f.db.Insert(f.plain).Row(f.id.Val(1)).Returning(f.scoped)
		}},
		{name: "ConflictUpdate.Where", build: func(f *relFixture) ctxSQLable {
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
		{name: "Table.As", build: func(f *relFixture) ctxSQLable {
			return f.db.Delete(f.late(func(t *pg.Table) *pg.Table { return t.As("u") }))
		}},
		{name: "Table.AddCheck", build: func(f *relFixture) ctxSQLable {
			return f.db.Delete(f.late(func(t *pg.Table) *pg.Table { return t.AddCheck("ck", "id > 0") }))
		}},
		{name: "Table.AddIndex", build: func(f *relFixture) ctxSQLable {
			return f.db.Delete(f.late(func(t *pg.Table) *pg.Table {
				return t.AddIndex(pg.NewIndex("ix", t, t.Col("id")))
			}))
		}},
		{name: "Table.AddPolicy", build: func(f *relFixture) ctxSQLable {
			return f.db.Delete(f.late(func(t *pg.Table) *pg.Table { return t.AddPolicy(pg.NewPolicy("pol")) }))
		}},
		{name: "Table.AddUnique", build: func(f *relFixture) ctxSQLable {
			return f.db.Delete(f.late(func(t *pg.Table) *pg.Table { return t.AddUnique("uq", t.Col("id")) }))
		}},
		{name: "Table.EnableRLS", build: func(f *relFixture) ctxSQLable {
			return f.db.Delete(f.late(func(t *pg.Table) *pg.Table { return t.EnableRLS() }))
		}},
		{name: "Table.PrimaryKey", build: func(f *relFixture) ctxSQLable {
			return f.db.Delete(f.late(func(t *pg.Table) *pg.Table { return t.PrimaryKey(t.Col("id")) }))
		}},
		{name: "Table.ForeignKey", build: func(f *relFixture) ctxSQLable {
			return f.db.Delete(f.late(func(t *pg.Table) *pg.Table {
				return t.ForeignKey(t.Col("tenantId"), f.other.Col("id"))
			}))
		}},
		{name: "Table.ForeignKeyN", build: func(f *relFixture) ctxSQLable {
			return f.db.Delete(f.late(func(t *pg.Table) *pg.Table {
				return t.ForeignKeyN([]pg.ColRef{t.Col("tenantId")}, f.other, []pg.ColRef{f.other.Col("id")})
			}))
		}},
		{name: "Table.DefaultFilter", build: func(f *relFixture) ctxSQLable {
			return f.db.Delete(f.late(func(t *pg.Table) *pg.Table { return t.DefaultFilter(pg.IsNotNull(t.Col("id"))) }))
		}},
		{name: "Table.ContextFilter", build: func(f *relFixture) ctxSQLable {
			return f.db.Delete(f.late(func(t *pg.Table) *pg.Table {
				return t.ContextFilter(func(context.Context) (drops.Expression, error) { return nil, nil })
			}))
		}},
		{name: "Table.ScopeWritesByTenant", build: func(f *relFixture) ctxSQLable {
			return f.db.Delete(f.late(func(t *pg.Table) *pg.Table { return t.ScopeWritesByTenant(t.Col("tenantId")) }))
		}},
		{name: "Table.OnInsert", build: func(f *relFixture) ctxSQLable {
			return f.db.Delete(f.late(func(t *pg.Table) *pg.Table {
				return t.OnInsert(pg.InsertHookFunc(func(*pg.InsertHookCtx) {}))
			}))
		}},
		{name: "Table.OnUpdate", build: func(f *relFixture) ctxSQLable {
			return f.db.Delete(f.late(func(t *pg.Table) *pg.Table {
				return t.OnUpdate(pg.UpdateHookFunc(func(*pg.UpdateHookCtx) {}))
			}))
		}},
		{name: "Table.OnDelete", build: func(f *relFixture) ctxSQLable {
			return f.db.Delete(f.late(func(t *pg.Table) *pg.Table {
				return t.OnDelete(pg.DeleteHookFunc(func(*pg.DeleteBuilder) drops.Expression { return nil }))
			}))
		}},
		{name: "ApplyMixins", build: func(f *relFixture) ctxSQLable {
			return f.db.Delete(f.late(func(t *pg.Table) *pg.Table { return pg.ApplyMixins(t, &pg.TimestampsMixin{}) }))
		}},

		// The same shape one type over: a handle on a table, handed out
		// by something that is not the table. Each of these answers
		// with the very *Table it was given, so each carries the
		// scoping declared after it was taken — and each is here so
		// that the day one of them starts answering with a COPY, it
		// fails the way Table.As did.
		{name: "Column.Table", build: func(f *relFixture) ctxSQLable {
			return f.db.Delete(f.late(func(t *pg.Table) *pg.Table { return t.Col("id").Table() }))
		}},
		{name: "DeleteBuilder.Table", build: func(f *relFixture) ctxSQLable {
			return f.db.Delete(f.late(func(t *pg.Table) *pg.Table { return f.db.Delete(t).Table() }))
		}},
		{name: "Entity.Table", build: func(f *relFixture) ctxSQLable {
			return f.db.Delete(f.late(func(t *pg.Table) *pg.Table { return pg.NewEntity[relLateRow](t).Table() }))
		}},
		{name: "Index.Table", build: func(f *relFixture) ctxSQLable {
			return f.db.Delete(f.late(func(t *pg.Table) *pg.Table {
				return pg.NewIndex("rc_ix", t, t.Col("id")).Table()
			}))
		}},
		{name: "Schema.Add", build: func(f *relFixture) ctxSQLable {
			return f.db.Delete(f.late(func(t *pg.Table) *pg.Table { return pg.NewSchema().Add(t).Tables()[0] }))
		}},
		{name: "Schema.Tables", build: func(f *relFixture) ctxSQLable {
			return f.db.Delete(f.late(func(t *pg.Table) *pg.Table { return pg.NewSchema(t).Tables()[0] }))
		}},

		// ---- doors on a builder with no statement to render ---------
		//
		// A FindBuilder answers with the SELECT it is building, so its
		// operand methods are read from the statement like every other.
		// An EntityQuery and a PageBuilder have no such method: they
		// execute, so the only way to see what they named is to run
		// them against a recording driver and read what was sent.
		{name: "FindBuilder.Where", build: func(f *relFixture) ctxSQLable {
			return f.db.Find(f.plain).Where(f.scoped).Select()
		}},
		{name: "FindBuilder.OrderBy", build: func(f *relFixture) ctxSQLable {
			return f.db.Find(f.plain).OrderBy(f.scoped).Select()
		}},
		{name: "EntityQuery.Where", exec: func(f *relFixture, ctx context.Context) error {
			_, err := pg.NewEntity[relRow](f.plain).Query(f.db).Where(f.scoped).All(ctx)
			return err
		}},
		{name: "EntityQuery.OrderBy", exec: func(f *relFixture, ctx context.Context) error {
			_, err := pg.NewEntity[relRow](f.plain).Query(f.db).OrderBy(f.scoped).All(ctx)
			return err
		}},
		{name: "PageBuilder.Where", exec: func(f *relFixture, ctx context.Context) error {
			_, err := pg.NewEntity[relRow](f.plain).Page(f.db).
				OrderBy(pg.Asc(f.plain.Col("id"))).Where(f.scoped).All(ctx)
			return err
		}},
	}
}

// relRow and relLateRow are the entity shapes the cases above need: one
// over relPlain, one over the table relFixture.late builds.
type relRow struct {
	ID int64 `drop:"id"`
}

type relLateRow struct {
	ID       int64 `drop:"id"`
	TenantID int64 `drop:"tenantId"`
}

// exemptRelationEntryPoints names a censused door that deliberately
// carries no scoping, with the reason. It is empty, and that is the
// point: every relation a caller can put into one of these statements
// is scoped or refused. An entry here has to say why scoping the
// relation would be WRONG, not merely inconvenient.
var exemptRelationEntryPoints = map[string]string{
	"Index.Where": "a partial index is a DECLARATION, not a request: CREATE INDEX ... WHERE is rendered by the migrator with no ctx and no tenant, and a predicate scoped to the tenant who happened to run the migration would index one tenant's rows for everybody — the reason ddlBody is exempt from resolveExpr, restated for the other object that stores a predicate",
}

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

// renderedStmt is one statement a case produced: what was rendered, and
// what was bound.
type renderedStmt struct {
	sql  string
	args []any
}

// relRender runs one case for ctx and returns every statement it
// produced. A door on a builder renders one; a door on something that
// only executes produces whatever reached the driver, and all of them
// are checked, because an eager load or a page count is as much a
// statement naming the relation as the query the caller wrote.
func relRender(tc relationCase, f *relFixture, ctx context.Context) ([]renderedStmt, error) {
	if tc.build != nil {
		sql, args, err := tc.build(f).ToSQLCtx(ctx)
		if err != nil {
			return nil, err
		}
		return []renderedStmt{{sql: sql, args: args}}, nil
	}
	f.drv.Reset()
	err := tc.exec(f, ctx)
	var out []renderedStmt
	for _, st := range f.drv.Statements() {
		out = append(out, renderedStmt{sql: st.SQL, args: st.Args})
	}
	if err != nil {
		return out, err
	}
	if len(out) == 0 {
		return nil, errors.New("the case executed and sent no statement at all")
	}
	return out, nil
}

// relNestings returns the same statement nested inside another one, for
// a case that builds a SELECT.
//
// This is the depth axis, and it is multiplied out rather than written
// down: a relation in a CTE's FROM and a relation in a subquery's FROM
// are the same relation in the same clause, one level in, and both are
// shapes the resolver reaches through a different path than the top
// level. Rather than a hand-written case per door per depth — which is
// where a door gets forgotten — every door that yields a SELECT is run
// at all three depths, so a door added tomorrow is checked at all three
// on the day it is written.
func relNestings(f *relFixture, stmt ctxSQLable) map[string]ctxSQLable {
	sel, ok := stmt.(*pg.SelectBuilder)
	if !ok {
		return nil
	}
	return map[string]ctxSQLable{
		"in a CTE body":    f.db.Select(f.id).With(pg.CTEDef("rc_cte", sel)).From(f.plain),
		"in a subquery":    f.db.Select(f.id).From(f.plain).Where(pg.Exists(sel)),
		"in a set operand": f.db.Select(f.id).From(f.plain).Union(sel),
	}
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
			for _, shape := range relFixtureShapes() {
				t.Run(shape.name, func(t *testing.T) {
					f := shape.new(caseKey(tc.name))
					stmts, err := relRender(tc, f, relCtx())
					if err != nil {
						relRequireRefusal(t, err, stmts)
						return
					}
					relRequireAxis(t, f.scoped.Name(), stmts)

					if tc.build == nil {
						return
					}
					for depth, nested := range relNestings(f, tc.build(f)) {
						t.Run(depth, func(t *testing.T) {
							sql, args, err := nested.ToSQLCtx(relCtx())
							if err != nil {
								relRequireRefusal(t, err, []renderedStmt{{sql: sql}})
								return
							}
							relRequireAxis(t, f.scoped.Name(), []renderedStmt{{sql: sql, args: args}})
						})
					}
				})
			}
		})
	}
}

// relRequireRefusal accepts the one error that is an ANSWER rather than
// a failure: a FULL JOIN with a scoped relation on either side has no
// clause a tenant predicate could be placed in, so it refuses. Anything
// else is the case failing to render.
func relRequireRefusal(t *testing.T, err error, stmts []renderedStmt) {
	t.Helper()
	if !errors.Is(err, pg.ErrFullJoinScoped) {
		t.Fatalf("ToSQLCtx: %v", err)
	}
	for _, st := range stmts {
		if st.sql != "" {
			t.Errorf("refused with %v but still rendered %q", err, st.sql)
		}
	}
}

// relRequireAxis is the verdict: wherever a statement names the scoped
// table as a relation, that statement carries its tenant axis.
func relRequireAxis(t *testing.T, name string, stmts []renderedStmt) {
	t.Helper()
	for _, st := range stmts {
		qualifier, named := relationPositions(st.sql)[name]
		if !named {
			continue // an expression here, not a relation.
		}
		if !carriesTenantAxis(st.sql, qualifier) {
			t.Errorf("the statement names %q as a relation and carries none of its tenant axis\n  got  = %v\n  want a predicate on %q.\"tenantId\", or the column stamped",
				name, st.sql, qualifier)
		}
		if !containsArg(st.args, relTenant) {
			t.Errorf("args = %v, want them to bind the tenant %v", st.args, relTenant)
		}
	}
}

// TestRelationEntryPointsFailClosedWithNoTenant is the other half. A
// relation the statement declares and cannot scope must stop the
// statement, not go out with the guard silently missing — which is
// precisely what FromExpr(*Table) and a stale alias both did.
func TestRelationEntryPointsFailClosedWithNoTenant(t *testing.T) {
	for _, tc := range relationEntryPoints() {
		t.Run(tc.name, func(t *testing.T) {
			for _, shape := range relFixtureShapes() {
				t.Run(shape.name, func(t *testing.T) {
					relRequireFailsClosed(t, tc, shape.new(caseKey(tc.name)))
				})
			}
		})
	}
}

// relRequireFailsClosed is the fail-closed verdict for one case over
// one fixture shape.
func relRequireFailsClosed(t *testing.T, tc relationCase, f *relFixture) {
	t.Helper()
	// Whether the relation lands in a relation position is decided on
	// the ctx that works, so a statement that refuses cannot be
	// mistaken for one that never named it. The name is read
	// afterwards: a case in the second half declares its table inside
	// build, through relFixture.late.
	stmts, err := relRender(tc, f, relCtx())
	name := f.scoped.Name()
	if err != nil || !relNamesRelation(name, stmts) {
		return
	}
	relRequireNothingSent(t, f, tc, context.Background())

	if tc.build == nil {
		return
	}
	for depth, nested := range relNestings(f, tc.build(f)) {
		t.Run(depth, func(t *testing.T) {
			sql, _, err := nested.ToSQLCtx(context.Background())
			if !errors.Is(err, pg.ErrTenantMissing) {
				t.Fatalf("got = %v (sql %q), want %v", err, sql, pg.ErrTenantMissing)
			}
			if sql != "" {
				t.Errorf("got sql = %q, want no statement at all", sql)
			}
		})
	}
}

// relNamesRelation reports whether any of these statements names the
// table as a relation.
func relNamesRelation(name string, stmts []renderedStmt) bool {
	for _, st := range stmts {
		if _, named := relationPositions(st.sql)[name]; named {
			return true
		}
	}
	return false
}

// relRequireNothingSent runs the case on a ctx with no tenant and
// requires it to refuse with nothing rendered and nothing sent — the
// assertion that separates "the guard reported" from "the guard
// reported after the rows had gone".
func relRequireNothingSent(t *testing.T, f *relFixture, tc relationCase, ctx context.Context) {
	t.Helper()
	stmts, err := relRender(tc, f, ctx)
	if !errors.Is(err, pg.ErrTenantMissing) {
		t.Fatalf("got = %v (sql %v), want %v", err, relSQLs(stmts), pg.ErrTenantMissing)
	}
	for _, st := range stmts {
		if st.sql != "" {
			t.Errorf("got sql = %q, want no statement at all", st.sql)
		}
	}
}

// relSQLs is the statements' text, for a message.
func relSQLs(stmts []renderedStmt) []string {
	out := make([]string, 0, len(stmts))
	for _, st := range stmts {
		out = append(out, st.sql)
	}
	return out
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

// relationReceivers derives the types whose exported methods can put a
// relation into a statement, and the derivation replaced a map of seven
// names that sat inside a check whose whole purpose is ending lists of
// names.
//
// A type qualifies three ways, and each is a different sentence about
// relations:
//
//   - it RENDERS one: its render closure reads a field that holds a
//     *Table. That is the four builders and the CTE, computed from what
//     they read rather than from what they are called;
//   - it HANDS ONE OUT: some method of it answers with a *Table, which
//     is a second handle on a table the caller can use later — the
//     shape Table.As got wrong;
//   - it STARTS one: some method takes a *Table and answers with a type
//     that renders relations. That is DB, and anything else that grows
//     a statement constructor.
//
// A type that starts rendering a relation is censused on the day it
// does, which is what the round-8 sentence requires of this file: the
// resolver walks expressions, so a relation reaching a statement by a
// door nobody enumerated is a relation nobody scopes.
func (p *pgSyntax) relationReceivers() map[string]bool {
	holdsTable := map[string]bool{}
	for {
		changed := false
		for name, st := range p.structs {
			if holdsTable[name] {
				continue
			}
			for _, f := range st.Fields.List {
				if tn := typeName(f.Type); tn == "Table" || holdsTable[tn] {
					holdsTable[name], changed = true, true
					break
				}
			}
		}
		if !changed {
			break
		}
	}

	out := map[string]bool{}
	for name, st := range p.structs {
		sites := p.renderSites(name)
		if len(sites) == 0 {
			continue
		}
		fields := map[string]bool{}
		for _, f := range st.Fields.List {
			for _, n := range f.Names {
				fields[n.Name] = true
			}
		}
		fieldType := map[string]ast.Expr{}
		for _, f := range st.Fields.List {
			for _, n := range f.Names {
				fieldType[n.Name] = f.Type
			}
		}
		for f := range p.fieldsReadAt(name, fields, sites) {
			if tn := typeName(fieldType[f]); tn == "Table" || holdsTable[tn] {
				out[name] = true
				break
			}
		}
	}
	for recv, ms := range p.methods {
		for _, fn := range ms {
			if returnsTable(fn.Type) {
				out[recv] = true
				break
			}
		}
	}
	for {
		changed := false
		for recv, ms := range p.methods {
			for _, fn := range ms {
				if fn.Type.Results == nil {
					continue
				}
				// A type that STARTS a statement: it takes a *Table and
				// answers with something that renders relations.
				if !out[recv] && fn.Type.Params != nil {
					takes := false
					for _, param := range fn.Type.Params.List {
						if typeName(param.Type) == "Table" {
							takes = true
						}
					}
					if takes {
						for _, r := range fn.Type.Results.List {
							if out[typeName(r.Type)] {
								out[recv], changed = true, true
							}
						}
					}
				}
				// And a handle a relation-renderer hands out to
				// configure part of the statement it is building — the
				// ON CONFLICT clause is one — writes into that
				// statement on its behalf, so its methods are doors
				// too.
				if !out[recv] {
					continue
				}
				for _, r := range fn.Type.Results.List {
					sub := typeName(r.Type)
					if sub == "" || out[sub] || p.structs[sub] == nil || !holdsTable[sub] {
						continue
					}
					out[sub], changed = true, true
				}
			}
		}
		if !changed {
			return out
		}
	}
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

	receivers := p.relationReceivers()
	for recv, ms := range p.methods {
		if !receivers[recv] {
			continue
		}
		for name, fn := range ms {
			if !ast.IsExported(name) {
				continue
			}
			if returnsTable(fn.Type) {
				add(recv + "." + name)
				continue
			}
			if fn.Type.Params == nil || len(fn.Type.Params.List) == 0 {
				continue
			}
			// A method that is handed a ctx EXECUTES: it sends the
			// statement rather than growing one, and the axis it
			// carries is asserted by the executor table in
			// pg/scopeexec_test.go, which censuses exactly these. The
			// house rule is that ctx comes first, but Entity's methods
			// take the *DB first, so the test is "does it take one at
			// all" rather than "in position zero" — the narrower
			// version let four of Entity's executors into this census,
			// where there is no statement to render.
			if takesContext(fn.Type) {
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

// takesContext reports whether a signature is handed a context.Context
// in any position.
func takesContext(ft *ast.FuncType) bool {
	if ft.Params == nil {
		return false
	}
	for _, param := range ft.Params.List {
		if isContextType(param.Type) {
			return true
		}
	}
	return false
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
