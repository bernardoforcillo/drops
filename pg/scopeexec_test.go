package pg_test

import (
	"context"
	"errors"
	"fmt"
	"go/ast"
	"sort"
	"strings"
	"testing"

	"github.com/bernardoforcillo/drops/dropstest"
	"github.com/bernardoforcillo/drops/pg"
	"github.com/bernardoforcillo/drops/vector"
)

// ----------------------------------------------------------------------
// The EXECUTOR census, enforced against the package source
//
//	Every method that can send a statement for a scoped table carries
//	that table's axis — and refuses, sending nothing, when the ctx
//	cannot supply it.
//
// Both halves of that sentence were already asserted, and neither was
// asserted of everything. The carrying half named twenty executors and
// the refusing half named seven, chosen by whoever wrote the test on
// the day: Entity.CreateMany, UpsertMany, Save, CreateCols, Patch,
// PatchKey and Page, the EntityQuery methods and the cursor path all
// refuse correctly, and nothing said so. A behaviour nothing asserts is
// a behaviour the next refactor is free to change, and the change is
// silent — the refusal is what stands between a request with no tenant
// and every tenant's rows.
//
// So the list is read from the package source instead of typed. An
// executor is an exported method that is handed a context.Context, on a
// type this package builds statements with (relationReceivers, derived
// in pg/scoperelation_census_test.go) — plus the DB itself. Both halves
// run the same table, so neither can drift from the other, and the
// census fails in both directions: a door with no case, and a case for
// a door that no longer exists.
//
// What "carries the axis" means is read from the statement rather than
// declared per case, exactly as the relation census reads it: a
// predicate qualified with the relation, or — for an INSERT, which has
// no WHERE clause for a predicate to reach — the tenant column stamped
// from ctx. See carriesTenantAxis.

// execTenant is the tenant these cases put on ctx. It differs from the
// other tenant constants in the package's tests so a value that arrived
// from somewhere else cannot pass by coincidence.
const execTenant = int64(53)

func execCtx() context.Context { return pg.WithTenant(context.Background(), execTenant) }

// execRow is the entity these cases execute against.
type execRow struct {
	ID       int64  `drop:"id"`
	TenantID int64  `drop:"tenantId"`
	Name     string `drop:"name"`
}

// execFixture is one scoped table, its entity, and the driver that
// records what was sent. Nothing else is scoped, so an axis in a
// recorded statement can only have come from this table.
type execFixture struct {
	drv  *dropstest.Driver
	db   *pg.DB
	tbl  *pg.Table
	id   *pg.Col[int64]
	name *pg.Col[string]
	ent  *pg.Entity[execRow]

	// rendered holds statements a case produced without sending them —
	// ToSQLCtx renders and returns rather than executing, and the axis
	// it carries is asserted on the same terms as one that was sent.
	rendered []renderedStmt
}

func newExecFixture() *execFixture {
	drv := dropstest.New().AlwaysRows(
		[]string{"id", "tenantId", "name"},
		[]any{int64(1), execTenant, "row"},
	)
	f := &execFixture{drv: drv, tbl: pg.NewTable("ex_rows")}
	f.db = pg.New(&execDriver{Driver: drv, f: f})
	f.id = pg.Add(f.tbl, pg.BigSerial("id").PrimaryKey())
	tenant := pg.Add(f.tbl, pg.BigInt("tenantId").NotNull())
	f.name = pg.Add(f.tbl, pg.Text("name").NotNull())
	f.ent = pg.NewEntity[execRow](f.tbl).ScopeByTenant(tenant)
	return f
}

// execDriver is the fixture's driver with COPY support bolted on, so
// [pg.CopyFrom] can be run by the same table as every other executor.
//
// A COPY carries no SQL for the axis to appear in — that is the point
// of it — so what reaches the server is recorded in the shape the
// assertion can read: the destination relation, the column list, and
// every value that was streamed. The tenant column has to be among the
// columns and the ctx tenant among the values, which is exactly what
// [pg.Entity.CreateMany] promises and what CopyFrom's own stamping
// exists to keep.
type execDriver struct {
	*dropstest.Driver
	f *execFixture
}

func (d *execDriver) Copy(_ context.Context, table string, cols []string, rows [][]any) (int64, error) {
	var b strings.Builder
	b.WriteString(`COPY "` + table + `" (`)
	for i, c := range cols {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(`"` + c + `"`)
	}
	b.WriteString(") FROM STDIN")
	var args []any
	for _, r := range rows {
		args = append(args, r...)
	}
	d.f.record(b.String(), args)
	return int64(len(rows)), nil
}

