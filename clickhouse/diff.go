package clickhouse

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// ErrRefused is what [Plan.Refused] wraps and what [Push] returns when
// the plan carries refusals. Test for it with errors.Is to tell "drops
// declined to run this" apart from "ClickHouse rejected a statement".
var ErrRefused = errors.New("drops/clickhouse: schema change refused")

// RefusalKind names a change ClickHouse will not make in place.
type RefusalKind string

const (
	// RefuseEngine is the table's engine, or one of the engine's
	// parameters, differing from the declaration.
	RefuseEngine RefusalKind = "engine"

	// RefuseSortingKey is the ORDER BY differing.
	RefuseSortingKey RefusalKind = "sorting_key"

	// RefusePrimaryKey is the PRIMARY KEY differing.
	RefusePrimaryKey RefusalKind = "primary_key"

	// RefusePartitionKey is the PARTITION BY differing.
	RefusePartitionKey RefusalKind = "partition_key"

	// RefuseColumnType is a type change on a column that takes part in
	// one of the keys.
	RefuseColumnType RefusalKind = "modify_column"

	// RefuseDropColumn is a drop of a column that takes part in one of
	// the keys.
	RefuseDropColumn RefusalKind = "drop_column"
)

// Refusal is a difference [Diff] saw and will not write a statement
// for, because ClickHouse has no statement that would make it.
//
// It is not a warning about a risky change; a risky change still gets
// its statement and is graded by [Analyze]. A refusal is the other
// thing entirely: there is no ALTER, at any cost, and the only way
// through is a new table and a copy. Saying so is the whole point —
// a migration tool that silently skips the change leaves a schema
// nobody has described.
type Refusal struct {
	// Kind is what could not be changed.
	Kind RefusalKind

	// Table is the qualified table the refusal concerns, spelled the
	// way a statement would spell it.
	Table string

	// Column is the column, empty for a refusal about the table
	// itself.
	Column string

	// From is what the server holds and To is what the declaration
	// says. Both are empty where the difference is not expressible as
	// two values.
	From string
	To   string

	// Detail is the full explanation, and always names the remedy.
	Detail string
}

// String renders the refusal as one line of review output. A refusal
// about the table itself names no column, and says so by leaving it
// out rather than by printing an empty one.
func (r Refusal) String() string {
	if r.Column == "" {
		return fmt.Sprintf("%s on %s: %s", r.Kind, r.Table, r.Detail)
	}
	return fmt.Sprintf("%s on %s.%s: %s", r.Kind, r.Table, strconv.Quote(r.Column), r.Detail)
}

// Notice is a difference [Diff] can see but chooses not to act on,
// because acting would churn: the statement would be re-emitted on
// every push without ever settling.
//
// Every one of them is a place where introspection cannot read back
// what a declaration can say, or where the server re-renders an
// expression in a spelling drops cannot compare against the one that
// was written. The withheld statement travels with the notice, so a
// caller who has looked and decided can run it by hand.
type Notice struct {
	// Rule is a stable identifier — "sample-key", "table-ttl",
	// "column-ttl", "unrepresentable-column".
	Rule string

	// Table is the qualified table, Object the column where one
	// applies.
	Table  string
	Object string

	// Message says what was seen and what was not done about it.
	Message string

	// SQL is the statement drops withheld, empty when there is none to
	// withhold.
	SQL string
}

// String renders the notice as "rule: message", with the withheld SQL
// appended when there is one.
func (n Notice) String() string {
	out := n.Rule + " on " + n.Table
	if n.Object != "" {
		out += "." + strconv.Quote(n.Object)
	}
	out += ": " + n.Message
	if n.SQL != "" {
		out += " (" + n.SQL + ")"
	}
	return out
}

// Plan is the difference between two snapshots: the statements that
// close it, the changes ClickHouse will not make, and the ones drops
// declined to guess at.
//
// A plan with refusals is not a plan that brings the two into line,
// however many statements it also carries.
type Plan struct {
	Statements []string
	Refusals   []Refusal
	Notices    []Notice
}

// Aligned reports whether the two snapshots already agree — nothing to
// run, nothing refused. A plan with no statements but with refusals is
// not aligned: it is stuck.
func (p Plan) Aligned() bool { return len(p.Statements) == 0 && len(p.Refusals) == 0 }

