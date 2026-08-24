package mirror

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/bernardoforcillo/drops"
	"github.com/bernardoforcillo/drops/clickhouse"
	"github.com/bernardoforcillo/drops/pg"
)

// ErrEvolutionRefused is what [Evolver.Apply] returns when the plan
// carries refusals. Test for it with errors.Is to tell "drops declined
// to run this" apart from "ClickHouse rejected a statement".
var ErrEvolutionRefused = errors.New("drops/mirror: schema evolution refused")

// EvolutionKind names the shape of one change, whether it was planned
// or refused.
type EvolutionKind string

const (
	// EvolveCreateTable is the whole mirror table, because ClickHouse
	// does not have it yet.
	EvolveCreateTable EvolutionKind = "create_table"

	// EvolveAddColumn is a column the source has and the mirror does
	// not.
	EvolveAddColumn EvolutionKind = "add_column"

	// EvolveModifyColumn is a column whose ClickHouse type no longer
	// matches the one [MapType] derives from the source.
	EvolveModifyColumn EvolutionKind = "modify_column"

	// EvolveDropColumn is a column the mirror has and the source no
	// longer does.
	EvolveDropColumn EvolutionKind = "drop_column"

	// EvolveSortingKey is the mirror's ORDER BY no longer being the
	// one the source derives. It never appears as an
	// [EvolutionStep]: ClickHouse's MODIFY ORDER BY can only extend a
	// sorting key with columns the same statement adds, so any other
	// change of key is a new table and a copy. It appears as a
	// [Refusal] because the alternative is worse — see
	// [Evolver.PlanAgainst].
	EvolveSortingKey EvolutionKind = "sorting_key"

	// EvolvePartitionKey is the mirror being partitioned when the
	// derived table is not, or the other way round. Like
	// EvolveSortingKey it is only ever a [Refusal]: ClickHouse has no
	// ALTER that changes a table's partitioning.
	EvolvePartitionKey EvolutionKind = "partition_key"
)

// EvolutionStep is one DDL statement, with enough context around it to
// review without re-deriving the reasoning.
type EvolutionStep struct {
	// Kind is what the statement does.
	Kind EvolutionKind

	// Column is the column it acts on; empty for EvolveCreateTable.
	Column string

	// From is the type ClickHouse currently holds. Empty for an add
	// and for the create.
	From string

	// To is the type the statement sets the column to. That is the
	// type derived from the source, except where the live column
	// carries a LowCardinality wrapper worth keeping, in which case it
	// is the derived type inside that wrapper. Empty for a drop and
	// for the create.
	To string

	// SQL is the statement itself.
	SQL string

	// Args are the values SQL's placeholders bind, and are usually
	// empty: DDL is mostly literal text. A [DeriveOption] whose
	// expression carries a value is the exception — a WithPartitionBy
	// of toIntervalDay(7) renders "?" and hands the 7 out here — so
	// the create statement can have them and [Evolver.Apply] passes
	// them through to the driver. A statement executed without its
	// args reaches ClickHouse with a bare "?" in it and does not
	// parse, which is why they travel with the SQL rather than being
	// re-derived.
	Args []any

	// Why is the one-sentence justification, phrased for whoever has
	// to approve the change.
	Why string
}

// RefusalReason says why a change was left out of the plan, and —
// more usefully — whether the caller can do anything about it.
type RefusalReason string

const (
	// RefusedNeedsOptIn means drops can emit the statement but will
	// not do so on its own, because the change loses information or
	// may fail against the stored data. Name the column to
	// [Evolver.AllowDrop] or [Evolver.AllowTypeChange] to clear it.
	RefusedNeedsOptIn RefusalReason = "needs_opt_in"

	// RefusedAmbiguous is RefusedNeedsOptIn with a specific
	// suspicion: a dropped column and an added column of the same
	// type look exactly like a rename, and the two have very
	// different consequences for the data already in the mirror. The
	// same opt-in clears it, once the caller has decided which it is.
	RefusedAmbiguous RefusalReason = "ambiguous"

	// RefusedNotInPlace means no opt-in exists, because ClickHouse
	// itself will not perform the change on a populated table. The
	// remedy is always the same and always manual: create a new table
	// with the shape you want and copy into it.
	RefusedNotInPlace RefusalReason = "not_in_place"
)

// Refusal is a change the plan describes but will not run.
type Refusal struct {
	// Kind is the statement drops declined to emit.
	Kind EvolutionKind

	// Column is the column the change would have acted on.
	Column string

	// From is the type ClickHouse currently holds, where the change
	// concerns a type.
	From string

	// To is the type derived from the source, where the change
	// concerns a type.
	To string

	// Reason says whether an opt-in can clear this.
	Reason RefusalReason

	// Detail is the full explanation, including the remedy.
	Detail string
}