// record is how a ToSQLCtx case reports what it rendered.
func (f *execFixture) record(sql string, args []any) {
	f.rendered = append(f.rendered, renderedStmt{sql: sql, args: args})
}

// statements is everything the case produced: what reached the driver,
// and what a rendering case rendered.
func (f *execFixture) statements() []renderedStmt {
	out := append([]renderedStmt(nil), f.rendered...)
	for _, st := range f.drv.Statements() {
		// A transaction boundary is a driver call and not a statement:
		// [dropstest.Driver] records begin, commit and rollback with no
		// SQL at all rather than inventing the text, so there is
		// nothing in one for an axis to be carried by and nothing sent
		// to a server by it. Seeder.Apply wraps its inserts in one.
		if st.Op != dropstest.OpExec && st.Op != dropstest.OpQuery {
			continue
		}
		out = append(out, renderedStmt{sql: st.SQL, args: st.Args})
	}
	return out
}

// executorCase is one door a statement can go out through.
//
// name is the censused door — "<receiver>.<method>" — optionally
// followed by a space and a note, so a second case for the same door
// (the cursor path through PageBuilder.All is one) can say what it
// covers. The census reads the first word, as the other censuses do.
type executorCase struct {
	name string
	// rows overrides the canned answer for a case that scans something
	// other than a row of ex_rows.
	rows func(*dropstest.Driver)
	run  func(f *execFixture, ctx context.Context) error
}