// Refused collects the plan's refusals into one error wrapping
// [ErrRefused], or returns nil when there are none.
func (p Plan) Refused() error {
	if len(p.Refusals) == 0 {
		return nil
	}
	parts := make([]string, 0, len(p.Refusals))
	for _, r := range p.Refusals {
		parts = append(parts, r.String())
	}
	return fmt.Errorf("%w: %s", ErrRefused, strings.Join(parts, "; "))
}

// DiffOptions tunes how [Diff] renders statements.
type DiffOptions struct {
	// Safe adds IF [NOT] EXISTS to every statement that takes it, so a
	// migration can be re-run after a failure part-way. ClickHouse
	// accepts it on CREATE TABLE, DROP TABLE, ADD COLUMN, DROP COLUMN,
	// MODIFY COLUMN and COMMENT COLUMN, which is most of what a diff
	// emits.
	//
	// [Push] turns it on and a generated migration leaves it off. The
	// difference is what the two are for: a push has no transaction
	// behind it and is expected to be re-run after a failure, while a
	// migration is a statement of what the schema should become, and
	// one that quietly does nothing when the table is already there is
	// a migration nobody can review.
	Safe bool
}

// Diff returns the statements that bring prev's schema to cur's,
// together with the changes ClickHouse cannot make and the ones drops
// will not guess at.
//
// Output is deterministic for a given (prev, cur, opts): keys are
// walked in sorted order, so re-running against the same input
// produces byte-identical SQL.
//
// # Why this returns a plan where the other dialects return statements
//
// drops/pg, drops/mysql and drops/sqlite return []string, because in
// those dialects every schema difference has a statement behind it,
// even when the statement is a slow or a destructive one. ClickHouse
// is not like that. It has no ALTER that changes a table's engine, its
// partitioning, its primary key, or — beyond appending columns the
// same statement adds — its sorting key, and it will not alter or drop
// a column that takes part in any of those keys. Those differences are
// real, a diff has to report them, and there is no SQL to report them
// as. So they come back as [Refusal] values that name the remedy.
//
// For a ReplacingMergeTree the sorting key is worth singling out. The
// engine collapses rows that share it, so the key is not a layout
// choice that happens to affect performance — it is the definition of
// "the same row". Change it and rows that used to merge into one stop
// merging, which is a data change wearing a schema change's clothes.
//
// # Operation order
//
//  1. DROP TABLE for tables removed entirely;
//  2. CREATE TABLE for new ones;
//  3. per surviving table: the refusals about the table itself, then
//     column removals of a property (REMOVE DEFAULT, REMOVE CODEC),
//     then ADD COLUMN in ascending position, then — per column, in
//     name order — its MODIFY COLUMN followed by its COMMENT COLUMN,
//     then DROP COLUMN, then the table's TTL and SETTINGS.
//
// Only the phases are ordered against each other; within the
// per-column phase the statements for one column stay together,
// because no statement there depends on one for a different column.
//
// The removals come before the restatements on purpose. ClickHouse's
// MODIFY COLUMN takes a whole column declaration, and drops does not
// claim to know whether a clause the restatement leaves out is
// preserved or reset — the existence of an explicit REMOVE sub-clause
// argues one way and MySQL's MODIFY, which resets, argues the other.
// Removing first and restating second reaches the declared shape under
// either reading.
//
// ADD COLUMN runs in ascending position and always says AFTER or
// FIRST: without it ClickHouse appends, so a column added to the
// middle of a declaration lands at the end and every later
// introspection reports a layout nobody chose. The column an AFTER
// names is the preceding declared one, which is either already live or
// added by an earlier statement of this same plan.
//
// # There is no DiffDown
//
// The other dialects offer one, and here it would be a lie. Most of
// what a ClickHouse migration does cannot be reversed by a statement
// at all — a dropped column's data is gone, a rebuilt table is a
// different table — so drops does not offer a function whose name
// promises otherwise. Diff(cur, prev) is one call away for anyone who
// wants the other direction and has read this far.
func Diff(prev, cur *Snapshot, opts ...DiffOptions) Plan {
	var opt DiffOptions
	if len(opts) > 0 {
		opt = opts[0]
	}
	if prev == nil {
		prev = EmptySnapshot()
	}
	if cur == nil {
		cur = EmptySnapshot()
	}
	var p Plan

	for _, key := range sortedKeys(prev.Tables) {
		if _, ok := cur.Tables[key]; ok {
			continue
		}
		p.Statements = append(p.Statements, dropTableSQL(prev.Tables[key], opt))
	}
	for _, key := range sortedKeys(cur.Tables) {
		if _, ok := prev.Tables[key]; ok {
			continue
		}
		p.Statements = append(p.Statements, CreateTableSQL(cur.Tables[key], opt))
	}
	for _, key := range sortedKeys(cur.Tables) {
		before, ok := prev.Tables[key]
		if !ok {
			continue
		}
		diffTable(&p, before, cur.Tables[key], opt)
	}
	return p
}