// String renders the refusal as one line of review output. A refusal
// about the table itself — [EvolveSortingKey], [EvolvePartitionKey] —
// names no column, and says so by leaving it out rather than by
// printing an empty one.
func (r Refusal) String() string {
	if r.Column == "" {
		return fmt.Sprintf("%s (%s): %s", r.Kind, r.Reason, r.Detail)
	}
	return fmt.Sprintf("%s %q (%s): %s", r.Kind, r.Column, r.Reason, r.Detail)
}

// EvolutionPlan is the difference between the mirror ClickHouse holds
// and the one the source table derives, split into what drops will run
// and what it refuses to.
type EvolutionPlan struct {
	// Table is the qualified mirror table the plan applies to.
	Table string

	// Steps are the statements, in the order they must run. Later
	// steps may depend on earlier ones — an ADD COLUMN ... AFTER
	// names the column added just before it.
	Steps []EvolutionStep

	// Refusals are the changes drops saw and would not make. A plan
	// with refusals is not a plan that brings the mirror into line.
	Refusals []Refusal
}

// Statements returns the plan's SQL, in order, for a caller that wants
// to review or store the DDL rather than let [Evolver.Apply] run it.
//
// A statement is only complete on its own when its step carries no
// [EvolutionStep.Args]. Read the steps rather than the statements if
// you intend to run them somewhere else: a plan built from a
// [DeriveOption] that binds a value renders that value as "?", and the
// text alone will not execute.
func (p EvolutionPlan) Statements() []string {
	out := make([]string, 0, len(p.Steps))
	for _, s := range p.Steps {
		out = append(out, s.SQL)
	}
	return out
}

// Aligned reports whether the mirror already matches the derived
// shape — nothing to run and nothing refused. A plan with no steps but
// with refusals is not aligned: it is stuck.
func (p EvolutionPlan) Aligned() bool {
	return len(p.Steps) == 0 && len(p.Refusals) == 0
}

// Refused collects the plan's refusals into one error wrapping
// [ErrEvolutionRefused], or returns nil when there are none.
func (p EvolutionPlan) Refused() error {
	if len(p.Refusals) == 0 {
		return nil
	}
	parts := make([]string, 0, len(p.Refusals))
	for _, r := range p.Refusals {
		parts = append(parts, r.String())
	}
	return fmt.Errorf("%w on %s: %s", ErrEvolutionRefused, p.Table, strings.Join(parts, "; "))
}

// MirrorColumn is one column of the mirror table as ClickHouse
// currently holds it.
type MirrorColumn struct {
	// Name is the column's identifier.
	Name string

	// Type is ClickHouse's own spelling of the type, which is not
	// always the spelling that was written: the server echoes
	// "Decimal(10, 2)" for a column declared "Decimal(10,2)".
	// Comparison normalises the difference away.
	Type string

	// InKey reports whether the column takes part in the sorting,
	// primary or partition key. ClickHouse refuses both MODIFY COLUMN
	// and DROP COLUMN on such a column once the table has data,
	// because those keys are the on-disk layout, so a plan can only
	// report the change rather than emit it.
	InKey bool

	// InSortingKey narrows InKey to the sorting key alone, which is
	// the one the mirror's behaviour depends on: ReplacingMergeTree
	// collapses rows that share it, and [ClickHouseSink] addresses a
	// tombstone by it. [Evolver.PlanAgainst] compares the columns
	// flagged here against the sorting key the source derives, so a
	// hand-built shape has to set it — a shape that flags nothing
	// reads as a mirror with no sorting key at all, and is refused
	// as such.
	InSortingKey bool

	// InPartitionKey narrows InKey to the partition key. It matters
	// because ReplacingMergeTree only collapses rows within one
	// partition: a mirror partitioned by something that is not a
	// function of its sorting key keeps every version of a row whose
	// partition changed, and FINAL returns them all.
	InPartitionKey bool
}

