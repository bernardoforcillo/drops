package mirror_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/bernardoforcillo/drops"
	"github.com/bernardoforcillo/drops/clickhouse"
	"github.com/bernardoforcillo/drops/mirror"
	"github.com/bernardoforcillo/drops/pg"
)

// The source an evolution starts from. Four columns is enough to have
// a key, a NOT NULL column, a nullable one and a neighbour to position
// an ADD COLUMN against.
func evolveSource() *pg.Table {
	t := pg.NewTable("docs")
	pg.Add(t, pg.BigSerial("id").PrimaryKey())
	pg.Add(t, pg.Text("title").NotNull())
	pg.Add(t, pg.Integer("views").NotNull())
	pg.Add(t, pg.Text("body"))
	return t
}

func evolver(t *testing.T, db *clickhouse.DB, src *pg.Table, opts ...mirror.DeriveOption) *mirror.Evolver {
	t.Helper()
	e, err := mirror.NewEvolver(db, src, opts...)
	if err != nil {
		t.Fatalf("NewEvolver: %v", err)
	}
	return e
}

// evolveLive renders a source table as the shape ClickHouse would
// report for a mirror derived from it — the "nothing has changed yet"
// starting point every test perturbs.
func evolveLive(t *testing.T, src *pg.Table, opts ...mirror.DeriveOption) []mirror.MirrorColumn {
	t.Helper()
	tbl, err := mirror.DeriveClickHouse(src, opts...)
	if err != nil {
		t.Fatalf("DeriveClickHouse: %v", err)
	}
	key := map[string]bool{}
	for _, name := range tbl.OrderByColumns() {
		key[name] = true
	}
	out := make([]mirror.MirrorColumn, 0, len(tbl.Columns()))
	for _, c := range tbl.Columns() {
		out = append(out, mirror.MirrorColumn{
			Name: c.Name(),
			Type: c.Type().TypeSQL(),
			// A derived mirror's only key is its sorting key, so
			// both flags say the same thing here. They are set
			// separately because a plan compares the sorting key on
			// its own: a shape that left InSortingKey false would be
			// a mirror that sorts by nothing.
			InKey:        key[c.Name()],
			InSortingKey: key[c.Name()],
		})
	}
	return out
}

// evolvePartitioned marks a column as taking part in the live table's
// partition key, which is how ClickHouse reports PARTITION BY: the
// columns the expression names, never the expression.
func evolvePartitioned(t *testing.T, live []mirror.MirrorColumn, name string) []mirror.MirrorColumn {
	t.Helper()
	out := append([]mirror.MirrorColumn(nil), live...)
	for i := range out {
		if out[i].Name == name {
			out[i].InKey = true
			out[i].InPartitionKey = true
			return out
		}
	}
	t.Fatalf("no column %q in the live shape", name)
	return nil
}

func evolveWithout(live []mirror.MirrorColumn, name string) []mirror.MirrorColumn {
	out := make([]mirror.MirrorColumn, 0, len(live))
	for _, c := range live {
		if c.Name != name {
			out = append(out, c)
		}
	}
	return out
}

func evolveRetyped(t *testing.T, live []mirror.MirrorColumn, name, typ string) []mirror.MirrorColumn {
	t.Helper()
	out := append([]mirror.MirrorColumn(nil), live...)
	for i := range out {
		if out[i].Name == name {
			out[i].Type = typ
			return out
		}
	}
	t.Fatalf("no column %q in the live shape", name)
	return nil
}

func evolvePlus(live []mirror.MirrorColumn, extra mirror.MirrorColumn) []mirror.MirrorColumn {
	return append(append([]mirror.MirrorColumn(nil), live...), extra)
}

func evolveStepFor(t *testing.T, p mirror.EvolutionPlan, column string) mirror.EvolutionStep {
	t.Helper()
	for _, s := range p.Steps {
		if s.Column == column {
			return s
		}
	}
	t.Fatalf("no step for column %q; plan = %+v", column, p)
	return mirror.EvolutionStep{}
}

func evolveRefusalFor(t *testing.T, p mirror.EvolutionPlan, column string) mirror.Refusal {
	t.Helper()
	for _, r := range p.Refusals {
		if r.Column == column {
			return r
		}
	}
	t.Fatalf("no refusal for column %q; plan = %+v", column, p)
	return mirror.Refusal{}
}

// --- fake driver -----------------------------------------------------

// evolveDriver answers the one query an evolution issues — the
// system.columns read — from a canned shape, and records every
// statement Apply runs. Recording the SQL rather than a parsed form is
// deliberate: an ADD COLUMN that quietly stopped saying IF NOT EXISTS
// still applies cleanly against a fake, and only the text shows it.
type evolveDriver struct {
	live []mirror.MirrorColumn

	queries   []string
	queryArgs [][]any
	execs     []string
	execArgs  [][]any

	queryErr error
	execErr  error
}

func (d *evolveDriver) Query(_ context.Context, sql string, args ...any) (drops.Rows, error) {
	d.queries = append(d.queries, sql)
	d.queryArgs = append(d.queryArgs, args)
	if d.queryErr != nil {
		return nil, d.queryErr
	}
	data := make([][]any, 0, len(d.live))
	for _, c := range d.live {
		// The three flags are answered separately so a shape
		// survives the round trip through InspectMirror: a column
		// that is in a key but in neither of the two named ones is
		// reported through is_in_primary_key, which is where
		// ClickHouse would put it.
		var sorting, primary, part uint8
		if c.InSortingKey {
			sorting = 1
		}
		if c.InPartitionKey {
			part = 1
		}
		if c.InKey && !c.InSortingKey && !c.InPartitionKey {
			primary = 1
		}
		data = append(data, []any{c.Name, c.Type, sorting, primary, part})
	}
	return &evolveRows{data: data}, nil
}

func (d *evolveDriver) Exec(_ context.Context, sql string, args ...any) (drops.Result, error) {
	d.execs = append(d.execs, sql)
	d.execArgs = append(d.execArgs, args)
	if d.execErr != nil {
		return nil, d.execErr
	}
	return evolveResult{}, nil
}