// diffTable adds the differences between one live table and its
// declaration.
func diffTable(p *Plan, prev, cur *TableSnapshot, opt DiffOptions) {
	name := QualifiedName(cur.Database, cur.Name)
	diffShape(p, name, prev, cur)

	// A key column cannot be altered or dropped, so the column pass
	// asks the live side about the keys rather than the declared one:
	// what stops the statement is the layout the data is already
	// written in.
	for _, colName := range sortedKeys(cur.Columns) {
		want := cur.Columns[colName]
		live, ok := prev.Columns[colName]
		if !ok {
			continue
		}
		diffColumnRemovals(p, name, live, want, opt)
	}
	for _, want := range columnsByPosition(cur) {
		if _, ok := prev.Columns[want.Name]; ok {
			continue
		}
		p.Statements = append(p.Statements, addColumnSQL(name, cur, want, opt))
	}
	for _, colName := range sortedKeys(cur.Columns) {
		want := cur.Columns[colName]
		live, ok := prev.Columns[colName]
		if !ok {
			continue
		}
		diffColumn(p, name, live, want, opt)
	}
	for _, colName := range sortedKeys(prev.Columns) {
		if _, ok := cur.Columns[colName]; ok {
			continue
		}
		diffDropColumn(p, name, prev.Columns[colName], opt)
	}
	diffTail(p, name, prev, cur)
}

// diffShape records the differences that are about the table rather
// than about one of its columns. None of them is expressible as an
// ALTER on a table that holds data, so all of them are refusals.
func diffShape(p *Plan, name string, prev, cur *TableSnapshot) {
	if cur.Engine != "" && !strings.EqualFold(prev.Engine, cur.Engine) {
		p.Refusals = append(p.Refusals, Refusal{
			Kind: RefuseEngine, Table: name, From: prev.Engine, To: cur.Engine,
			Detail: fmt.Sprintf(
				"the table is a %s and the declaration says %s. ClickHouse has no ALTER that changes a table's "+
					"engine, and the two do not hold data the same way — a MergeTree where a "+
					"ReplacingMergeTree was meant deduplicates nothing, so a replayed batch is duplicate rows "+
					"rather than one row twice written. Create a table with the engine you want and copy across",
				engineText(prev.Engine, prev.EngineParams), engineText(cur.Engine, cur.EngineParams)),
		})
	} else if cur.Engine != "" && !sameStrings(prev.EngineParams, cur.EngineParams) {
		p.Refusals = append(p.Refusals, Refusal{
			Kind: RefuseEngine, Table: name,
			From: engineText(prev.Engine, prev.EngineParams),
			To:   engineText(cur.Engine, cur.EngineParams),
			Detail: "the engine is right and its parameters are not, and ClickHouse has no ALTER for them " +
				"either. The parameters are not decoration: the version column of a ReplacingMergeTree is how " +
				"it picks which of two rows with the same sorting key survives, and a table that lost it keeps " +
				"whichever part merged last. Create a table with the parameters you want and copy across",
		})
	}
	if prev.SortingKey != cur.SortingKey {
		p.Refusals = append(p.Refusals, Refusal{
			Kind: RefuseSortingKey, Table: name, From: keyText(prev.SortingKey), To: keyText(cur.SortingKey),
			Detail: fmt.Sprintf(
				"the table sorts by %s and the declaration says %s. ClickHouse's MODIFY ORDER BY only extends a "+
					"sorting key with columns the same statement adds — the rows in every existing part are "+
					"already written in the old order and no metadata change reorders them — so there is no "+
					"ALTER for this. On a ReplacingMergeTree it is more than a layout change: the engine "+
					"collapses rows that share the sorting key, so changing the key changes which rows are "+
					"\"the same row\", and every group the old key merged into one comes back apart. Create a "+
					"table with the key you want and copy the data across",
				keyText(prev.SortingKey), keyText(cur.SortingKey)),
		})
	}
	if prev.PrimaryKey != cur.PrimaryKey {
		p.Refusals = append(p.Refusals, Refusal{
			Kind: RefusePrimaryKey, Table: name, From: keyText(prev.PrimaryKey), To: keyText(cur.PrimaryKey),
			Detail: "the primary key differs, and ClickHouse has no ALTER that changes it: the primary key is " +
				"the sparse index every part carries on disk. Note that a table declared with ORDER BY and no " +
				"PRIMARY KEY has the sorting key as its primary key, which is what drops records for it, so a " +
				"primary key that differs on its own means an explicit PRIMARY KEY was added, removed or " +
				"changed. Create a table with the key you want and copy across",
		})
	}
	if prev.PartitionKey != cur.PartitionKey {
		p.Refusals = append(p.Refusals, Refusal{
			Kind: RefusePartitionKey, Table: name, From: keyText(prev.PartitionKey), To: keyText(cur.PartitionKey),
			Detail: fmt.Sprintf(
				"the table is partitioned by %s and the declaration says %s. ClickHouse has no ALTER that "+
					"changes a table's partitioning — the partition value decides which directory a part is "+
					"written into, so changing it means rewriting every part. It also decides what a "+
					"ReplacingMergeTree can collapse, since the engine only ever merges within one partition: "+
					"a partitioning that is not a function of the sorting key keeps every version of a row "+
					"whose partition value changed. Create a table with the layout you want and copy across",
				keyText(prev.PartitionKey), keyText(cur.PartitionKey)),
		})
	}
	if prev.SampleBy != cur.SampleBy {
		p.Notices = append(p.Notices, Notice{
			Rule: "sample-key", Table: name,
			Message: fmt.Sprintf(
				"the table samples by %s and the declaration says %s. ClickHouse does have MODIFY SAMPLE BY, "+
					"but it takes only a key that is already a prefix of the primary key, so drops leaves the "+
					"statement to a human who can check that",
				keyText(prev.SampleBy), keyText(cur.SampleBy)),
			SQL: "ALTER TABLE " + name + " MODIFY SAMPLE BY " + cur.SampleBy,
		})
	}
}