// InspectMirror reads the mirror table's current shape out of
// ClickHouse's system.columns.
//
// The live shape is taken from the server rather than from a second
// derived table on purpose. The failure this package exists to prevent
// — a sink writing a column the mirror does not have — is a fact about
// ClickHouse, not about the Go declarations. Diffing two derived
// tables answers "what changed in the source declaration", which
// misses every way the mirror drifts on its own: an ALTER someone ran
// by hand, an evolution that failed part-way through, a table restored
// from a backup taken at an older schema, a mirror that was never
// created at all. Those are exactly the situations in which the answer
// matters.
//
// An empty result means ClickHouse has no such table; a plan built
// against it is a CREATE TABLE.
//
// system.columns describes columns, so that is the whole of what a
// plan can see. Which columns take part in the sorting and partition
// keys is there — [MirrorColumn.InSortingKey] and
// [MirrorColumn.InPartitionKey] — but their order within a composite
// key is not, and the table's engine, SETTINGS, TTL, column codecs,
// defaults and comments are not there at all. See the Evolver's
// "What a plan cannot see" for what that means for the diff.
func InspectMirror(ctx context.Context, db *clickhouse.DB, t *clickhouse.Table) ([]MirrorColumn, error) {
	if db == nil || t == nil {
		return nil, fmt.Errorf("drops/mirror: InspectMirror needs a ClickHouse db and a table")
	}
	const projection = "SELECT name, type, is_in_sorting_key, is_in_primary_key, is_in_partition_key FROM system.columns WHERE "
	const order = " ORDER BY position"

	sql := projection + "database = currentDatabase() AND table = ?" + order
	args := []any{t.Name()}
	if t.Database() != "" {
		sql = projection + "database = ? AND table = ?" + order
		args = []any{t.Database(), t.Name()}
	}

	rows, err := db.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("drops/mirror: reading system.columns for %s: %w", evolveQualified(t), err)
	}
	defer rows.Close()

	var out []MirrorColumn
	for rows.Next() {
		var (
			name, typ              string
			sorting, primary, part uint8
		)
		if err := rows.Scan(&name, &typ, &sorting, &primary, &part); err != nil {
			return nil, fmt.Errorf("drops/mirror: scanning system.columns for %s: %w", evolveQualified(t), err)
		}
		out = append(out, MirrorColumn{
			Name: name,
			Type: typ,
			// The union is what an ALTER of the column runs into;
			// the sorting and partition flags are kept apart from it
			// because each of the two answers a question of its own
			// about the table.
			InKey:          sorting|primary|part != 0,
			InSortingKey:   sorting != 0,
			InPartitionKey: part != 0,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("drops/mirror: reading system.columns for %s: %w", evolveQualified(t), err)
	}
	return out, nil
}

// Evolver brings a ClickHouse mirror back into line with its source
// table after the source's schema changes.
//
// [DeriveClickHouse] builds the mirror once. When Postgres later gains
// a column, nothing issues the matching ALTER, and the next batch the
// sink writes names a column ClickHouse does not have. Evolver is the
// missing half: it derives the table the source implies now, reads
// what ClickHouse actually holds (see [InspectMirror]), and produces
// the DDL between the two.
//
// What it will do unasked is exactly the set of changes that cannot
// lose anything: create the table if it is absent, add a column the
// source gained, and widen a column's type where every stored value is
// still a value of the new type. Everything else — a drop, a narrowing
// or otherwise unprovable cast — is reported as a [Refusal] and left
// for a human, because a mirror that silently discards a column is
// worse than one that stops and says so. Name the column to
// [Evolver.AllowDrop] or [Evolver.AllowTypeChange] to accept it.
//
// A LowCardinality wrapper someone added by hand is deliberately not
// treated as drift. It is a storage encoding, not a change of value
// domain, so a column whose derived type is otherwise unchanged is
// left alone; and where the derived type does change, the wrapper is
// carried over onto the new type rather than dropped — for the types
// ClickHouse will accept it on, which of the types [MapType] derives
// means String. Anywhere else the plan refuses and says why, because
// both answers are destructive: re-wrapping hits
// allow_suspicious_low_cardinality_types, and unwrapping rewrites a
// column the operator tuned deliberately.
//
// # Order of operations
//
// [NewClickHouseSink] snapshots the table's column list, so the sink
// and the mirror have to be brought into line in an order that never
// leaves a running sink naming a column ClickHouse does not have.
// That order is not the same for every kind of step, because an add
// and a drop fail in opposite directions:
//
//   - [EvolveCreateTable], [EvolveAddColumn] and [EvolveModifyColumn]:
//     run the DDL first, then build a new sink from [Evolver.Target],
//     then swap it into the pump, and only then start emitting the new
//     column. The running sink does not name the added column, so it
//     keeps working throughout; a sink built before the ALTER lands
//     would write a column ClickHouse lacks.
//   - [EvolveDropColumn]: the reverse. Swap the sink first, then run
//     the DDL. A ClickHouseSink binds a value for every column it
//     snapshotted, so from the moment the column is gone every batch
//     it writes is rejected — and [Pump.Step] does not acknowledge a
//     batch a sink refused, so [Pump.Run] retries that same batch with
//     backoff until someone swaps the sink, or gives up on it once its
//     attempt budget runs out. A sink built from Target() before the
//     DDL simply stops writing the column, and ClickHouse fills it
//     with its default for the few batches in between.
//
// A plan that both adds and drops has no single safe order, so it
// wants two applies with the swap between them. The default opt-in
// already shapes that: a drop is refused unless [Evolver.AllowDrop]
// names it, so run [Evolver.AllowPartial] first to apply the adds,
// swap the sink, then apply again with AllowDrop for the drops.
//
// Evolver never touches the table a live sink holds — it derives a
// fresh one.
//
// # What an ALTER cannot do
//
// ADD COLUMN backfills nothing. Rows already in the mirror read the
// new column's type default — 0, the empty string, NULL when the
// column is Nullable — and no ALTER changes that.
//
// Filling those rows in is a reseed, and it has to be a repair-mode
// one ([NewRepairReseeder]). A repair re-emits every row through the
// outbox, so each arrives as an ordinary change carrying a version in
// the live band, which outranks whatever the mirror already holds for
// that key. A fill-mode reseed is not a cheaper version of the same
// thing and will not do it: a fill stamps the seed band, below every
// version the change stream has ever written, so a sink discards each
// seeded row for a key it already has and the new column keeps its
// default forever. A fill populates keys the mirror is missing
// altogether; only a repair overwrites a row that is already there.
// Swap the sink in before the repair runs, or the pump will deliver
// the reseeded rows through a sink that does not yet write the new
// column.
//
// # What a plan cannot see
//
// The live shape comes from system.columns (see [InspectMirror]), so
// the diff covers columns, their types, and which of them are in the
// sorting and partition keys. Those two are compared as sets of
// columns: the position of a column within a composite sorting key is
// not reported, and neither is the partitioning expression — only
// which columns it names, which is why [WithPartitionBy] is compared
// on presence alone.
//
// The table's engine, its SETTINGS, its TTL, and each column's codec,
// DEFAULT and COMMENT are not in system.columns at all, so [WithEngine]
// and [WithSetting] can never show up here as drift. A mirror whose
// engine was swapped for a plain MergeTree stops converging — nothing
// deduplicates, so a replayed batch duplicates rows — and this plan
// will still call it aligned. SHOW CREATE TABLE is the tool for that,
// and it is worth reading whenever the mirror has been touched by
// hand.
//
// An Evolver is configured before use and then read-only; drive it
// from one goroutine.
type Evolver struct {
	db     *clickhouse.DB
	target *clickhouse.Table
	name   string

	// partitioned records whether the options declared a PARTITION
	// BY. The derived table renders its partitioning but does not
	// hand it back, and the live side reports which columns take part
	// rather than the expression, so what the two can be compared on
	// is whether each is partitioned at all — and this is where the
	// declared half of that comes from.
	partitioned bool

	allowDrop map[string]bool
	allowType map[string]bool
	partial   bool
}

// NewEvolver derives the mirror src implies and prepares to compare it
// against the one mir holds. Pass the same [DeriveOption] values the
// mirror was originally created with — the name and database are how
// the live table is found, and a mismatch reads as "the table does not
// exist".
func NewEvolver(mir *clickhouse.DB, src *pg.Table, opts ...DeriveOption) (*Evolver, error) {
	if mir == nil {
		return nil, fmt.Errorf("drops/mirror: NewEvolver needs a ClickHouse db")
	}
	target, err := DeriveClickHouse(src, opts...)
	if err != nil {
		return nil, err
	}
	var cfg deriveConfig
	for _, o := range opts {
		o(&cfg)
	}
	return &Evolver{
		db:          mir,
		target:      target,
		name:        evolveQualified(target),
		partitioned: cfg.partition != nil,
		allowDrop:   map[string]bool{},
		allowType:   map[string]bool{},
	}, nil
}

// Target returns the table the source derives — the shape the mirror
// is being brought to. Build the replacement [ClickHouseSink] from it
// once [Evolver.Apply] has succeeded.
func (e *Evolver) Target() *clickhouse.Table { return e.target }

// AllowDrop accepts losing the named columns. Only columns named here
// are dropped; the opt-in is per column rather than a single flag so
// that drift appearing between the review and the apply is still
// refused instead of being swept along by a blanket permission.
//
// It does not clear a [RefusedNotInPlace] refusal: ClickHouse will not
// drop a column that is part of the sorting, primary or partition key,
// whatever the caller permits.
func (e *Evolver) AllowDrop(columns ...string) *Evolver {
	for _, c := range columns {
		e.allowDrop[c] = true
	}
	return e
}

// AllowTypeChange accepts the CAST for the named columns — that
// ClickHouse may round, overflow or throw part-way through rewriting
// the column, and that the values it writes are whatever the cast
// produces. Widening changes never need this; they are emitted anyway.
//
// Like [Evolver.AllowDrop] it cannot clear a [RefusedNotInPlace]
// refusal.
func (e *Evolver) AllowTypeChange(columns ...string) *Evolver {
	for _, c := range columns {
		e.allowType[c] = true
	}
	return e
}

// AllowPartial lets [Evolver.Apply] run the safe steps even though the
// plan also carries refusals.
//
// The default is all-or-nothing, because a plan drops does not
// understand in full is a plan whose result nobody has described. The
// exception this exists for is real though: an unblockable refusal —
// a type change ClickHouse cannot make in place — would otherwise also
// block the ADD COLUMN that stops the sink from failing on every
// batch, and the two problems are unrelated. Apply still returns
// [ErrEvolutionRefused] afterwards, so a caller cannot mistake a
// partial apply for a finished one.
func (e *Evolver) AllowPartial() *Evolver {
	e.partial = true
	return e
}

// Plan reads the live table and returns the difference. It executes no
// DDL, which is the default on purpose: schema changes to an analytics
// store are worth a human reading first.
func (e *Evolver) Plan(ctx context.Context) (EvolutionPlan, error) {
	live, err := InspectMirror(ctx, e.db, e.target)
	if err != nil {
		return EvolutionPlan{Table: e.name}, err
	}
	return e.PlanAgainst(live), nil
}

// PlanAgainst is [Evolver.Plan] against a shape the caller already
// has — from their own migration tooling, or from a snapshot taken
// earlier. It touches no database.
//
// The diff is not only the columns. A mirror whose sorting key is no
// longer the derived one is a mirror that has stopped behaving like a
// mirror — ReplacingMergeTree collapses rows that share the sorting
// key, and [ClickHouseSink] writes a tombstone addressed by the
// derived key, so a live key of "slug" and a derived key of "id" sends
// every delete to a row that does not exist and leaves the deleted one
// readable for good. Every column type can be byte-identical while
// that is true, which is why the key is compared separately, and why
// the answer is a [Refusal] and not a step: ClickHouse's MODIFY ORDER
// BY only extends a key with columns the same statement adds, so this
// is a new table and a copy, followed by a sink built from
// [Evolver.Target].
//
// A shape built by hand rather than read by [InspectMirror] therefore
// has to flag its sorting key: a shape where no column sets
// [MirrorColumn.InSortingKey] describes a mirror that sorts by
// nothing, and is refused as one.
func (e *Evolver) PlanAgainst(live []MirrorColumn) EvolutionPlan {
	plan := EvolutionPlan{Table: e.name}
	if len(live) == 0 {
		sql, args := drops.StringWithDialect(clickhouse.Dialect, clickhouse.CreateTableIfNotExists(e.target))
		plan.Steps = append(plan.Steps, EvolutionStep{
			Kind: EvolveCreateTable,
			SQL:  sql,
			Args: args,
			Why:  "ClickHouse reports no columns for this table, so the mirror has never been created",
		})
		return plan
	}

	e.planShape(&plan, live)

	byName := make(map[string]MirrorColumn, len(live))
	for _, c := range live {
		byName[c.Name] = c
	}

	desired := e.target.Columns()
	for i, col := range desired {
		want := col.Type().TypeSQL()
		cur, ok := byName[col.Name()]
		if !ok {
			plan.Steps = append(plan.Steps, e.addStep(desired, i))
			continue
		}
		e.planType(&plan, col.Name(), cur, want)
	}

	// Only the columns ClickHouse has that the derived table does
	// not. [VersionColumn] and [DeletedColumn] are never among them:
	// they belong to the mirror rather than to the source, and
	// DeriveClickHouse puts them on every table it derives, so a
	// source with no such column is no evidence at all for removing
	// them.
	for _, cur := range live {
		if e.target.Col(cur.Name) != nil {
			continue
		}
		e.planDrop(&plan, cur)
	}
	return plan
}

// planShape records the differences that are about the table rather
// than about one of its columns. Neither is expressible as an ALTER,
// so both are refusals, and both are RefusedNotInPlace: no opt-in
// exists for a change ClickHouse will not make.
func (e *Evolver) planShape(p *EvolutionPlan, live []MirrorColumn) {
	want := e.target.OrderByColumns()
	var got []string
	partitioned := false
	for _, c := range live {
		if c.InSortingKey {
			got = append(got, c.Name)
		}
		if c.InPartitionKey {
			partitioned = true
		}
	}
	if !evolveSameColumns(got, want) {
		detail := fmt.Sprintf(
			"the mirror sorts by %s and the source derives %s. The sorting key is what ReplacingMergeTree "+
				"deduplicates on and what a tombstone is addressed by, so a sink built from the derived table "+
				"would write every delete under a key this table does not sort by, and the deleted rows would "+
				"stay readable through a FINAL scan for good. ClickHouse's MODIFY ORDER BY can only extend a "+
				"sorting key with columns the same statement adds, so there is no ALTER for this: create a "+
				"table with the derived key, copy the data across, and swap the sink onto it",
			evolveColumnList(got), evolveColumnList(want))
		// A key of nothing is not a shape ClickHouse can report for a
		// MergeTree table, so the likely cause is the shape and not
		// the mirror. Say so before sending anyone off to rebuild and
		// copy a table that may be perfectly well keyed.
		if len(got) == 0 {
			detail += ". A live shape read by InspectMirror never looks like this — a MergeTree table always " +
				"sorts by something — so check the shape first if it was built by hand: a MirrorColumn that " +
				"leaves InSortingKey false is a column in no sorting key, and a shape where no column sets it " +
				"is a mirror that sorts by nothing"
		}
		p.Refusals = append(p.Refusals, Refusal{
			Kind: EvolveSortingKey, Reason: RefusedNotInPlace, Detail: detail,
		})
	}
	if partitioned != e.partitioned {
		detail := "the mirror is partitioned and the derived table declares no PARTITION BY, so the two disagree " +
			"about the on-disk layout. It matters for more than tidiness: ReplacingMergeTree collapses rows only " +
			"within one partition, so a partitioning that is not a function of the sorting key keeps every version " +
			"of a row whose partition value changed and FINAL returns them all"
		if e.partitioned {
			detail = "the derived table declares a PARTITION BY and the mirror has no column in a partition key, " +
				"so the mirror was created without it — a partitioning added to the Go declaration after the fact"
		}
		p.Refusals = append(p.Refusals, Refusal{
			Kind: EvolvePartitionKey, Reason: RefusedNotInPlace,
			Detail: detail + ". ClickHouse has no ALTER that changes a table's partitioning: create a table with " +
				"the layout you want and copy across. Note that drops compares only whether each side is " +
				"partitioned — system.columns reports which columns a partition expression names, not the " +
				"expression, so two different expressions over the same column look alike here",
		})
	}
}

// evolveSameColumns compares two column name sets. Sets, not
// sequences: system.columns says whether a column is in the sorting
// key and not where in it, so a composite key that has been reordered
// is invisible to this comparison. It is stated in [InspectMirror]
// rather than papered over.
func evolveSameColumns(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	seen := make(map[string]int, len(a))
	for _, n := range a {
		seen[n]++
	}
	for _, n := range b {
		seen[n]--
		if seen[n] < 0 {
			return false
		}
	}
	return true
}

// evolveColumnList renders a key for a human reading a refusal.
func evolveColumnList(names []string) string {
	if len(names) == 0 {
		return "no column at all"
	}
	quoted := make([]string, 0, len(names))
	for _, n := range names {
		quoted = append(quoted, strconv.Quote(n))
	}
	return "(" + strings.Join(quoted, ", ") + ")"
}

// planType records what a column's type difference means.
//
// The type the statement sets is the classifier's, not the derived
// one: a live LowCardinality wrapper that can be kept is kept, and
// "to" is then the derived type inside it.
func (e *Evolver) planType(p *EvolutionPlan, name string, cur MirrorColumn, want string) {
	verdict, to, why := evolveClassifyType(want, cur.Type, cur.InKey)
	switch verdict {
	case evolveTypeMatches:
	case evolveTypeInPlace:
		p.Steps = append(p.Steps, e.modifyStep(name, cur.Type, to, why))
	case evolveTypeLossy:
		if e.allowType[name] {
			p.Steps = append(p.Steps, e.modifyStep(name, cur.Type, to,
				"AllowTypeChange names this column, accepting that "+why))
			return
		}
		p.Refusals = append(p.Refusals, Refusal{
			Kind: EvolveModifyColumn, Column: name, From: cur.Type, To: want,
			Reason: RefusedNeedsOptIn,
			Detail: why + "; pass " + strconv.Quote(name) + " to AllowTypeChange to accept it",
		})
	case evolveTypeImpossible:
		p.Refusals = append(p.Refusals, Refusal{
			Kind: EvolveModifyColumn, Column: name, From: cur.Type, To: want,
			Reason: RefusedNotInPlace, Detail: why,
		})
	}
}

// planDrop records what a column ClickHouse has and the source no
// longer does means.
func (e *Evolver) planDrop(p *EvolutionPlan, cur MirrorColumn) {
	if cur.InKey {
		p.Refusals = append(p.Refusals, Refusal{
			Kind: EvolveDropColumn, Column: cur.Name, From: cur.Type,
			Reason: RefusedNotInPlace,
			Detail: "ClickHouse will not drop a column that is part of the sorting, primary or partition key, " +
				"and the mirror's sorting key is what ReplacingMergeTree deduplicates on; " +
				"a mirror that no longer has this column is a different table, so create it and copy across",
		})
		return
	}
	if e.allowDrop[cur.Name] {
		p.Steps = append(p.Steps, e.dropStep(cur))
		return
	}
	reason := RefusedNeedsOptIn
	detail := "the source no longer has this column, and dropping it discards the data ClickHouse still holds; " +
		"pass " + strconv.Quote(cur.Name) + " to AllowDrop to accept that"
	if twin := evolveRenameTwin(p.Steps, cur.Type); twin != "" {
		reason = RefusedAmbiguous
		detail = fmt.Sprintf(
			"%s is gone from the source and %s was added with the same type (%s), which is what a rename looks like — "+
				"and a rename keeps the data ClickHouse already holds where a drop-plus-add throws it away. "+
				"drops will not guess: run ALTER TABLE %s RENAME COLUMN %s TO %s yourself if it was a rename, "+
				"or pass %s to AllowDrop if the column really is going",
			strconv.Quote(cur.Name), strconv.Quote(twin), cur.Type,
			e.name,
			clickhouse.Dialect.QuoteIdent(cur.Name), clickhouse.Dialect.QuoteIdent(twin),
			strconv.Quote(cur.Name))
	}
	p.Refusals = append(p.Refusals, Refusal{
		Kind: EvolveDropColumn, Column: cur.Name, From: cur.Type,
		Reason: reason, Detail: detail,
	})
}

// Apply plans and then runs the plan's statements.
//
// With refusals outstanding it runs nothing and returns
// [ErrEvolutionRefused] — unless [Evolver.AllowPartial] was set, in
// which case the steps run first and the same error is returned after
// them. Either way a nil error means, and only means, that the mirror
// now matches the derived shape.
//
// ClickHouse has no transactional DDL, so a failure part-way leaves
// the earlier statements applied. Every statement carries IF [NOT]
// EXISTS for that reason — the MODIFY included, which is the one that
// would otherwise abort the rest of the plan when it loses the race:
// re-running Apply after a failure is safe, and so is racing another
// operator who applied the same change first.
//
// The order the statements run in is the plan's, and it is the right
// one within a single Apply. It is not the whole order of the
// operation: a live sink has to be swapped before or after the DDL
// depending on what the plan does — see the Evolver's "Order of
// operations".
func (e *Evolver) Apply(ctx context.Context) (EvolutionPlan, error) {
	plan, err := e.Plan(ctx)
	if err != nil {
		return plan, err
	}
	if len(plan.Refusals) > 0 && !e.partial {
		return plan, plan.Refused()
	}
	for _, s := range plan.Steps {
		if _, err := e.db.Exec(ctx, s.SQL, s.Args...); err != nil {
			return plan, fmt.Errorf("drops/mirror: %s: %w", s.SQL, err)
		}
	}
	return plan, plan.Refused()
}

// addStep renders the ADD COLUMN for desired[i].
//
// The position clause is not cosmetic housekeeping alone: without it
// ClickHouse appends, so a column added to the middle of the source
// table lands after the bookkeeping columns and every later inspection
// reports a layout nobody chose. Naming the preceding derived column
// is always valid — it is either already in the mirror or added by an
// earlier step of this same plan.
func (e *Evolver) addStep(desired []*clickhouse.Column, i int) EvolutionStep {
	col := desired[i]
	want := col.Type().TypeSQL()
	sql := "ALTER TABLE " + e.name + " ADD COLUMN IF NOT EXISTS " +
		clickhouse.Dialect.QuoteIdent(col.Name()) + " " + want
	if i == 0 {
		sql += " FIRST"
	} else {
		sql += " AFTER " + clickhouse.Dialect.QuoteIdent(desired[i-1].Name())
	}
	return EvolutionStep{
		Kind: EvolveAddColumn, Column: col.Name(), To: want, SQL: sql,
		Why: "the source has this column and the mirror does not, so the sink is about to write a column " +
			"ClickHouse would reject; an ALTER cannot backfill, so rows already mirrored read the type's " +
			"default until a repair-mode reseed (NewRepairReseeder) re-emits them through the outbox — a " +
			"fill-mode reseed stamps the seed band and loses to every row the stream has already written, " +
			"so it would leave the column at its default",
	}
}

// modifyStep renders the MODIFY COLUMN.
//
// IF EXISTS for the same reason the add and the drop carry theirs, and
// with more at stake: a plan runs its statements in order and stops at
// the first failure, so a MODIFY that lost a race against another
// operator's DROP would take the ADD COLUMN queued behind it down too
// — and that ADD is what stops the live sink rejecting every batch.
// ClickHouse skips a MODIFY COLUMN IF EXISTS naming a column that is
// gone, which turns that abort into a no-op.
func (e *Evolver) modifyStep(name, from, to, why string) EvolutionStep {
	return EvolutionStep{
		Kind: EvolveModifyColumn, Column: name, From: from, To: to,
		SQL: "ALTER TABLE " + e.name + " MODIFY COLUMN IF EXISTS " +
			clickhouse.Dialect.QuoteIdent(name) + " " + to,
		Why: why,
	}
}

// dropStep renders the DROP COLUMN.
func (e *Evolver) dropStep(cur MirrorColumn) EvolutionStep {
	return EvolutionStep{
		Kind: EvolveDropColumn, Column: cur.Name, From: cur.Type,
		SQL: "ALTER TABLE " + e.name + " DROP COLUMN IF EXISTS " +
			clickhouse.Dialect.QuoteIdent(cur.Name),
		Why: "the source no longer has this column and AllowDrop names it; swap the pump's sink for one built " +
			"from Target() before running this statement, not after — a running sink binds a value for every " +
			"column it snapshotted, so from the moment the column is gone ClickHouse rejects every batch it " +
			"writes and the pump retries them until the swap",
	}
}

// evolveRenameTwin returns the name of a column this plan is adding
// whose type matches a column being dropped, or "" when there is none.
// It reads the steps built so far rather than a map so the answer does
// not depend on iteration order.
func evolveRenameTwin(steps []EvolutionStep, dropped string) string {
	want := clickhouse.NormalizeType(dropped)
	for _, s := range steps {
		if s.Kind == EvolveAddColumn && clickhouse.NormalizeType(s.To) == want {
			return s.Column
		}
	}
	return ""
}

// evolveQualified renders the table's name the way a statement must
// spell it.
func evolveQualified(t *clickhouse.Table) string {
	sql, _ := drops.StringWithDialect(clickhouse.Dialect, t)
	return sql
}

// evolveVerdict is what a type difference means for ALTER.
type evolveVerdict int

const (
	// evolveTypeMatches: nothing to do.
	evolveTypeMatches evolveVerdict = iota

	// evolveTypeInPlace: the cast cannot lose or reject a value.
	evolveTypeInPlace

	// evolveTypeLossy: ClickHouse will run it, and the result depends
	// on data drops cannot see.
	evolveTypeLossy

	// evolveTypeImpossible: ClickHouse refuses it outright.
	evolveTypeImpossible
)

// evolveClassifyType decides what to do about a column whose live type
// is not the one the source derives. It returns the verdict, the type
// a MODIFY COLUMN should actually set — which is the derived type
// except where a live LowCardinality wrapper is carried onto it — and
// the sentence that explains the decision.
//
// The analysis itself belongs to the dialect and lives there, in
// [clickhouse.ClassifyTypeChange]: whether Int16 widens to Int32,
// whether a LowCardinality wrapper can travel onto the new type, and
// what ClickHouse will refuse outright are facts about ClickHouse
// rather than about mirroring, and two copies of them would drift.
//
// What stays here is the part that is about this mirror, and it is not
// only wording. A column losing its Nullable is merely unprovable for
// a table in general — the mutation fails on the first NULL and there
// may be none — and it is certain to fail here, because
// [ClickHouseSink] writes NULL into every non-key column of every
// tombstone. So the verdict is upgraded, and the explanations name the
// mirror's own reasons: the sorting key doubling as what
// ReplacingMergeTree deduplicates on, the source deriving the type
// rather than an operator declaring it.
func evolveClassifyType(want, live string, inKey bool) (verdict evolveVerdict, to, why string) {
	change := clickhouse.ClassifyTypeChange(want, live, inKey)
	switch change.Cause {
	case clickhouse.CauseKeyColumn:
		return evolveTypeImpossible, "", fmt.Sprintf(
			"%s is part of the sorting, primary or partition key, and ClickHouse will not ALTER such a column: "+
				"the key is the on-disk layout, and for this mirror it is also what ReplacingMergeTree deduplicates on. "+
				"Changing it to %s means creating a new table and copying into it", live, want)

	case clickhouse.CauseNullRemoval:
		// Unprovable for a table in general, certain here.
		return evolveTypeImpossible, "", fmt.Sprintf(
			"going from %s to %s means casting every stored NULL, and this mirror writes NULL into every "+
				"non-key column of every tombstone, so the mutation fails as soon as one delete has been mirrored. "+
				"Keep the column Nullable, or rebuild the table", live, want)

	case clickhouse.CauseLowCardinalityWrapper:
		why := fmt.Sprintf(
			"the mirror holds %s where the source derives %s, and the wrapper cannot come along: ClickHouse "+
				"refuses LowCardinality(%s) unless allow_suspicious_low_cardinality_types is set, so the "+
				"statement would rewrite the whole column back to an unencoded %s and lose an encoding somebody "+
				"chose deliberately; keeping it means a MODIFY COLUMN run by hand",
			live, want, want, want)
		// The encoding is the smaller loss when the values are at
		// risk too. Saying only "you lose LowCardinality" reads as a
		// storage-size decision, and the caller who shrugs at that
		// and passes the column to AllowTypeChange has not been told
		// the mutation may not finish.
		if change.ValuesAtRisk {
			why += fmt.Sprintf(
				". The values are the larger risk: ClickHouse would cast every one of them from %s to %s, and "+
					"drops cannot prove that conversion keeps them — it can round, overflow, or throw part-way "+
					"through the mutation", live, want)
		}
		return evolveTypeLossy, change.To, why
	}

	switch change.Verdict {
	case clickhouse.TypeWidens:
		return evolveTypeInPlace, change.To, change.Why
	case clickhouse.TypeUnprovable:
		return evolveTypeLossy, change.To, change.Why
	}
	return evolveTypeMatches, "", ""
}