// executorEntryPoints is the table both halves enforce.
func executorEntryPoints() []executorCase {
	return []executorCase{
		// select.go.
		{name: "SelectBuilder.Rows", run: func(f *execFixture, ctx context.Context) error {
			rows, err := f.db.Select(f.id).From(f.tbl).Rows(ctx)
			if err == nil {
				rows.Close()
			}
			return err
		}},
		{name: "SelectBuilder.All", run: func(f *execFixture, ctx context.Context) error {
			var out []execRow
			return f.db.Select().From(f.tbl).All(ctx, &out)
		}},
		{name: "SelectBuilder.One", run: func(f *execFixture, ctx context.Context) error {
			var out execRow
			return f.db.Select().From(f.tbl).One(ctx, &out)
		}},
		{
			name: "SelectBuilder.Count",
			rows: func(d *dropstest.Driver) { d.AlwaysRows([]string{"count"}, []any{int64(1)}) },
			run: func(f *execFixture, ctx context.Context) error {
				_, err := f.db.Select().From(f.tbl).Count(ctx)
				return err
			},
		},
		{name: "SelectBuilder.ToSQLCtx", run: func(f *execFixture, ctx context.Context) error {
			return execRender(f, f.db.Select(f.id).From(f.tbl), ctx)
		}},

		// update.go.
		{name: "UpdateBuilder.Exec", run: func(f *execFixture, ctx context.Context) error {
			_, err := f.db.Update(f.tbl).Set(f.name.Val("x")).Exec(ctx)
			return err
		}},
		{name: "UpdateBuilder.All", run: func(f *execFixture, ctx context.Context) error {
			var out []execRow
			return f.db.Update(f.tbl).Set(f.name.Val("x")).Returning(f.id).All(ctx, &out)
		}},
		{name: "UpdateBuilder.One", run: func(f *execFixture, ctx context.Context) error {
			var out execRow
			return f.db.Update(f.tbl).Set(f.name.Val("x")).Returning(f.id).One(ctx, &out)
		}},
		{name: "UpdateBuilder.ToSQLCtx", run: func(f *execFixture, ctx context.Context) error {
			return execRender(f, f.db.Update(f.tbl).Set(f.name.Val("x")), ctx)
		}},

		// delete.go.
		{name: "DeleteBuilder.Exec", run: func(f *execFixture, ctx context.Context) error {
			_, err := f.db.Delete(f.tbl).Exec(ctx)
			return err
		}},
		{name: "DeleteBuilder.All", run: func(f *execFixture, ctx context.Context) error {
			var out []execRow
			return f.db.Delete(f.tbl).Returning(f.id).All(ctx, &out)
		}},
		{name: "DeleteBuilder.One", run: func(f *execFixture, ctx context.Context) error {
			var out execRow
			return f.db.Delete(f.tbl).Returning(f.id).One(ctx, &out)
		}},
		{name: "DeleteBuilder.ToSQLCtx", run: func(f *execFixture, ctx context.Context) error {
			return execRender(f, f.db.Delete(f.tbl), ctx)
		}},

		// insert.go — the axis an INSERT carries is the stamped column.
		{name: "InsertBuilder.Exec", run: func(f *execFixture, ctx context.Context) error {
			_, err := f.db.Insert(f.tbl).Row(f.name.Val("x")).Exec(ctx)
			return err
		}},
		{name: "InsertBuilder.All", run: func(f *execFixture, ctx context.Context) error {
			var out []execRow
			return f.db.Insert(f.tbl).Row(f.name.Val("x")).Returning(f.id).All(ctx, &out)
		}},
		{name: "InsertBuilder.One", run: func(f *execFixture, ctx context.Context) error {
			var out execRow
			return f.db.Insert(f.tbl).Row(f.name.Val("x")).Returning(f.id).One(ctx, &out)
		}},
		{name: "InsertBuilder.ToSQLCtx", run: func(f *execFixture, ctx context.Context) error {
			return execRender(f, f.db.Insert(f.tbl).Row(f.name.Val("x")), ctx)
		}},

		// find.go.
		{name: "FindBuilder.All", run: func(f *execFixture, ctx context.Context) error {
			var out []execRow
			return f.db.Find(f.tbl).All(ctx, &out)
		}},
		{name: "FindBuilder.One", run: func(f *execFixture, ctx context.Context) error {
			var out execRow
			return f.db.Find(f.tbl).One(ctx, &out)
		}},

		// entity.go — the reads.
		{name: "Entity.Get", run: func(f *execFixture, ctx context.Context) error {
			_, err := f.ent.Get(f.db, ctx, int64(1))
			return err
		}},
		{name: "EntityQuery.All", run: func(f *execFixture, ctx context.Context) error {
			_, err := f.ent.Query(f.db).All(ctx)
			return err
		}},
		{name: "EntityQuery.One", run: func(f *execFixture, ctx context.Context) error {
			_, err := f.ent.Query(f.db).One(ctx)
			return err
		}},
		{name: "EntityQuery.Stream", run: func(f *execFixture, ctx context.Context) error {
			return f.ent.Query(f.db).Stream(ctx, func(*execRow) error { return nil })
		}},

		// entity.go — the writes. Each stamps the tenant rather than
		// predicating it, except the ones that carry a WHERE clause.
		{name: "Entity.Create", run: func(f *execFixture, ctx context.Context) error {
			return f.ent.Create(f.db, ctx, &execRow{Name: "x"})
		}},
		{name: "Entity.CreateCols", run: func(f *execFixture, ctx context.Context) error {
			// The tenant column is named explicitly because CreateCols
			// refuses a column list without it — a row written outside
			// every tenant is the write-side shape of the same leak.
			return f.ent.CreateCols(f.db, ctx, &execRow{Name: "x"}, f.name, f.tbl.Col("tenantId"))
		}},
		{name: "Entity.CreateMany", run: func(f *execFixture, ctx context.Context) error {
			_, err := f.ent.CreateMany(f.db, ctx, []execRow{{Name: "x"}, {Name: "y"}})
			return err
		}},
		{name: "Entity.UpsertMany", run: func(f *execFixture, ctx context.Context) error {
			_, err := f.ent.UpsertMany(f.db, ctx, []execRow{{ID: 1, Name: "x"}})
			return err
		}},
		{name: "Entity.Save inserts", run: func(f *execFixture, ctx context.Context) error {
			return f.ent.Save(f.db, ctx, &execRow{Name: "x"})
		}},
		{name: "Entity.Save updates", run: func(f *execFixture, ctx context.Context) error {
			return f.ent.Save(f.db, ctx, &execRow{ID: 1, Name: "x"})
		}},
		{name: "Entity.Update", run: func(f *execFixture, ctx context.Context) error {
			return f.ent.Update(f.db, ctx, &execRow{ID: 1, TenantID: execTenant, Name: "x"})
		}},
		{name: "Entity.Delete", run: func(f *execFixture, ctx context.Context) error {
			_, err := f.ent.Delete(f.db, ctx, int64(1))
			return err
		}},
		{name: "Entity.Patch", run: func(f *execFixture, ctx context.Context) error {
			_, err := f.ent.Patch(f.db, ctx, int64(1), f.name.Val("x"))
			return err
		}},
		{name: "Entity.PatchKey", run: func(f *execFixture, ctx context.Context) error {
			_, err := f.ent.PatchKey(f.db, ctx, []any{int64(1)}, f.name.Val("x"))
			return err
		}},

		// page.go, including the cursor path — the branch that rewrites
		// the predicate around the decoded cursor values, and so the
		// branch where a hand-written WHERE clause could lose the axis.
		{name: "PageBuilder.All", run: func(f *execFixture, ctx context.Context) error {
			_, err := f.ent.Page(f.db).OrderBy(pg.Asc(f.id)).Limit(2).All(ctx)
			return err
		}},
		{
			name: "PageBuilder.All after a cursor",
			// Two rows for a limit of one, so the first page reports
			// more and hands back a cursor to page after.
			rows: func(d *dropstest.Driver) {
				d.AlwaysRows([]string{"id", "tenantId", "name"},
					[]any{int64(1), execTenant, "row"},
					[]any{int64(2), execTenant, "row"})
			},
			run: func(f *execFixture, ctx context.Context) error {
				page, err := f.ent.Page(f.db).OrderBy(pg.Asc(f.id)).Limit(1).All(execCtx())
				if err != nil {
					return err
				}
				if page.NextCursor == "" {
					return errors.New("the first page returned no cursor to page after")
				}
				f.drv.Reset()
				f.rendered = nil
				_, err = f.ent.Page(f.db).OrderBy(pg.Asc(f.id)).Limit(1).After(page.NextCursor).All(ctx)
				return err
			},
		},

		// db.go — the statement a caller assembled themselves.
		{name: "DB.ExecExpr", run: func(f *execFixture, ctx context.Context) error {
			_, err := f.db.ExecExpr(ctx, f.db.Delete(f.tbl))
			return err
		}},

		// copy.go — the bulk path, which sends no SQL at all. See
		// execDriver for the shape its rows are asserted in.
		{name: "CopyFrom", run: func(f *execFixture, ctx context.Context) error {
			_, err := pg.CopyFrom(f.db, ctx, f.ent, []execRow{{Name: "x"}, {Name: "y"}})
			return err
		}},

		// factory.go — a test fixture builder, and every row it writes
		// is a row in somebody's table.
		{name: "Factory.Create", run: func(f *execFixture, ctx context.Context) error {
			_, err := execFactory(f).Create(ctx, f.db)
			return err
		}},
		{name: "Factory.CreateN", run: func(f *execFixture, ctx context.Context) error {
			_, err := execFactory(f).CreateN(ctx, f.db, 2)
			return err
		}},

		// seed.go — the same rows through the seeder, inside its
		// transaction.
		{name: "Seeder.Apply", run: func(f *execFixture, ctx context.Context) error {
			s := pg.NewSeeder(f.db)
			pg.SeedAdd(s, f.ent, execRow{Name: "x"})
			return s.Apply(ctx)
		}},

		// vectorstore.go — a nearest-neighbour read is a read.
		{
			name: "VectorStore.Search",
			// The store scans (id, distance) and nothing else, so the
			// canned three-column row of the fixture would not align.
			rows: func(d *dropstest.Driver) {
				d.AlwaysRows([]string{"id", "_drops_distance"}, []any{int64(1), 0.5})
			},
			run: func(f *execFixture, ctx context.Context) error {
				// The text column stands in for the embedding: the
				// distance expression is built from the query vector
				// and the column reference, and what this case asserts
				// is the predicate around them.
				store := pg.NewVectorStore(f.db, f.tbl, f.id, f.name)
				_, err := store.Search(ctx, vector.Query{
					Vector: []float32{1, 2, 3},
					TopK:   1,
					Metric: vector.Cosine,
				})
				return err
			},
		},
	}
}