func (d *evolveDriver) Begin(context.Context) (drops.Tx, error) {
	return nil, errors.New("unexpected Begin")
}

type evolveResult struct{}

func (evolveResult) RowsAffected() (int64, error) { return 0, nil }

type evolveRows struct {
	data [][]any
	pos  int
}

func (r *evolveRows) Next() bool {
	if r.pos >= len(r.data) {
		return false
	}
	r.pos++
	return true
}

func (r *evolveRows) Scan(dest ...any) error {
	row := r.data[r.pos-1]
	for i, d := range dest {
		if i >= len(row) {
			break
		}
		reflect.ValueOf(d).Elem().Set(reflect.ValueOf(row[i]))
	}
	return nil
}

func (r *evolveRows) Columns() ([]string, error) { return nil, nil }
func (r *evolveRows) Close() error               { return nil }
func (r *evolveRows) Err() error                 { return nil }

// --- planning --------------------------------------------------------

func TestEvolveIsSilentWhenTheMirrorMatches(t *testing.T) {
	src := evolveSource()
	e := evolver(t, clickhouse.New(&evolveDriver{}), src)
	plan := e.PlanAgainst(evolveLive(t, src))
	if !plan.Aligned() {
		t.Fatalf("an untouched mirror should need nothing: %+v", plan)
	}
	if len(plan.Statements()) != 0 {
		t.Errorf("statements = %v", plan.Statements())
	}
}

func TestEvolveAddsAColumnTheSourceGained(t *testing.T) {
	src := evolveSource()
	live := evolveWithout(evolveLive(t, src), "body")

	e := evolver(t, clickhouse.New(&evolveDriver{}), src)
	plan := e.PlanAgainst(live)

	if len(plan.Refusals) != 0 {
		t.Fatalf("adding a column needs no permission: %v", plan.Refusals)
	}
	step := evolveStepFor(t, plan, "body")
	if step.Kind != mirror.EvolveAddColumn {
		t.Errorf("kind = %s", step.Kind)
	}
	if !strings.Contains(step.SQL, `ADD COLUMN IF NOT EXISTS "body" Nullable(String)`) {
		t.Errorf("SQL = %s", step.SQL)
	}
	// Positioned, so the mirror keeps the source's column order and
	// the bookkeeping columns stay at the end where they were put.
	if !strings.Contains(step.SQL, `AFTER "views"`) {
		t.Errorf("ADD COLUMN did not name its predecessor: %s", step.SQL)
	}
	if step.To != "Nullable(String)" {
		t.Errorf("To = %q", step.To)
	}
}

func TestEvolveAddsALeadingColumnFirst(t *testing.T) {
	src := evolveSource()
	live := evolveWithout(evolveLive(t, src), "id")

	e := evolver(t, clickhouse.New(&evolveDriver{}), src)
	step := evolveStepFor(t, e.PlanAgainst(live), "id")
	if !strings.Contains(step.SQL, " FIRST") {
		t.Errorf("the first derived column has no predecessor to sit after: %s", step.SQL)
	}
}

func TestEvolveCreatesTheTableWhenClickHouseHasNone(t *testing.T) {
	src := evolveSource()
	e := evolver(t, clickhouse.New(&evolveDriver{}), src)
	plan := e.PlanAgainst(nil)

	if len(plan.Steps) != 1 || plan.Steps[0].Kind != mirror.EvolveCreateTable {
		t.Fatalf("an absent table is a CREATE, not a pile of ALTERs: %+v", plan.Steps)
	}
	sql := plan.Steps[0].SQL
	for _, want := range []string{
		`CREATE TABLE IF NOT EXISTS "docs"`,
		`"` + mirror.VersionColumn + `" UInt64`,
		`ReplacingMergeTree`,
	} {
		if !strings.Contains(sql, want) {
			t.Errorf("CREATE missing %s\ngot %s", want, sql)
		}
	}
}

// --- refusals --------------------------------------------------------

func TestEvolveRefusesToDropWithoutBeingTold(t *testing.T) {
	src := evolveSource()
	live := evolvePlus(evolveLive(t, src), mirror.MirrorColumn{Name: "legacy", Type: "String"})
	db := clickhouse.New(&evolveDriver{})

	plan := evolver(t, db, src).PlanAgainst(live)
	if len(plan.Steps) != 0 {
		t.Fatalf("a drop is not something to decide for the caller: %+v", plan.Steps)
	}
	r := evolveRefusalFor(t, plan, "legacy")
	if r.Kind != mirror.EvolveDropColumn || r.Reason != mirror.RefusedNeedsOptIn {
		t.Errorf("refusal = %+v", r)
	}
	if !strings.Contains(r.Detail, "AllowDrop") {
		t.Errorf("the refusal must say how to clear it: %s", r.Detail)
	}
	if !errors.Is(plan.Refused(), mirror.ErrEvolutionRefused) {
		t.Errorf("Refused() = %v", plan.Refused())
	}
	if plan.Aligned() {
		t.Error("a plan that refuses something has not aligned anything")
	}

	// Named, and only named, the drop is emitted.
	allowed := evolver(t, db, src).AllowDrop("legacy").PlanAgainst(live)
	if len(allowed.Refusals) != 0 {
		t.Fatalf("still refused after AllowDrop: %v", allowed.Refusals)
	}
	step := evolveStepFor(t, allowed, "legacy")
	if !strings.Contains(step.SQL, `DROP COLUMN IF EXISTS "legacy"`) {
		t.Errorf("SQL = %s", step.SQL)
	}
}

// The opt-in is per column so that drift which appears between the
// review and the apply is still refused rather than swept along.
func TestAllowDropCoversOnlyTheNamedColumn(t *testing.T) {
	src := evolveSource()
	live := evolvePlus(
		evolvePlus(evolveLive(t, src), mirror.MirrorColumn{Name: "legacy", Type: "String"}),
		mirror.MirrorColumn{Name: "also_legacy", Type: "String"})

	plan := evolver(t, clickhouse.New(&evolveDriver{}), src).AllowDrop("legacy").PlanAgainst(live)
	if len(plan.Steps) != 1 || plan.Steps[0].Column != "legacy" {
		t.Fatalf("steps = %+v", plan.Steps)
	}
	if r := evolveRefusalFor(t, plan, "also_legacy"); r.Reason != mirror.RefusedNeedsOptIn {
		t.Errorf("refusal = %+v", r)
	}
}