// diffColumnRemovals emits the REMOVE statements for a property the
// live column has and the declaration does not.
//
// They are separate statements rather than an omission from the
// restatement because an omission is ambiguous; see [Diff].
func diffColumnRemovals(p *Plan, table string, live, want *ColumnSnapshot, opt DiffOptions) {
	if live.InKey() {
		// Nothing here can run: an ALTER of a key column is refused
		// whatever it asks for. diffColumn reports it once.
		return
	}
	if live.DefaultKind == "DEFAULT" && live.Default != "" && want.Default == "" {
		p.Statements = append(p.Statements,
			"ALTER TABLE "+table+" MODIFY COLUMN "+ifExists(opt)+Dialect.QuoteIdent(want.Name)+" REMOVE DEFAULT")
	}
	if live.Codec != "" && want.Codec == "" {
		p.Statements = append(p.Statements,
			"ALTER TABLE "+table+" MODIFY COLUMN "+ifExists(opt)+Dialect.QuoteIdent(want.Name)+" REMOVE CODEC")
	}
}

// diffColumn records what a column's differences mean.
func diffColumn(p *Plan, table string, live, want *ColumnSnapshot, opt DiffOptions) {
	if live.DefaultKind != "" && live.DefaultKind != "DEFAULT" {
		p.Notices = append(p.Notices, Notice{
			Rule: "unrepresentable-column", Table: table, Object: want.Name,
			Message: "the live column is " + live.DefaultKind + ", which this DSL cannot declare; drops compares " +
				"its type and leaves its expression alone rather than proposing to turn it into a plain column",
		})
	}
	if want.TTL != "" {
		p.Notices = append(p.Notices, Notice{
			Rule: "column-ttl", Table: table, Object: want.Name,
			Message: "the declaration gives this column a TTL and system.columns does not report one, so drops " +
				"cannot tell whether the server already has it. A restatement forced by some other change to " +
				"the column carries the TTL along; on its own it is withheld rather than emitted on every " +
				"push. Run it once by hand, or read the column back with SHOW CREATE TABLE",
			SQL: "ALTER TABLE " + table + " MODIFY COLUMN " + Dialect.QuoteIdent(want.Name) +
				" " + want.Type + " TTL " + want.TTL,
		})
	}

	change := ClassifyTypeChange(want.Type, live.Type, live.InKey())
	if change.Verdict == TypeRefused {
		p.Refusals = append(p.Refusals, Refusal{
			Kind: RefuseColumnType, Table: table, Column: want.Name,
			From: live.Type, To: want.Type, Detail: change.Why,
		})
		return
	}
	// A key column's storage is out of reach, so the only thing left
	// to say about one is its comment, which is handled below and is
	// metadata on any column.
	if live.InKey() {
		diffColumnComment(p, table, live, want, opt)
		return
	}

	// A clause the declaration no longer has was taken off above, so
	// what is left to restate is a type that moved and a clause the
	// declaration wants set. Restating for a removal as well would
	// emit a MODIFY COLUMN that says nothing the REMOVE before it did
	// not already say.
	setsDefault := want.Default != "" && NormalizeKeyExpr(live.Default) != NormalizeKeyExpr(want.Default)
	setsCodec := want.Codec != "" && live.Codec != want.Codec
	restate := change.Verdict != TypeUnchanged || setsDefault || setsCodec
	if restate {
		typ := change.To
		if typ == "" {
			// Unchanged type, some other property moved. MODIFY COLUMN
			// takes a whole declaration, so the type has to be in it,
			// and the live spelling is the one the server already
			// agrees with.
			typ = live.Type
		}
		// The restatement carries the comment and the TTL as well as
		// the type, so that the column reaches the declared shape
		// whether ClickHouse preserves an omitted clause or resets it.
		p.Statements = append(p.Statements,
			"ALTER TABLE "+table+" MODIFY COLUMN "+ifExists(opt)+
				columnDefSQL(want.Name, typ, want.Default, want.Codec, want.TTL, want.Comment))
	}
	diffColumnComment(p, table, live, want, opt)
}