// execFactory is the factory the two Factory cases share: one row per
// call, with nothing set but the name, so the tenant in the statement
// can only have come from the entity's axis.
func execFactory(f *execFixture) *pg.Factory[execRow] {
	return pg.NewFactory(f.ent, func(seq int) execRow {
		return execRow{Name: fmt.Sprintf("row-%d", seq)}
	})
}

// execRender runs a ToSQLCtx case: it renders rather than sends, and
// what it rendered is asserted on the same terms as what was sent.
func execRender(f *execFixture, stmt ctxSQLable, ctx context.Context) error {
	sql, args, err := stmt.ToSQLCtx(ctx)
	if err != nil {
		return err
	}
	f.record(sql, args)
	return nil
}

// exemptExecutors names an executor that deliberately carries no axis,
// with the reason. Each entry has to say why carrying one would be
// WRONG or impossible, not merely inconvenient.
var exemptExecutors = map[string]string{
	"DB.Exec":             "raw SQL the caller wrote: drops is handed a string and never learns which relations it names, so there is no relation to scope — the escape hatch, documented as one",
	"DB.Query":            "raw SQL the caller wrote, for the same reason as DB.Exec",
	"DB.Ping":             "sends no statement against any relation",
	"DB.Begin":            "starts a transaction and sends no statement of its own; the statements inside it are the executors below, each scoped on its own terms",
	"DB.StartPoolMetrics": "samples the driver's connection pool on a timer and sends no statement at all; the ctx is the sampler's lifetime",
	"DB.InTx":             "runs the caller's function against a transactional *DB, and every statement inside it goes out through one of the executors this table covers",
	"Push":                "reconciles the SCHEMA: every statement it sends is DDL — CREATE, ALTER, DROP — against relations rather than rows, and DDL has no WHERE clause and no row for a tenant predicate to select. What it must not do is destroy a relation nobody declared, which is ownedBy's promise and TestPushWithholdsUnmanagedObjects's assertion",
	"Subscribe":           "sends LISTEN and no statement against the entity's table: the change feed is a whole-table stream by construction, and the one place it reads a row is Entity.Get, which is in this table and carries the axis. What crosses tenants is the primary key of a changed row, which is what a change feed publishes — a consumer that must not see other tenants' keys filters the stream, or reads the feed behind row-level security",
}