// A drop and an add of the same type are the same diff as a rename,
// and only one of the two keeps the data.
func TestEvolveFlagsAPossibleRename(t *testing.T) {
	before := evolveSource()
	after := pg.NewTable("docs")
	pg.Add(after, pg.BigSerial("id").PrimaryKey())
	pg.Add(after, pg.Text("heading").NotNull())
	pg.Add(after, pg.Integer("views").NotNull())
	pg.Add(after, pg.Text("body"))

	plan := evolver(t, clickhouse.New(&evolveDriver{}), after).PlanAgainst(evolveLive(t, before))

	if s := evolveStepFor(t, plan, "heading"); s.Kind != mirror.EvolveAddColumn {
		t.Errorf("the new column is still added: %+v", s)
	}
	r := evolveRefusalFor(t, plan, "title")
	if r.Reason != mirror.RefusedAmbiguous {
		t.Fatalf("reason = %s, want the rename suspicion; detail = %s", r.Reason, r.Detail)
	}
	for _, want := range []string{`"heading"`, "RENAME COLUMN", "AllowDrop"} {
		if !strings.Contains(r.Detail, want) {
			t.Errorf("refusal detail missing %q: %s", want, r.Detail)
		}
	}
}

func TestEvolveRefusesToTouchAKeyColumn(t *testing.T) {
	src := evolveSource()
	db := clickhouse.New(&evolveDriver{})

	t.Run("type", func(t *testing.T) {
		live := evolveRetyped(t, evolveLive(t, src), "id", "Int32")
		r := evolveRefusalFor(t, evolver(t, db, src).PlanAgainst(live), "id")
		if r.Reason != mirror.RefusedNotInPlace {
			t.Fatalf("reason = %s: ClickHouse cannot ALTER a key column", r.Reason)
		}
		// No opt-in exists for something the server will not do.
		again := evolver(t, db, src).AllowTypeChange("id").PlanAgainst(live)
		if len(again.Steps) != 0 {
			t.Errorf("AllowTypeChange must not conjure an impossible ALTER: %+v", again.Steps)
		}
		if evolveRefusalFor(t, again, "id").Reason != mirror.RefusedNotInPlace {
			t.Error("the refusal should survive the opt-in")
		}
	})

	t.Run("drop", func(t *testing.T) {
		rekeyed := pg.NewTable("docs")
		pg.Add(rekeyed, pg.BigSerial("doc_id").PrimaryKey())
		pg.Add(rekeyed, pg.Text("title").NotNull())
		pg.Add(rekeyed, pg.Integer("views").NotNull())
		pg.Add(rekeyed, pg.Text("body"))

		plan := evolver(t, db, rekeyed).AllowDrop("id").PlanAgainst(evolveLive(t, src))
		r := evolveRefusalFor(t, plan, "id")
		if r.Kind != mirror.EvolveDropColumn || r.Reason != mirror.RefusedNotInPlace {
			t.Fatalf("refusal = %+v", r)
		}
		for _, s := range plan.Steps {
			if s.Kind == mirror.EvolveDropColumn {
				t.Errorf("emitted a drop ClickHouse would reject: %s", s.SQL)
			}
		}
	})
}

// Every tombstone this package writes leaves NULL in the non-key
// columns, so a column that stops being Nullable cannot be converted
// in place — the mutation dies on the first mirrored delete.
func TestEvolveRefusesToRemoveNullable(t *testing.T) {
	before := evolveSource()
	after := pg.NewTable("docs")
	pg.Add(after, pg.BigSerial("id").PrimaryKey())
	pg.Add(after, pg.Text("title").NotNull())
	pg.Add(after, pg.Integer("views").NotNull())
	pg.Add(after, pg.Text("body").NotNull())

	db := clickhouse.New(&evolveDriver{})
	live := evolveLive(t, before)

	r := evolveRefusalFor(t, evolver(t, db, after).PlanAgainst(live), "body")
	if r.Reason != mirror.RefusedNotInPlace {
		t.Fatalf("reason = %s; detail = %s", r.Reason, r.Detail)
	}
	if !strings.Contains(r.Detail, "tombstone") {
		t.Errorf("the refusal should name the reason it cannot work: %s", r.Detail)
	}
	if plan := evolver(t, db, after).AllowTypeChange("body").PlanAgainst(live); len(plan.Steps) != 0 {
		t.Errorf("AllowTypeChange must not emit a MODIFY that is certain to fail: %+v", plan.Steps)
	}
}

// --- type changes ----------------------------------------------------

func TestEvolveWidensWithoutAsking(t *testing.T) {
	src := evolveSource()
	db := clickhouse.New(&evolveDriver{})

	t.Run("integer", func(t *testing.T) {
		live := evolveRetyped(t, evolveLive(t, src), "views", "Int16")
		plan := evolver(t, db, src).PlanAgainst(live)
		if len(plan.Refusals) != 0 {
			t.Fatalf("a widening loses nothing: %v", plan.Refusals)
		}
		step := evolveStepFor(t, plan, "views")
		if step.Kind != mirror.EvolveModifyColumn || step.From != "Int16" || step.To != "Int32" {
			t.Fatalf("step = %+v", step)
		}
		if !strings.Contains(step.SQL, `MODIFY COLUMN IF EXISTS "views" Int32`) {
			t.Errorf("SQL = %s", step.SQL)
		}
	})

	t.Run("gaining nullable", func(t *testing.T) {
		live := evolveRetyped(t, evolveLive(t, src), "body", "String")
		step := evolveStepFor(t, evolver(t, db, src).PlanAgainst(live), "body")
		if !strings.Contains(step.SQL, `MODIFY COLUMN IF EXISTS "body" Nullable(String)`) {
			t.Errorf("SQL = %s", step.SQL)
		}
	})
}