// diffColumnComment emits the COMMENT COLUMN for a comment that moved.
//
// It is its own statement rather than part of a restatement: it is
// metadata alone, it cannot rewrite anything, it is the one change
// reachable on a key column, and an empty literal is how a comment is
// removed — there is no statement that leaves a column with none. It
// is emitted even after a restatement that already carried the
// comment, because a restatement that preserves rather than resets
// would otherwise leave the old one in place, and setting the same
// comment twice costs nothing.
func diffColumnComment(p *Plan, table string, live, want *ColumnSnapshot, opt DiffOptions) {
	if live.Comment == want.Comment {
		return
	}
	p.Statements = append(p.Statements,
		"ALTER TABLE "+table+" COMMENT COLUMN "+ifExists(opt)+
			Dialect.QuoteIdent(want.Name)+" "+quoteLiteral(want.Comment))
}

// diffDropColumn records what a column the server has and the
// declaration does not mean.
func diffDropColumn(p *Plan, table string, live *ColumnSnapshot, opt DiffOptions) {
	if live.InKey() {
		p.Refusals = append(p.Refusals, Refusal{
			Kind: RefuseDropColumn, Table: table, Column: live.Name, From: live.Type,
			Detail: "ClickHouse will not drop a column that takes part in the sorting, primary or partition " +
				"key: the key is how every part on disk is ordered and named. A table without this column is " +
				"a table with a different key, so create it with the key you want and copy across",
		})
		return
	}
	p.Statements = append(p.Statements,
		"ALTER TABLE "+table+" DROP COLUMN "+ifExists(opt)+Dialect.QuoteIdent(live.Name))
}