// TestEveryExecutorCarriesTheTenantAxis is the first half: with a
// tenant on ctx, every statement an executor produces against a scoped
// table carries that table's axis, and binds that tenant.
func TestEveryExecutorCarriesTheTenantAxis(t *testing.T) {
	for _, tc := range executorEntryPoints() {
		t.Run(tc.name, func(t *testing.T) {
			f := newExecFixture()
			if tc.rows != nil {
				tc.rows(f.drv)
			}
			if err := tc.run(f, execCtx()); err != nil {
				t.Fatalf("run: %v", err)
			}
			stmts := f.statements()
			if len(stmts) == 0 {
				t.Fatal("no statement was produced at all")
			}
			for _, st := range stmts {
				if !carriesTenantAxis(st.sql, f.tbl.Name()) {
					t.Errorf("the statement carries none of the table's tenant axis\n  got  = %v\n  want a predicate on %q.\"tenantId\", or the column stamped",
						st.sql, f.tbl.Name())
				}
				if !containsArg(st.args, execTenant) {
					t.Errorf("args = %v, want them to bind the tenant %v\n  in %v", st.args, execTenant, st.sql)
				}
			}
		})
	}
}

// TestEveryExecutorFailsClosedWithNoTenant is the half that was
// asserted of seven of these and true of all of them. With no tenant on
// ctx the executor refuses, and — the part worth asserting separately —
// nothing reaches the driver. A guard that reported after the rows had
// gone would be a leak with a stack trace attached.
func TestEveryExecutorFailsClosedWithNoTenant(t *testing.T) {
	for _, tc := range executorEntryPoints() {
		t.Run(tc.name, func(t *testing.T) {
			f := newExecFixture()
			if tc.rows != nil {
				tc.rows(f.drv)
			}
			err := tc.run(f, context.Background())
			if !errors.Is(err, pg.ErrTenantMissing) {
				t.Fatalf("got = %v (statements %v), want %v", err, relSQLs(f.statements()), pg.ErrTenantMissing)
			}
			for _, st := range f.statements() {
				t.Errorf("a refused request still produced a statement: %v", st.sql)
			}
		})
	}
}