func TestEvolveTreatsAnUnprovableCastAsAnOptIn(t *testing.T) {
	src := evolveSource()
	db := clickhouse.New(&evolveDriver{})

	for name, liveType := range map[string]string{
		"narrowing":     "Int64",
		"other family":  "Float64",
		"unsigned":      "UInt32",
		"from a string": "String",
	} {
		t.Run(name, func(t *testing.T) {
			live := evolveRetyped(t, evolveLive(t, src), "views", liveType)
			plan := evolver(t, db, src).PlanAgainst(live)
			if len(plan.Steps) != 0 {
				t.Fatalf("%s to Int32 is not provably lossless: %+v", liveType, plan.Steps)
			}
			r := evolveRefusalFor(t, plan, "views")
			if r.Reason != mirror.RefusedNeedsOptIn {
				t.Fatalf("refusal = %+v", r)
			}

			allowed := evolver(t, db, src).AllowTypeChange("views").PlanAgainst(live)
			if len(allowed.Refusals) != 0 {
				t.Fatalf("still refused after AllowTypeChange: %v", allowed.Refusals)
			}
			if s := evolveStepFor(t, allowed, "views"); !strings.Contains(s.SQL, `MODIFY COLUMN IF EXISTS "views" Int32`) {
				t.Errorf("SQL = %s", s.SQL)
			}
		})
	}
}

// The server echoes types in its own spelling, and a mirror that
// reported drift on a stray space would cry wolf on every run.
func TestEvolveComparesTypesNotFormatting(t *testing.T) {
	src := pg.NewTable("docs")
	pg.Add(src, pg.BigSerial("id").PrimaryKey())
	pg.Add(src, pg.Numeric("price", 10, 2))
	pg.Add(src, pg.Timestamp("created_at", true).NotNull())

	live := evolveLive(t, src)
	live = evolveRetyped(t, live, "price", "Nullable(Decimal(10, 2))")
	live = evolveRetyped(t, live, "created_at", "DateTime64(6,'UTC')")

	plan := evolver(t, clickhouse.New(&evolveDriver{}), src).PlanAgainst(live)
	if !plan.Aligned() {
		t.Fatalf("whitespace is not drift: steps %+v refusals %+v", plan.Steps, plan.Refusals)
	}
}

// LowCardinality changes how values are stored, not which values can
// be stored, so a hand-applied one is tuning to respect rather than
// drift to undo.
func TestEvolveLeavesLowCardinalityAlone(t *testing.T) {
	src := evolveSource()
	live := evolveRetyped(t, evolveLive(t, src), "title", "LowCardinality(String)")

	plan := evolver(t, clickhouse.New(&evolveDriver{}), src).PlanAgainst(live)
	if !plan.Aligned() {
		t.Fatalf("steps %+v refusals %+v", plan.Steps, plan.Refusals)
	}
}

// The bookkeeping columns are the mirror's own, so a source that has
// never heard of them is no argument for taking them away.
func TestEvolveNeverProposesRemovingTheBookkeepingColumns(t *testing.T) {
	src := evolveSource()
	// The source has gained a column, so the drop pass runs with
	// something real to compare against.
	grown := evolveSource()
	pg.Add(grown, pg.Text("summary"))

	plan := evolver(t, clickhouse.New(&evolveDriver{}), grown).PlanAgainst(evolveLive(t, src))
	if len(plan.Refusals) != 0 {
		t.Fatalf("nothing here is destructive: %v", plan.Refusals)
	}
	for _, s := range plan.Steps {
		if s.Column == mirror.VersionColumn || s.Column == mirror.DeletedColumn {
			t.Errorf("proposed %s on a bookkeeping column: %s", s.Kind, s.SQL)
		}
	}
	if len(plan.Steps) != 1 || plan.Steps[0].Column != "summary" {
		t.Errorf("steps = %+v", plan.Steps)
	}
}

// --- introspection ---------------------------------------------------

func TestInspectMirrorReadsSystemColumns(t *testing.T) {
	src := evolveSource()
	d := &evolveDriver{live: evolveLive(t, src)}
	e := evolver(t, clickhouse.New(d), src)

	got, err := mirror.InspectMirror(context.Background(), clickhouse.New(d), e.Target())
	if err != nil {
		t.Fatalf("InspectMirror: %v", err)
	}
	if len(got) != len(d.live) {
		t.Fatalf("read %d columns, want %d", len(got), len(d.live))
	}
	if got[0].Name != "id" || !got[0].InKey {
		t.Errorf("the sorting key column should come back flagged: %+v", got[0])
	}
	if got[1].InKey {
		t.Errorf("a plain column should not be flagged: %+v", got[1])
	}
	if len(d.queries) != 1 {
		t.Fatalf("queries = %v", d.queries)
	}
	for _, want := range []string{"system.columns", "is_in_sorting_key", "is_in_partition_key", "currentDatabase()", "ORDER BY position"} {
		if !strings.Contains(d.queries[0], want) {
			t.Errorf("query missing %q: %s", want, d.queries[0])
		}
	}
	if len(d.queryArgs[0]) != 1 || d.queryArgs[0][0] != "docs" {
		t.Errorf("args = %v", d.queryArgs[0])
	}
}

func TestInspectMirrorScopesToTheDeclaredDatabase(t *testing.T) {
	src := evolveSource()
	d := &evolveDriver{}
	e := evolver(t, clickhouse.New(d), src, mirror.WithDatabase("analytics"))

	if _, err := mirror.InspectMirror(context.Background(), clickhouse.New(d), e.Target()); err != nil {
		t.Fatalf("InspectMirror: %v", err)
	}
	if strings.Contains(d.queries[0], "currentDatabase()") {
		t.Errorf("an explicit database must not fall back to the session's: %s", d.queries[0])
	}
	if len(d.queryArgs[0]) != 2 || d.queryArgs[0][0] != "analytics" || d.queryArgs[0][1] != "docs" {
		t.Errorf("args = %v", d.queryArgs[0])
	}
}

func TestInspectMirrorReportsAFailedRead(t *testing.T) {
	src := evolveSource()
	boom := errors.New("connection reset")
	d := &evolveDriver{queryErr: boom}
	e := evolver(t, clickhouse.New(d), src)

	if _, err := e.Plan(context.Background()); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the driver's", err)
	}
}