// diffTail records the table-wide TTL and the SETTINGS.
func diffTail(p *Plan, table string, prev, cur *TableSnapshot) {
	switch {
	case prev.TTL == "" && cur.TTL != "":
		p.Statements = append(p.Statements, "ALTER TABLE "+table+" MODIFY TTL "+cur.TTL)
	case prev.TTL != "" && cur.TTL == "":
		p.Statements = append(p.Statements, "ALTER TABLE "+table+" REMOVE TTL")
	case prev.TTL != cur.TTL && NormalizeKeyExpr(prev.TTL) != NormalizeKeyExpr(cur.TTL):
		// Both sides have a TTL and the two do not read alike, which
		// is not the same as their meaning differently: ClickHouse
		// re-renders a TTL from its own parse tree, so a declared
		// "ts + INTERVAL 30 DAY" reads back as "ts + toIntervalDay(30)".
		// drops cannot tell that apart from a real change without
		// parsing SQL, and emitting the statement on the chance would
		// re-emit it on every push for ever.
		p.Notices = append(p.Notices, Notice{
			Rule: "table-ttl", Table: table,
			Message: "the table's TTL is " + strconv.Quote(prev.TTL) + " and the declaration says " +
				strconv.Quote(cur.TTL) + ". ClickHouse re-renders a TTL in its own spelling, so drops cannot " +
				"tell a changed expression from the same one written differently, and withholds the statement " +
				"rather than re-emitting it on every push",
			SQL: "ALTER TABLE " + table + " MODIFY TTL " + cur.TTL,
		})
	}

	// Only the settings the declaration names. ClickHouse materialises
	// an engine's defaults into the table's metadata — every MergeTree
	// reports index_granularity whether or not anyone asked for it —
	// so a live setting with no declared counterpart is not evidence
	// that anything was removed, and resetting it would fight the
	// server on every push.
	for _, key := range sortedKeys(cur.Settings) {
		value := cur.Settings[key]
		if live, ok := prev.Settings[key]; ok && live == value {
			continue
		}
		p.Statements = append(p.Statements, "ALTER TABLE "+table+" MODIFY SETTING "+key+" = "+value)
	}
}

// ----------------------------------------------------------------------
// Statement rendering
// ----------------------------------------------------------------------

// QualifiedName spells a table the way a statement has to spell it.
func QualifiedName(database, name string) string {
	if database == "" {
		return Dialect.QuoteIdent(name)
	}
	return Dialect.QuoteIdent(database) + "." + Dialect.QuoteIdent(name)
}

// CreateTableSQL renders the CREATE TABLE for a snapshot — the
// statement [Diff] emits for a table the server does not have.
//
// The clause order is the one [CreateTable] renders from a live
// *Table, and it is not free to vary: ClickHouse's column-declaration
// parser reads DEFAULT, then COMMENT, then CODEC, then TTL, and a
// comment emitted after a codec is a syntax error for the whole
// statement. The two renderers are kept in step by a test that asserts
// they agree.
func CreateTableSQL(ts *TableSnapshot, opts ...DiffOptions) string {
	var opt DiffOptions
	if len(opts) > 0 {
		opt = opts[0]
	}
	var b strings.Builder
	b.WriteString("CREATE TABLE ")
	if opt.Safe {
		b.WriteString("IF NOT EXISTS ")
	}
	b.WriteString(QualifiedName(ts.Database, ts.Name))
	b.WriteString(" (\n")
	for i, c := range columnsByPosition(ts) {
		if i > 0 {
			b.WriteString(",\n")
		}
		b.WriteByte('\t')
		b.WriteString(columnDefSQL(c.Name, c.Type, c.Default, c.Codec, c.TTL, c.Comment))
	}
	b.WriteString("\n) ENGINE = ")
	b.WriteString(engineText(ts.Engine, ts.EngineParams))
	if ts.SortingKey != "" {
		b.WriteString("\nORDER BY (")
		b.WriteString(ts.SortingKey)
		b.WriteByte(')')
	}
	if ts.PartitionKey != "" {
		b.WriteString("\nPARTITION BY (")
		b.WriteString(ts.PartitionKey)
		b.WriteByte(')')
	}
	// The primary key is only written when it is not simply the
	// sorting key, which is what ClickHouse defaults it to and what
	// BuildSnapshot records for a declaration that leaves it out.
	if ts.PrimaryKey != "" && ts.PrimaryKey != ts.SortingKey {
		b.WriteString("\nPRIMARY KEY (")
		b.WriteString(ts.PrimaryKey)
		b.WriteByte(')')
	}
	if ts.SampleBy != "" {
		b.WriteString("\nSAMPLE BY ")
		b.WriteString(ts.SampleBy)
	}
	if ts.TTL != "" {
		b.WriteString("\nTTL ")
		b.WriteString(ts.TTL)
	}
	if len(ts.Settings) > 0 {
		b.WriteString("\nSETTINGS ")
		pairs := make([]string, 0, len(ts.Settings))
		for _, k := range sortedKeys(ts.Settings) {
			pairs = append(pairs, k+" = "+ts.Settings[k])
		}
		b.WriteString(strings.Join(pairs, ", "))
	}
	return b.String()
}