// TestEveryExecutorIsCensused reads the package source and requires
// every method that can send a statement for a caller's table to appear
// in executorEntryPoints or in exemptExecutors — in both directions, so
// a case for a door that no longer exists fails as loudly as a door
// with no case.
func TestEveryExecutorIsCensused(t *testing.T) {
	covered := map[string]bool{}
	for _, tc := range executorEntryPoints() {
		covered[strings.Fields(tc.name)[0]] = true
	}

	census := censusExecutors(t, loadPgSyntax(t))
	for _, want := range []string{"SelectBuilder.All", "Entity.CreateMany", "EntityQuery.Stream", "PageBuilder.All"} {
		if !hasEntry(census, want) {
			t.Fatalf("%s is no longer an executor — this census has gone stale", want)
		}
	}

	present := map[string]bool{}
	var missing []string
	for _, name := range census {
		present[name] = true
		if !covered[name] && exemptExecutors[name] == "" {
			missing = append(missing, name)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("these can send a statement for a scoped table and no case runs them: %v", missing)
	}
	for name := range covered {
		if !present[name] {
			t.Errorf("case %q names no executor any more — drop it or fix the name", name)
		}
	}
	for name, reason := range exemptExecutors {
		if reason == "" {
			t.Errorf("exempt executor %q carries no reason — an exemption without one is a leak with a name", name)
		}
		if !present[name] {
			t.Errorf("exempt executor %q is no longer an executor — drop the exemption", name)
		}
	}
}

// censusExecutors returns every door a statement for a caller's table
// can go out through: an exported function or method that is handed a
// context.Context and can reach a driver.
//
// "Can reach a driver" is the whole test, and it replaced a narrower
// one. The census used to also require the receiver to be a type that
// builds statements out of relations, which is true of the builders and
// of Entity and false of everything else that sends SQL — so CopyFrom,
// Subscribe, the Factory, VectorStore.Search and Seeder.Apply were
// outside it, and a package-level function that took a *DB and a ctx
// was outside it twice over. None of them is exempt from the axis: a
// statement is a statement whoever assembled it, and the tenant a
// scoped table demands is owed by the door that sends it, not by the
// door that looks like a builder.
//
// Which declarations can carry a *DB is computed by the same predicate
// the relation census uses for a *Table — see bearsRelation — so a
// receiver that holds its handle behind a local interface is censused
// like one that names the type.
func censusExecutors(t *testing.T, p *pgSyntax) []string {
	t.Helper()
	holdsDB := p.relationHolders("DB")

	carriesRelation := p.relationHolders("Table")
	renders := p.relationReceivers()

	// hasRelation: the door has a caller's relation in its hands,
	// either because the receiver carries one or because the signature
	// is handed one. The DB is the third case and the only named one:
	// it carries no relation of its own and renders whatever it is
	// given, which is what ExecExpr is.
	hasRelation := func(recv string, ft *ast.FuncType) bool {
		if recv == "DB" || carriesRelation[recv] || renders[recv] {
			return true
		}
		if ft.Params == nil {
			return false
		}
		for _, param := range ft.Params.List {
			// A parameter that IS an expression can be handed a *Table
			// — DB.ExecExpr is the door that is — and one that CARRIES
			// a relation is one too, which is how CopyFrom and
			// Subscribe are handed a caller's table through an
			// *Entity[T].
			//
			// `any` is neither, and this is the one place it is read
			// that way. A payload parameter — Notify's, the outbox
			// row's, the event store's — commits to nothing and names
			// no relation; reading it as one put every function with a
			// payload in this census, where there is no statement for
			// an axis to be carried by.
			if isExpressionType(elementType(param.Type)) || p.declaresRelation(param.Type, carriesRelation) {
				return true
			}
		}
		return false
	}

	var out []string
	for recv, ms := range p.methods {
		for name, fn := range ms {
			if !ast.IsExported(name) || !takesContext(fn.Type) {
				continue
			}
			// It has to be able to REACH a driver: the receiver holds a
			// *DB, or is handed one. Entity's methods take the *DB,
			// which is why this is not "holds" alone.
			if !holdsDB[recv] && !p.handedDB(fn.Type, holdsDB) {
				continue
			}
			if !hasRelation(recv, fn.Type) {
				continue
			}
			out = append(out, recv+"."+name)
		}
	}
	// A package-level function handed a ctx and a *DB sends statements
	// with nobody's receiver in the way, and the census could not see
	// one at all.
	for _, fn := range p.funcs {
		if !fn.Name.IsExported() || !takesContext(fn.Type) {
			continue
		}
		if !p.handedDB(fn.Type, holdsDB) || !hasRelation("", fn.Type) {
			continue
		}
		out = append(out, fn.Name.Name)
	}
	sort.Strings(out)
	return out
}

// handedDB reports whether a signature is given something that can
// reach a driver.
//
// It asks declaresRelation rather than bearsRelation: a parameter typed
// drops.Expression, or `any`, can be handed a *Table, which is why the
// relation census treats it as a door — but nobody hands a *DB through
// one, and reading every expression parameter as a database handle
// would make this census the package.
func (p *pgSyntax) handedDB(ft *ast.FuncType, holdsDB map[string]bool) bool {
	if ft.Params == nil {
		return false
	}
	for _, param := range ft.Params.List {
		if p.declaresRelation(param.Type, holdsDB) {
			return true
		}
	}
	return false
}