// --- applying --------------------------------------------------------

func TestPlanRunsNoDDL(t *testing.T) {
	src := evolveSource()
	d := &evolveDriver{live: evolveWithout(evolveLive(t, src), "body")}
	e := evolver(t, clickhouse.New(d), src)

	plan, err := e.Plan(context.Background())
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if len(plan.Steps) != 1 {
		t.Fatalf("steps = %+v", plan.Steps)
	}
	if len(d.execs) != 0 {
		t.Errorf("Plan executed %v", d.execs)
	}
}

func TestApplyRunsTheStatements(t *testing.T) {
	src := evolveSource()
	d := &evolveDriver{live: evolveWithout(evolveLive(t, src), "body")}
	e := evolver(t, clickhouse.New(d), src)

	plan, err := e.Apply(context.Background())
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if got := d.execs; !reflect.DeepEqual(got, plan.Statements()) {
		t.Errorf("executed %v, plan said %v", got, plan.Statements())
	}
}

func TestApplyRunsNothingWhenAnythingIsRefused(t *testing.T) {
	src := evolveSource()
	live := evolvePlus(evolveWithout(evolveLive(t, src), "body"),
		mirror.MirrorColumn{Name: "legacy", Type: "String"})
	d := &evolveDriver{live: live}
	e := evolver(t, clickhouse.New(d), src)

	plan, err := e.Apply(context.Background())
	if !errors.Is(err, mirror.ErrEvolutionRefused) {
		t.Fatalf("err = %v, want ErrEvolutionRefused", err)
	}
	if len(d.execs) != 0 {
		t.Errorf("a plan drops does not understand in full must not be run in part: %v", d.execs)
	}
	if len(plan.Steps) != 1 || len(plan.Refusals) != 1 {
		t.Errorf("the plan should still describe both halves: %+v", plan)
	}
}

func TestAllowPartialRunsTheSafeStepsAndStillReports(t *testing.T) {
	src := evolveSource()
	live := evolvePlus(evolveWithout(evolveLive(t, src), "body"),
		mirror.MirrorColumn{Name: "legacy", Type: "Int64"})
	d := &evolveDriver{live: live}
	e := evolver(t, clickhouse.New(d), src).AllowPartial()

	plan, err := e.Apply(context.Background())
	if !errors.Is(err, mirror.ErrEvolutionRefused) {
		t.Fatalf("err = %v: a partial apply is not a finished one", err)
	}
	if len(d.execs) != 1 || !strings.Contains(d.execs[0], `ADD COLUMN IF NOT EXISTS "body"`) {
		t.Errorf("executed %v", d.execs)
	}
	if len(plan.Refusals) != 1 {
		t.Errorf("refusals = %v", plan.Refusals)
	}
}

func TestApplyReportsTheFailingStatement(t *testing.T) {
	src := evolveSource()
	boom := errors.New("TOO_MANY_PARTS")
	d := &evolveDriver{live: evolveWithout(evolveLive(t, src), "body"), execErr: boom}
	e := evolver(t, clickhouse.New(d), src)

	_, err := e.Apply(context.Background())
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the driver's", err)
	}
	if !strings.Contains(err.Error(), "ADD COLUMN") {
		t.Errorf("the error should name the statement that failed: %v", err)
	}
}

// --- construction ----------------------------------------------------

func TestNewEvolverRejectsWhatItCannotMirror(t *testing.T) {
	t.Run("no db", func(t *testing.T) {
		if _, err := mirror.NewEvolver(nil, evolveSource()); err == nil {
			t.Error("expected an error without a ClickHouse db")
		}
	})

	t.Run("no source", func(t *testing.T) {
		if _, err := mirror.NewEvolver(clickhouse.New(&evolveDriver{}), nil); err == nil {
			t.Error("expected an error without a source table")
		}
	})

	// The target is derived, so a source that cannot be mirrored at
	// all fails here rather than at the first Plan.
	t.Run("no primary key", func(t *testing.T) {
		tbl := pg.NewTable("nokey")
		pg.Add(tbl, pg.Text("a"))
		if _, err := mirror.NewEvolver(clickhouse.New(&evolveDriver{}), tbl); err == nil {
			t.Error("expected an error for a keyless source")
		}
	})
}

func TestTargetIsAFreshTable(t *testing.T) {
	src := evolveSource()
	db := clickhouse.New(&evolveDriver{})
	first := evolver(t, db, src).Target()
	second := evolver(t, db, src).Target()

	// A live sink holds the table it was built with and snapshots its
	// columns; handing back that same table would let an evolution
	// mutate a running sink's view of the schema.
	if first == second {
		t.Error("two evolvers must not share one table")
	}
	if first.Col(mirror.VersionColumn) == nil || first.Col(mirror.DeletedColumn) == nil {
		t.Error("the target is what the replacement sink is built from and must carry the bookkeeping columns")
	}
}

// --- the table's own shape ------------------------------------------

// evolveKeyedSource is evolveSource's shape with a second candidate
// key, so a migration can move the primary key without changing a
// single derived column type — the case a column diff cannot see.
func evolveKeyedSource(t *testing.T, key string) *pg.Table {
	t.Helper()
	tbl := pg.NewTable("docs")
	if key == "id" {
		pg.Add(tbl, pg.BigSerial("id").PrimaryKey())
		pg.Add(tbl, pg.Text("slug").NotNull())
	} else {
		pg.Add(tbl, pg.BigInt("id").NotNull())
		pg.Add(tbl, pg.Text("slug").PrimaryKey())
	}
	pg.Add(tbl, pg.Text("title").NotNull())
	return tbl
}