// dropTableSQL renders the DROP TABLE.
func dropTableSQL(ts *TableSnapshot, opt DiffOptions) string {
	out := "DROP TABLE "
	if opt.Safe {
		out += "IF EXISTS "
	}
	return out + QualifiedName(ts.Database, ts.Name)
}

// addColumnSQL renders the ADD COLUMN, positioned against the column
// the declaration puts before it.
func addColumnSQL(table string, ts *TableSnapshot, want *ColumnSnapshot, opt DiffOptions) string {
	out := "ALTER TABLE " + table + " ADD COLUMN "
	if opt.Safe {
		out += "IF NOT EXISTS "
	}
	out += columnDefSQL(want.Name, want.Type, want.Default, want.Codec, want.TTL, want.Comment)
	if prev := columnBefore(ts, want); prev != "" {
		out += " AFTER " + Dialect.QuoteIdent(prev)
	} else {
		out += " FIRST"
	}
	return out
}

// columnBefore returns the name of the declared column immediately
// preceding want, or "" when want is first.
func columnBefore(ts *TableSnapshot, want *ColumnSnapshot) string {
	var best *ColumnSnapshot
	for _, c := range ts.Columns {
		if c.Position >= want.Position {
			continue
		}
		if best == nil || c.Position > best.Position {
			best = c
		}
	}
	if best == nil {
		return ""
	}
	return best.Name
}

// columnsByPosition returns a table's columns in declaration order,
// falling back to name order for a snapshot that carries no positions.
func columnsByPosition(ts *TableSnapshot) []*ColumnSnapshot {
	out := make([]*ColumnSnapshot, 0, len(ts.Columns))
	for _, name := range sortedKeys(ts.Columns) {
		out = append(out, ts.Columns[name])
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j-1].Position > out[j].Position; j-- {
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
	return out
}

// engineText renders an engine and its parameters. An engine with none
// still renders its parentheses, which is what [MergeTree] and friends
// emit and what a live server accepts.
//
// A snapshot with no engine at all can only come from a declaration
// that never set one, and ClickHouse refuses CREATE TABLE without an
// ENGINE clause. It renders the same loud marker [CreateTable] does,
// so the statement fails with a readable message rather than a bare
// syntax error — see [ErrEngineRequired] for the checked form.
func engineText(name string, params []string) string {
	if name == "" {
		return "/* drops/clickhouse: engine missing — call .Engine(...) on the table */"
	}
	return name + "(" + strings.Join(params, ", ") + ")"
}

// keyText renders a key for a human reading a refusal.
func keyText(expr string) string {
	if expr == "" {
		return "nothing at all"
	}
	return expr
}

// ifExists renders the IF EXISTS a Safe diff puts after MODIFY COLUMN,
// DROP COLUMN and COMMENT COLUMN, trailing space included.
func ifExists(opt DiffOptions) string {
	if opt.Safe {
		return "IF EXISTS "
	}
	return ""
}

// sameStrings reports whether two ordered lists hold the same values.
func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// columnDefSQL renders "name Type [DEFAULT …] [COMMENT '…'] [CODEC(…)]
// [TTL …]" — the column declaration ClickHouse's parser reads in that
// exact order, in both CREATE TABLE and ALTER TABLE.
//
// The optional comment is variadic so the ALTER paths that must not
// restate a comment can leave it out. This is the same clause order
// [writeColumnDef] renders from a live *Column; the two are separate
// because one works from a *Column and the other from a
// [ColumnSnapshot], and a test asserts they agree.
func columnDefSQL(name, typ, defaultExpr, codec, ttl string, comment ...string) string {
	var b strings.Builder
	b.WriteString(Dialect.QuoteIdent(name))
	b.WriteByte(' ')
	b.WriteString(typ)
	if defaultExpr != "" {
		b.WriteString(" DEFAULT ")
		b.WriteString(defaultExpr)
	}
	if len(comment) > 0 && comment[0] != "" {
		b.WriteString(" COMMENT ")
		b.WriteString(quoteLiteral(comment[0]))
	}
	if codec != "" {
		b.WriteString(" CODEC(")
		b.WriteString(codec)
		b.WriteByte(')')
	}
	if ttl != "" {
		b.WriteString(" TTL ")
		b.WriteString(ttl)
	}
	return b.String()
}
