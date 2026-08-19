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

	// To is the type derived from the source. Empty for a drop and
	// for the create.
	To string

	// SQL is the statement itself.
	SQL string

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

// String renders the refusal as one line of review output.
func (r Refusal) String() string {
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
			Name:  name,
			Type:  typ,
			InKey: sorting|primary|part != 0,
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
// Two things Evolver deliberately does not treat as drift. A
// LowCardinality wrapper someone added by hand is a storage encoding,
// not a change of value domain, so it is left alone rather than undone.
// Codecs, defaults and comments do not appear in system.columns' type
// at all, so they are invisible here by construction.
//
// # Order of operations
//
// [NewClickHouseSink] snapshots the table's column list, and the
// derived table is not safe to mutate underneath a live sink. So:
// apply the DDL, then build a new sink from [Evolver.Target], then
// swap that sink into the pump, and only then start emitting the new
// column. Any other order writes a column ClickHouse lacks, or leaves
// a running sink bound to a stale column list. Evolver never touches
// the table a live sink holds — it derives a fresh one.
//
// # What an ALTER cannot do
//
// ADD COLUMN backfills nothing. Rows already in the mirror read the
// new column's type default — 0, the empty string, NULL when the
// column is Nullable — and no ALTER changes that. Only a fill-mode
// reseed writes real values into the rows the stream never revisits.
//
// An Evolver is configured before use and then read-only; drive it
// from one goroutine.
type Evolver struct {
	db     *clickhouse.DB
	target *clickhouse.Table
	name   string

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
	return &Evolver{
		db:        mir,
		target:    target,
		name:      evolveQualified(target),
		allowDrop: map[string]bool{},
		allowType: map[string]bool{},
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
func (e *Evolver) PlanAgainst(live []MirrorColumn) EvolutionPlan {
	plan := EvolutionPlan{Table: e.name}
	if len(live) == 0 {
		sql, _ := drops.StringWithDialect(clickhouse.Dialect, clickhouse.CreateTableIfNotExists(e.target))
		plan.Steps = append(plan.Steps, EvolutionStep{
			Kind: EvolveCreateTable,
			SQL:  sql,
			Why:  "ClickHouse reports no columns for this table, so the mirror has never been created",
		})
		return plan
	}

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

// planType records what a column's type difference means.
func (e *Evolver) planType(p *EvolutionPlan, name string, cur MirrorColumn, want string) {
	verdict, why := evolveClassifyType(want, cur.Type, cur.InKey)
	switch verdict {
	case evolveTypeMatches:
	case evolveTypeInPlace:
		p.Steps = append(p.Steps, e.modifyStep(name, cur.Type, want, why))
	case evolveTypeLossy:
		if e.allowType[name] {
			p.Steps = append(p.Steps, e.modifyStep(name, cur.Type, want,
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
// EXISTS for that reason: re-running Apply after a failure is safe,
// and so is racing another operator who applied the same change first.
func (e *Evolver) Apply(ctx context.Context) (EvolutionPlan, error) {
	plan, err := e.Plan(ctx)
	if err != nil {
		return plan, err
	}
	if len(plan.Refusals) > 0 && !e.partial {
		return plan, plan.Refused()
	}
	for _, s := range plan.Steps {
		if _, err := e.db.Exec(ctx, s.SQL); err != nil {
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
			"ClickHouse would reject; rows already mirrored read the type's default until a fill-mode reseed " +
			"writes them, because an ALTER cannot backfill",
	}
}

// modifyStep renders the MODIFY COLUMN.
func (e *Evolver) modifyStep(name, from, to, why string) EvolutionStep {
	return EvolutionStep{
		Kind: EvolveModifyColumn, Column: name, From: from, To: to,
		SQL: "ALTER TABLE " + e.name + " MODIFY COLUMN " +
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
		Why: "the source no longer has this column and AllowDrop names it",
	}
}

// evolveRenameTwin returns the name of a column this plan is adding
// whose type matches a column being dropped, or "" when there is none.
// It reads the steps built so far rather than a map so the answer does
// not depend on iteration order.
func evolveRenameTwin(steps []EvolutionStep, dropped string) string {
	want := evolveNormalizeType(dropped)
	for _, s := range steps {
		if s.Kind == EvolveAddColumn && evolveNormalizeType(s.To) == want {
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
// is not the one the source derives, and returns the sentence that
// explains the decision.
func evolveClassifyType(want, live string, inKey bool) (evolveVerdict, string) {
	w := evolveNormalizeType(want)
	l := evolveNormalizeType(live)
	if w == l {
		return evolveTypeMatches, ""
	}
	// LowCardinality changes how values are stored, not which values
	// can be stored. The derived declaration says nothing about
	// encodings, so one applied by hand is a tuning decision to
	// respect rather than drift to undo.
	stripped := evolveStripLowCardinality(l)
	if stripped == w {
		return evolveTypeMatches, ""
	}
	if inKey {
		return evolveTypeImpossible, fmt.Sprintf(
			"%s is part of the sorting, primary or partition key, and ClickHouse will not ALTER such a column: "+
				"the key is the on-disk layout, and for this mirror it is also what ReplacingMergeTree deduplicates on. "+
				"Changing it to %s means creating a new table and copying into it", live, want)
	}
	wBase, wNull := evolveNullable(w)
	lBase, lNull := evolveNullable(stripped)
	if lNull && !wNull {
		return evolveTypeImpossible, fmt.Sprintf(
			"going from %s to %s means casting every stored NULL, and this mirror writes NULL into every "+
				"non-key column of every tombstone, so the mutation fails as soon as one delete has been mirrored. "+
				"Keep the column Nullable, or rebuild the table", live, want)
	}
	if wBase == lBase || evolveWidens(lBase, wBase) {
		return evolveTypeInPlace, fmt.Sprintf(
			"%s widens to %s, so every value ClickHouse already holds is still a value of the new type", live, want)
	}
	return evolveTypeLossy, fmt.Sprintf(
		"ClickHouse would cast every stored value from %s to %s, and drops cannot prove that conversion keeps "+
			"them — it can round, overflow, or throw part-way through the mutation", live, want)
}

// evolveNormalizeType strips the whitespace ClickHouse adds when it
// echoes a type back — "Decimal(10, 2)" for a column declared
// "Decimal(10,2)" — so that a comparison is about the type rather than
// about the server's formatting. Text inside quotes is left alone,
// because a time zone name is a value and not syntax.
func evolveNormalizeType(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	inQuote := false
	for _, r := range s {
		if r == '\'' {
			inQuote = !inQuote
		} else if !inQuote && (r == ' ' || r == '\t' || r == '\n' || r == '\r') {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// evolveStripLowCardinality removes one LowCardinality wrapper.
func evolveStripLowCardinality(t string) string {
	if inner, ok := evolveInner(t, "LowCardinality"); ok {
		return inner
	}
	return t
}

// evolveNullable splits Nullable(T) into T and true.
func evolveNullable(t string) (base string, nullable bool) {
	if inner, ok := evolveInner(t, "Nullable"); ok {
		return inner, true
	}
	return t, false
}

// evolveInner unwraps "Wrapper(inner)". It only ever runs against a
// normalised type, where the wrapper is the outermost token and there
// is no whitespace to skip.
func evolveInner(t, wrapper string) (string, bool) {
	if !strings.HasPrefix(t, wrapper+"(") || !strings.HasSuffix(t, ")") {
		return "", false
	}
	return t[len(wrapper)+1 : len(t)-1], true
}

// evolveWidens reports whether every value of type from is also a
// value of type to, which is the condition under which a MODIFY COLUMN
// can neither lose a value nor fail on one.
//
// The rule is deliberately narrow. Only same-family numeric widening
// and Decimal precision growth qualify; Int64 to Float64 does not,
// because floats above 2^53 stop being exact, and unsigned to signed
// does not, because the top of the range wraps.
func evolveWidens(from, to string) bool {
	if f, ok := evolveNumeric(from); ok {
		t, ok := evolveNumeric(to)
		return ok && f.family == t.family && t.bits > f.bits
	}
	fp, fs, ok := evolveDecimal(from)
	if !ok {
		return false
	}
	tp, ts, ok := evolveDecimal(to)
	return ok && ts == fs && tp > fp
}

// evolveNumericType is a ClickHouse fixed-width number.
type evolveNumericType struct {
	family string
	bits   int
}

// evolveNumeric parses "Int32", "UInt8", "Float64" and friends.
func evolveNumeric(t string) (evolveNumericType, bool) {
	// UInt before Int: "UInt8" carries both prefixes and only the
	// first is the family.
	for _, family := range []string{"UInt", "Int", "Float"} {
		if !strings.HasPrefix(t, family) {
			continue
		}
		bits, err := strconv.Atoi(t[len(family):])
		if err != nil {
			return evolveNumericType{}, false
		}
		return evolveNumericType{family: family, bits: bits}, true
	}
	return evolveNumericType{}, false
}

// evolveDecimal parses "Decimal(p,s)".
func evolveDecimal(t string) (precision, scale int, ok bool) {
	args, found := evolveInner(t, "Decimal")
	if !found {
		return 0, 0, false
	}
	p, s, split := strings.Cut(args, ",")
	if !split {
		return 0, 0, false
	}
	precision, err := strconv.Atoi(p)
	if err != nil {
		return 0, 0, false
	}
	scale, err = strconv.Atoi(s)
	if err != nil {
		return 0, 0, false
	}
	return precision, scale, true
}