// A mirror whose ORDER BY is no longer the derived one is broken in a
// way no column type shows: ReplacingMergeTree deduplicates on the
// sorting key and ClickHouseSink addresses a tombstone by the derived
// one, so every delete would land under a key the table does not sort
// by and the deleted rows would stay readable for good.
func TestEvolveRefusesASortingKeyThatMoved(t *testing.T) {
	before := evolveKeyedSource(t, "id")
	after := evolveKeyedSource(t, "slug")
	live := evolveLive(t, before)

	// The premise: nothing about the columns changed.
	sameShape := evolveLive(t, after)
	for i := range live {
		if live[i].Name != sameShape[i].Name || live[i].Type != sameShape[i].Type {
			t.Fatalf("column %d differs (%+v vs %+v); the test needs a pure key change", i, live[i], sameShape[i])
		}
	}

	d := &evolveDriver{live: live}
	plan := evolver(t, clickhouse.New(d), after).PlanAgainst(live)

	if len(plan.Steps) != 0 {
		t.Fatalf("there is no ALTER for a key change: %+v", plan.Steps)
	}
	if plan.Aligned() {
		t.Fatal("a mirror sorted by the wrong column is not aligned")
	}
	if len(plan.Refusals) != 1 {
		t.Fatalf("refusals = %+v", plan.Refusals)
	}
	r := plan.Refusals[0]
	if r.Kind != mirror.EvolveSortingKey || r.Reason != mirror.RefusedNotInPlace {
		t.Fatalf("refusal = %+v", r)
	}
	for _, want := range []string{`"id"`, `"slug"`, "tombstone", "MODIFY ORDER BY"} {
		if !strings.Contains(r.Detail, want) {
			t.Errorf("the refusal must name %s: %s", want, r.Detail)
		}
	}
	// No column, so no empty pair of quotes in the review line.
	if strings.Contains(r.String(), `""`) {
		t.Errorf("Refusal.String = %s", r.String())
	}

	// And it stops an Apply, which is the whole point: the operator
	// who ignores the plan and builds a sink from Target() is the one
	// who loses the deletes.
	e := evolver(t, clickhouse.New(d), after)
	if _, err := e.Apply(context.Background()); !errors.Is(err, mirror.ErrEvolutionRefused) {
		t.Fatalf("Apply err = %v, want ErrEvolutionRefused", err)
	}
	if len(d.execs) != 0 {
		t.Errorf("Apply ran %v", d.execs)
	}
}

// The same blindness in the other direction: a mirror that is
// partitioned when the derived table is not, or the reverse.
// ReplacingMergeTree only collapses rows inside one partition, so this
// decides whether the mirror converges at all.
func TestEvolveRefusesPartitioningThatDoesNotMatch(t *testing.T) {
	src := pg.NewTable("docs")
	pg.Add(src, pg.BigSerial("id").PrimaryKey())
	pg.Add(src, pg.Text("title").NotNull())
	madeAt := pg.Add(src, pg.Timestamp("created_at", true).NotNull())

	t.Run("live has one and the declaration does not", func(t *testing.T) {
		live := evolvePartitioned(t, evolveLive(t, src), "created_at")
		plan := evolver(t, clickhouse.New(&evolveDriver{}), src).PlanAgainst(live)
		// Length first: indexing an empty slice would panic, and a
		// panic takes the whole test binary down with it — the
		// failure this test exists to report would be the last thing
		// the run said.
		if len(plan.Refusals) != 1 {
			t.Fatalf("refusals = %+v", plan.Refusals)
		}
		r := plan.Refusals[0]
		if r.Kind != mirror.EvolvePartitionKey || r.Reason != mirror.RefusedNotInPlace {
			t.Fatalf("refusal = %+v", r)
		}
		if !strings.Contains(r.Detail, "partition") {
			t.Errorf("detail = %s", r.Detail)
		}
	})

	t.Run("the declaration has one and live does not", func(t *testing.T) {
		part := mirror.WithPartitionBy(clickhouse.Func("toYYYYMM", madeAt))
		live := evolveLive(t, src, part)
		plan := evolver(t, clickhouse.New(&evolveDriver{}), src, part).PlanAgainst(live)
		if len(plan.Refusals) != 1 || plan.Refusals[0].Kind != mirror.EvolvePartitionKey {
			t.Fatalf("refusals = %+v", plan.Refusals)
		}
	})

	t.Run("both, and they agree as far as this can tell", func(t *testing.T) {
		part := mirror.WithPartitionBy(clickhouse.Func("toYYYYMM", madeAt))
		live := evolvePartitioned(t, evolveLive(t, src, part), "created_at")
		plan := evolver(t, clickhouse.New(&evolveDriver{}), src, part).PlanAgainst(live)
		if !plan.Aligned() {
			t.Fatalf("steps %+v refusals %+v", plan.Steps, plan.Refusals)
		}
	})
}

// InspectMirror has to hand the plan the sorting key on its own:
// is_in_sorting_key is what the comparison runs on, and folding it
// into InKey with the partition columns would make a partitioned
// mirror look like one keyed on the partition column.
func TestInspectMirrorKeepsTheKeyFlagsApart(t *testing.T) {
	src := pg.NewTable("docs")
	pg.Add(src, pg.BigSerial("id").PrimaryKey())
	madeAt := pg.Add(src, pg.Timestamp("created_at", true).NotNull())
	part := mirror.WithPartitionBy(clickhouse.Func("toYYYYMM", madeAt))

	live := evolvePartitioned(t, evolveLive(t, src, part), "created_at")
	d := &evolveDriver{live: live}
	e := evolver(t, clickhouse.New(d), src, part)

	got, err := mirror.InspectMirror(context.Background(), clickhouse.New(d), e.Target())
	if err != nil {
		t.Fatalf("InspectMirror: %v", err)
	}
	byName := map[string]mirror.MirrorColumn{}
	for _, c := range got {
		byName[c.Name] = c
	}
	if id := byName["id"]; !id.InSortingKey || id.InPartitionKey || !id.InKey {
		t.Errorf("the sorting key column came back as %+v", id)
	}
	if at := byName["created_at"]; at.InSortingKey || !at.InPartitionKey || !at.InKey {
		t.Errorf("the partition column came back as %+v", at)
	}
	if title := byName["title"]; title.InKey || title.InSortingKey || title.InPartitionKey {
		t.Errorf("a plain column came back as %+v", title)
	}
}

// --- statements ------------------------------------------------------

// Every statement has to name the database the evolver was built for.
// An unqualified ALTER runs against whatever database the session
// happens to be in, which is a different table with the same name —
// and the assertions elsewhere in this file all read the clause after
// ALTER TABLE, so nothing else here would notice.
func TestGeneratedStatementsNameTheQualifiedTable(t *testing.T) {
	src := evolveSource()
	grown := evolveSource()
	pg.Add(grown, pg.Text("summary"))

	const qualified = `"analytics"."docs"`
	db := clickhouse.New(&evolveDriver{})

	// One plan carrying an add, a modify and a drop, plus the create
	// that a missing table produces.
	live := evolvePlus(
		evolveRetyped(t, evolveLive(t, src), "views", "Int16"),
		mirror.MirrorColumn{Name: "legacy", Type: "String"})
	plan := evolver(t, db, grown, mirror.WithDatabase("analytics")).
		AllowDrop("legacy").PlanAgainst(live)

	kinds := map[mirror.EvolutionKind]bool{}
	for _, s := range plan.Steps {
		kinds[s.Kind] = true
		if !strings.HasPrefix(s.SQL, "ALTER TABLE "+qualified+" ") {
			t.Errorf("%s does not alter %s: %s", s.Kind, qualified, s.SQL)
		}
	}
	for _, want := range []mirror.EvolutionKind{mirror.EvolveAddColumn, mirror.EvolveModifyColumn, mirror.EvolveDropColumn} {
		if !kinds[want] {
			t.Errorf("the plan should exercise %s: %+v", want, plan.Steps)
		}
	}

	created := evolver(t, db, grown, mirror.WithDatabase("analytics")).PlanAgainst(nil)
	if sql := created.Steps[0].SQL; !strings.Contains(sql, "CREATE TABLE IF NOT EXISTS "+qualified) {
		t.Errorf("CREATE does not name %s: %s", qualified, sql)
	}
}

// A MODIFY is the one statement that aborts the rest of the plan when
// it loses a race, because Apply stops at the first failure — and the
// step queued behind it is usually the ADD COLUMN that stops the live
// sink rejecting every batch. IF EXISTS turns that abort into a
// skipped statement, which is what Apply's doc promises for every
// statement it runs.
func TestModifyColumnToleratesAColumnThatIsAlreadyGone(t *testing.T) {
	src := evolveSource()
	live := evolveRetyped(t, evolveLive(t, src), "views", "Int16")

	step := evolveStepFor(t, evolver(t, clickhouse.New(&evolveDriver{}), src).PlanAgainst(live), "views")
	if !strings.Contains(step.SQL, `MODIFY COLUMN IF EXISTS "views" Int32`) {
		t.Errorf("SQL = %s", step.SQL)
	}
	for _, s := range evolver(t, clickhouse.New(&evolveDriver{}), src).PlanAgainst(live).Steps {
		if strings.Contains(s.SQL, "ALTER TABLE") && !strings.Contains(s.SQL, "IF EXISTS") && !strings.Contains(s.SQL, "IF NOT EXISTS") {
			t.Errorf("statement carries no existence guard: %s", s.SQL)
		}
	}
}

// A DeriveOption may bind a value — a partition bucket of so many
// days — and then the DDL is not self-contained text. Dropping the
// args sends ClickHouse a statement with a bare ? in it.
func TestCreateTableCarriesTheArgsItsDDLBinds(t *testing.T) {
	src := pg.NewTable("docs")
	pg.Add(src, pg.BigSerial("id").PrimaryKey())
	madeAt := pg.Add(src, pg.Timestamp("created_at", true).NotNull())

	d := &evolveDriver{} // no columns: the mirror does not exist yet
	e := evolver(t, clickhouse.New(d), src, mirror.WithPartitionBy(
		clickhouse.Func("toStartOfInterval", madeAt, clickhouse.Func("toIntervalDay", 7))))

	plan, err := e.Apply(context.Background())
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(plan.Steps) != 1 || plan.Steps[0].Kind != mirror.EvolveCreateTable {
		t.Fatalf("steps = %+v", plan.Steps)
	}
	if !strings.Contains(plan.Steps[0].SQL, "toIntervalDay(?)") {
		t.Fatalf("the fixture no longer binds anything: %s", plan.Steps[0].SQL)
	}
	if got := plan.Steps[0].Args; !reflect.DeepEqual(got, []any{7}) {
		t.Errorf("step args = %#v, want the bound 7", got)
	}
	if len(d.execArgs) != 1 || !reflect.DeepEqual(d.execArgs[0], []any{7}) {
		t.Errorf("Apply executed with args %#v; a statement run without them reaches ClickHouse with a bare ?", d.execArgs)
	}
}

// --- what the steps tell the operator to do next ---------------------

// An ALTER cannot backfill, and the reseed that can is the repair one:
// a fill is stamped in the seed band, below every version the stream
// has written, so a sink discards it for every key the mirror already
// holds — which is every key the new column is empty for.
func TestAddColumnSendsTheOperatorToARepairReseed(t *testing.T) {
	src := evolveSource()
	live := evolveWithout(evolveLive(t, src), "body")

	step := evolveStepFor(t, evolver(t, clickhouse.New(&evolveDriver{}), src).PlanAgainst(live), "body")
	if !strings.Contains(step.Why, "NewRepairReseeder") {
		t.Errorf("the remedy for an un-backfilled column is a repair reseed: %s", step.Why)
	}
	if strings.Contains(step.Why, "until a fill-mode reseed writes them") {
		t.Errorf("a fill cannot backfill a column: %s", step.Why)
	}
}

// The prescribed order is per step kind, not one order for all four: a
// drop run before the sink is swapped stops the pump dead.
func TestDropColumnSaysToSwapTheSinkFirst(t *testing.T) {
	src := evolveSource()
	live := evolvePlus(evolveLive(t, src), mirror.MirrorColumn{Name: "legacy", Type: "String"})

	plan := evolver(t, clickhouse.New(&evolveDriver{}), src).AllowDrop("legacy").PlanAgainst(live)
	step := evolveStepFor(t, plan, "legacy")
	if !strings.Contains(step.Why, "before running this statement") {
		t.Errorf("a drop has to say the sink goes first: %s", step.Why)
	}
}

// --- LowCardinality --------------------------------------------------

// The wrapper is the operator's tuning decision and the derived
// declaration says nothing about encodings, so a type change carries
// it across rather than rewriting the column back to an unencoded one.
func TestEvolveCarriesLowCardinalityOntoTheNewType(t *testing.T) {
	before := evolveSource()
	after := pg.NewTable("docs")
	pg.Add(after, pg.BigSerial("id").PrimaryKey())
	pg.Add(after, pg.Text("title")) // NOT NULL dropped: String -> Nullable(String)
	pg.Add(after, pg.Integer("views").NotNull())
	pg.Add(after, pg.Text("body"))

	live := evolveRetyped(t, evolveLive(t, before), "title", "LowCardinality(String)")
	plan := evolver(t, clickhouse.New(&evolveDriver{}), after).PlanAgainst(live)

	if len(plan.Refusals) != 0 {
		t.Fatalf("refusals = %+v", plan.Refusals)
	}
	step := evolveStepFor(t, plan, "title")
	if step.To != "LowCardinality(Nullable(String))" {
		t.Fatalf("To = %q: the wrapper must survive the widening", step.To)
	}
	if !strings.Contains(step.SQL, `MODIFY COLUMN IF EXISTS "title" LowCardinality(Nullable(String))`) {
		t.Errorf("SQL = %s", step.SQL)
	}
	if !strings.Contains(step.Why, "LowCardinality") {
		t.Errorf("Why = %s", step.Why)
	}
}

// Where the wrapper cannot be carried across, both answers are
// destructive, so the plan asks instead of picking one: ClickHouse
// refuses LowCardinality over a small fixed-size type unless
// allow_suspicious_low_cardinality_types is set, and unwrapping
// rewrites the whole column.
func TestEvolveRefusesToUnwrapLowCardinalityOnItsOwn(t *testing.T) {
	src := evolveSource()
	live := evolveRetyped(t, evolveLive(t, src), "views", "LowCardinality(Int16)")
	db := clickhouse.New(&evolveDriver{})

	plan := evolver(t, db, src).PlanAgainst(live)
	if len(plan.Steps) != 0 {
		t.Fatalf("the encoding is not drops' to discard: %+v", plan.Steps)
	}
	r := evolveRefusalFor(t, plan, "views")
	if r.Reason != mirror.RefusedNeedsOptIn {
		t.Fatalf("refusal = %+v", r)
	}
	for _, want := range []string{"LowCardinality(Int32)", "allow_suspicious_low_cardinality_types", "AllowTypeChange"} {
		if !strings.Contains(r.Detail, want) {
			t.Errorf("detail missing %q: %s", want, r.Detail)
		}
	}

	// Told to, it accepts the loss and emits the derived type bare.
	allowed := evolver(t, db, src).AllowTypeChange("views").PlanAgainst(live)
	step := evolveStepFor(t, allowed, "views")
	if !strings.Contains(step.SQL, `MODIFY COLUMN IF EXISTS "views" Int32`) {
		t.Errorf("SQL = %s", step.SQL)
	}
}

// The wrapper is not always the only thing at stake. Where the derived
// type is one the live values do not all fit into, the cast is the
// larger risk of the two, and a refusal that names only the encoding
// invites an AllowTypeChange nobody would have granted knowing the
// column may not survive the mutation.
func TestEvolveNamesTheCastRiskTheWrapperHides(t *testing.T) {
	src := evolveSource()

	t.Run("the values do not all fit", func(t *testing.T) {
		// Int32 -> Int16 under a wrapper: the encoding goes, and so
		// may every value above 32767.
		narrowed := pg.NewTable("docs")
		pg.Add(narrowed, pg.BigSerial("id").PrimaryKey())
		pg.Add(narrowed, pg.Text("title").NotNull())
		pg.Add(narrowed, pg.SmallInt("views").NotNull())
		pg.Add(narrowed, pg.Text("body"))

		live := evolveRetyped(t, evolveLive(t, src), "views", "LowCardinality(Int32)")
		r := evolveRefusalFor(t, evolver(t, clickhouse.New(&evolveDriver{}), narrowed).PlanAgainst(live), "views")
		for _, want := range []string{"allow_suspicious_low_cardinality_types", "overflow"} {
			if !strings.Contains(r.Detail, want) {
				t.Errorf("detail missing %q: %s", want, r.Detail)
			}
		}

		// And the operator who grants the opt-in is told the same
		// thing on the step they are about to run.
		step := evolveStepFor(t, evolver(t, clickhouse.New(&evolveDriver{}), narrowed).
			AllowTypeChange("views").PlanAgainst(live), "views")
		if !strings.Contains(step.Why, "overflow") {
			t.Errorf("the step hides the cast risk: %s", step.Why)
		}
	})

	t.Run("only the encoding is at stake", func(t *testing.T) {
		// Int16 -> Int32 is a widening: no value is in danger, so
		// saying they are would be crying wolf.
		live := evolveRetyped(t, evolveLive(t, src), "views", "LowCardinality(Int16)")
		r := evolveRefusalFor(t, evolver(t, clickhouse.New(&evolveDriver{}), src).PlanAgainst(live), "views")
		if strings.Contains(r.Detail, "overflow") {
			t.Errorf("a widening puts no value at risk: %s", r.Detail)
		}
	})
}

// A sorting key of nothing is far more often a caller who built the
// shape by hand and left InSortingKey false than a mirror that really
// sorts by no column, and the two have very different remedies —
// rebuilding a 10M-row table is not the answer to a missing flag.
func TestASortingKeyOfNothingPointsAtTheShapeFirst(t *testing.T) {
	src := evolveSource()
	live := evolveLive(t, src)
	for i := range live {
		live[i].InSortingKey = false // the shape a pre-InSortingKey caller builds
	}

	plan := evolver(t, clickhouse.New(&evolveDriver{}), src).PlanAgainst(live)
	if len(plan.Refusals) != 1 || plan.Refusals[0].Kind != mirror.EvolveSortingKey {
		t.Fatalf("refusals = %+v", plan.Refusals)
	}
	if d := plan.Refusals[0].Detail; !strings.Contains(d, "InSortingKey") {
		t.Errorf("the refusal sends the operator to rebuild a table without saying the shape may be at fault: %s", d)
	}
}
