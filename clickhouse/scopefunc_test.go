package clickhouse_test

import (
	"testing"
	"time"

	"github.com/bernardoforcillo/drops"
	"github.com/bernardoforcillo/drops/clickhouse"
)

// The corpus: every expression shape this package can build that has no
// statement written inside it.
//
// It exists to hold one promise the operand rework makes — that moving
// the operands out of their closures changed WHERE a statement can be
// reached from and nothing about what an expression RENDERS. Each entry
// is rendered and compared against text captured from the commit before
// the rework, so a constructor that quietly gained or lost a
// parenthesis, a space or a bound argument fails here rather than in
// somebody's query log.
//
// Every entry is deliberately statement-free. An expression with a
// SELECT inside it is supposed to render differently once a ctx is in
// hand — that is the whole feature — so it belongs in scope_test.go,
// not here.

var (
	corpusTable  = clickhouse.NewTable("corpus")
	corpusID     = clickhouse.Add(corpusTable, clickhouse.UInt64("id"))
	corpusName   = clickhouse.Add(corpusTable, clickhouse.String("name"))
	corpusScore  = clickhouse.Add(corpusTable, clickhouse.Float64("score"))
	corpusTS     = clickhouse.Add(corpusTable, clickhouse.DateTime("ts", "UTC"))
	corpusOther  = clickhouse.NewTable("other")
	corpusOtherK = clickhouse.Add(corpusOther, clickhouse.UInt64("k"))
)

type corpusEntry struct {
	name string
	expr drops.Expression
}

func corpusEntries() []corpusEntry {
	win := clickhouse.WindowSpec().PartitionBy(corpusName).OrderBy(corpusTS.Desc())
	framed := clickhouse.WindowSpec().OrderBy(corpusTS.Asc()).Frame("ROWS BETWEEN 1 PRECEDING AND CURRENT ROW")
	return []corpusEntry{
		{"Eq/col-val", clickhouse.Eq(corpusID, 7)},
		{"Eq/col-col", clickhouse.Eq(corpusID, corpusOtherK)},
		{"Ne", clickhouse.Ne(corpusID, 7)},
		{"Gt", clickhouse.Gt(corpusScore, 1.5)},
		{"Gte", clickhouse.Gte(corpusScore, 1.5)},
		{"Lt", clickhouse.Lt(corpusScore, 1.5)},
		{"Lte", clickhouse.Lte(corpusScore, 1.5)},
		{"Like", clickhouse.Like(corpusName, "a%")},
		{"ILike", clickhouse.ILike(corpusName, "a%")},
		{"And/0", clickhouse.And()},
		{"And/1", clickhouse.And(clickhouse.Eq(corpusID, 1))},
		{"And/2", clickhouse.And(clickhouse.Eq(corpusID, 1), clickhouse.Eq(corpusName, "x"))},
		{"And/3", clickhouse.And(clickhouse.Eq(corpusID, 1), clickhouse.Eq(corpusName, "x"), clickhouse.IsNull(corpusTS))},
		{"Or/0", clickhouse.Or()},
		{"Or/1", clickhouse.Or(clickhouse.Eq(corpusID, 1))},
		{"Or/2", clickhouse.Or(clickhouse.Eq(corpusID, 1), clickhouse.Eq(corpusName, "x"))},
		{"And-of-Or", clickhouse.And(clickhouse.Or(clickhouse.Eq(corpusID, 1), clickhouse.Eq(corpusID, 2)), clickhouse.Eq(corpusName, "x"))},
		{"Not", clickhouse.Not(clickhouse.Eq(corpusID, 1))},
		{"Not/nested", clickhouse.Not(clickhouse.And(clickhouse.Eq(corpusID, 1), clickhouse.Eq(corpusID, 2)))},
		{"In/spread", clickhouse.In(corpusID, 1, 2, 3)},
		{"In/slice", clickhouse.In(corpusID, []int{1, 2, 3})},
		{"In/one", clickhouse.In(corpusID, 9)},
		{"In/empty", clickhouse.In(corpusID)},
		{"NotIn", clickhouse.NotIn(corpusID, 1, 2)},
		{"NotIn/empty", clickhouse.NotIn(corpusID)},
		{"In/col-operand", clickhouse.In(corpusID, corpusOtherK, 3)},
		{"IsNull", clickhouse.IsNull(corpusTS)},
		{"IsNotNull", clickhouse.IsNotNull(corpusTS)},
		{"Between", clickhouse.Between(corpusScore, 1, 2)},
		{"Between/exprs", clickhouse.Between(corpusScore, corpusOtherK, corpusID)},
		{"Count", clickhouse.Count(corpusID)},
		{"CountAll", clickhouse.CountAll()},
		{"Sum", clickhouse.Sum(corpusScore)},
		{"Avg", clickhouse.Avg(corpusScore)},
		{"Min", clickhouse.Min(corpusScore)},
		{"Max", clickhouse.Max(corpusScore)},
		{"Uniq", clickhouse.Uniq(corpusID)},
		{"UniqExact", clickhouse.UniqExact(corpusID)},
		{"UniqHLL12", clickhouse.UniqHLL12(corpusID)},
		{"AnyAgg", clickhouse.AnyAgg(corpusName)},
		{"AnyLast", clickhouse.AnyLast(corpusName)},
		{"AnyHeavy", clickhouse.AnyHeavy(corpusName)},
		{"Quantile", clickhouse.Quantile(0.95, corpusScore)},
		{"QuantileExact", clickhouse.QuantileExact(0.5, corpusScore)},
		{"QuantileTiming", clickhouse.QuantileTiming(0.99, corpusScore)},
		{"GroupArray", clickhouse.GroupArray(corpusName)},
		{"GroupUniqArray", clickhouse.GroupUniqArray(corpusName)},
		{"ArgMax", clickhouse.ArgMax(corpusName, corpusScore)},
		{"ArgMin", clickhouse.ArgMin(corpusName, corpusScore)},
		{"Lower", clickhouse.Lower(corpusName)},
		{"Upper", clickhouse.Upper(corpusName)},
		{"Length", clickhouse.Length(corpusName)},
		{"Abs", clickhouse.Abs(corpusScore)},
		{"Round", clickhouse.Round(corpusScore)},
		{"Coalesce", clickhouse.Coalesce(corpusName, "n/a")},
		{"IfNull", clickhouse.IfNull(corpusName, "n/a")},
		{"Now", clickhouse.Now()},
		{"ToDate", clickhouse.ToDate(corpusTS)},
		{"ToDateTime", clickhouse.ToDateTime(corpusTS)},
		{"ToStartOfDay", clickhouse.ToStartOfDay(corpusTS)},
		{"ToStartOfHour", clickhouse.ToStartOfHour(corpusTS)},
		{"ToStartOfMinute", clickhouse.ToStartOfMinute(corpusTS)},
		{"ToStartOfMonth", clickhouse.ToStartOfMonth(corpusTS)},
		{"ToYYYYMM", clickhouse.ToYYYYMM(corpusTS)},
		{"ToYYYYMMDD", clickhouse.ToYYYYMMDD(corpusTS)},
		{"DateDiff", clickhouse.DateDiff("day", corpusTS, clickhouse.Now())},
		{"As", clickhouse.As(clickhouse.Sum(corpusScore), "total")},
		{"Func/noargs", clickhouse.Func("rand")},
		{"Func/args", clickhouse.Func("multiIf", clickhouse.Eq(corpusID, 1), "a", "b")},
		{"Column.As", corpusName.As("n")},
		{"Column.Asc", corpusTS.Asc()},
		{"Column.Desc", corpusTS.Desc()},
		{"Column.WriteSQL", corpusID},
		{"CastAs", clickhouse.CastAs(corpusID, "UInt32")},
		{"Cast", clickhouse.Cast(corpusName, "String")},
		{"Case/searched", clickhouse.Case().When(clickhouse.Eq(corpusID, 1), "one").Else("many").End()},
		{"Case/simple", clickhouse.CaseOn(corpusID).When(1, "one").When(2, "two").End()},
		{"Case/no-else", clickhouse.Case().When(clickhouse.IsNull(corpusTS), 0).End()},
		{"Over/partition", clickhouse.Over(clickhouse.Sum(corpusScore), win)},
		{"Over/frame", clickhouse.Over(clickhouse.Avg(corpusScore), framed)},
		{"Over/empty", clickhouse.Over(clickhouse.CountAll(), clickhouse.WindowSpec())},
		{"RowNumber", clickhouse.RowNumber()},
		{"Rank", clickhouse.Rank()},
		{"DenseRank", clickhouse.DenseRank()},
		{"Lag", clickhouse.Lag(corpusScore, 1, 0)},
		{"Lead", clickhouse.Lead(corpusScore)},
		{"FirstValue", clickhouse.FirstValue(corpusScore)},
		{"LastValue", clickhouse.LastValue(corpusScore)},
		{"NthValue", clickhouse.NthValue(corpusScore, 2)},
		{"Exists/raw", clickhouse.Exists(drops.Raw("SELECT 1"))},
		{"NotExists/raw", clickhouse.NotExists(drops.Raw("SELECT 1"))},
		{"Subquery/raw", clickhouse.Subquery(drops.Raw("SELECT 1"))},
		{"InSub/raw", clickhouse.InSub(corpusID, drops.Raw("SELECT 1"))},
		{"NotInSub/raw", clickhouse.NotInSub(corpusID, drops.Raw("SELECT 1"))},
		{"Col.Eq", corpusID.Eq(1)},
		{"Col.In", corpusID.In(1, 2)},
		{"Col.IsNull", corpusTS.IsNull()},
		{"time-arg", clickhouse.Eq(corpusTS, time.Unix(0, 0).UTC())},
		{"CTE.Ref", clickhouse.CTEDef("c", drops.Raw("SELECT 1")).Ref()},
	}
}

// corpusBefore is the rendered text and bound-argument count of every
// entry in corpusEntries, captured by running the same corpus against
// the commit BEFORE the operands came out of their closures.
//
// It is generated once and edited never. A diff here is the change this
// round promised not to make: an expression with no statement inside it
// renders the bytes it always did, so that a package upgrade does not
// rewrite the SQL of every query somebody already reviewed.
var corpusBefore = []struct {
	name string
	sql  string
	args int
}{
	{`Eq/col-val`, `("corpus"."id" = ?)`, 1},
	{`Eq/col-col`, `("corpus"."id" = "other"."k")`, 0},
	{`Ne`, `("corpus"."id" != ?)`, 1},
	{`Gt`, `("corpus"."score" > ?)`, 1},
	{`Gte`, `("corpus"."score" >= ?)`, 1},
	{`Lt`, `("corpus"."score" < ?)`, 1},
	{`Lte`, `("corpus"."score" <= ?)`, 1},
	{`Like`, `("corpus"."name" LIKE ?)`, 1},
	{`ILike`, `("corpus"."name" ILIKE ?)`, 1},
	{`And/0`, `true`, 0},
	{`And/1`, `("corpus"."id" = ?)`, 1},
	{`And/2`, `(("corpus"."id" = ?) AND ("corpus"."name" = ?))`, 2},
	{`And/3`, `(("corpus"."id" = ?) AND ("corpus"."name" = ?) AND ("corpus"."ts" IS NULL))`, 2},
	{`Or/0`, `false`, 0},
	{`Or/1`, `("corpus"."id" = ?)`, 1},
	{`Or/2`, `(("corpus"."id" = ?) OR ("corpus"."name" = ?))`, 2},
	{`And-of-Or`, `((("corpus"."id" = ?) OR ("corpus"."id" = ?)) AND ("corpus"."name" = ?))`, 3},
	{`Not`, `(NOT ("corpus"."id" = ?))`, 1},
	{`Not/nested`, `(NOT (("corpus"."id" = ?) AND ("corpus"."id" = ?)))`, 2},
	{`In/spread`, `("corpus"."id" IN (?, ?, ?))`, 3},
	{`In/slice`, `("corpus"."id" IN (?, ?, ?))`, 3},
	{`In/one`, `("corpus"."id" IN (?))`, 1},
	{`In/empty`, `(false)`, 0},
	{`NotIn`, `("corpus"."id" NOT IN (?, ?))`, 2},
	{`NotIn/empty`, `(true)`, 0},
	{`In/col-operand`, `("corpus"."id" IN ("other"."k", ?))`, 1},
	{`IsNull`, `("corpus"."ts" IS NULL)`, 0},
	{`IsNotNull`, `("corpus"."ts" IS NOT NULL)`, 0},
	{`Between`, `("corpus"."score" BETWEEN ? AND ?)`, 2},
	{`Between/exprs`, `("corpus"."score" BETWEEN "other"."k" AND "corpus"."id")`, 0},
	{`Count`, `count("corpus"."id")`, 0},
	{`CountAll`, `count()`, 0},
	{`Sum`, `sum("corpus"."score")`, 0},
	{`Avg`, `avg("corpus"."score")`, 0},
	{`Min`, `min("corpus"."score")`, 0},
	{`Max`, `max("corpus"."score")`, 0},
	{`Uniq`, `uniq("corpus"."id")`, 0},
	{`UniqExact`, `uniqExact("corpus"."id")`, 0},
	{`UniqHLL12`, `uniqHLL12("corpus"."id")`, 0},
	{`AnyAgg`, `any("corpus"."name")`, 0},
	{`AnyLast`, `anyLast("corpus"."name")`, 0},
	{`AnyHeavy`, `anyHeavy("corpus"."name")`, 0},
	{`Quantile`, `quantile(?)("corpus"."score")`, 1},
	{`QuantileExact`, `quantileExact(?)("corpus"."score")`, 1},
	{`QuantileTiming`, `quantileTiming(?)("corpus"."score")`, 1},
	{`GroupArray`, `groupArray("corpus"."name")`, 0},
	{`GroupUniqArray`, `groupUniqArray("corpus"."name")`, 0},
	{`ArgMax`, `argMax("corpus"."name", "corpus"."score")`, 0},
	{`ArgMin`, `argMin("corpus"."name", "corpus"."score")`, 0},
	{`Lower`, `lower("corpus"."name")`, 0},
	{`Upper`, `upper("corpus"."name")`, 0},
	{`Length`, `length("corpus"."name")`, 0},
	{`Abs`, `abs("corpus"."score")`, 0},
	{`Round`, `round("corpus"."score")`, 0},
	{`Coalesce`, `coalesce("corpus"."name", ?)`, 1},
	{`IfNull`, `ifNull("corpus"."name", ?)`, 1},
	{`Now`, `now()`, 0},
	{`ToDate`, `toDate("corpus"."ts")`, 0},
	{`ToDateTime`, `toDateTime("corpus"."ts")`, 0},
	{`ToStartOfDay`, `toStartOfDay("corpus"."ts")`, 0},
	{`ToStartOfHour`, `toStartOfHour("corpus"."ts")`, 0},
	{`ToStartOfMinute`, `toStartOfMinute("corpus"."ts")`, 0},
	{`ToStartOfMonth`, `toStartOfMonth("corpus"."ts")`, 0},
	{`ToYYYYMM`, `toYYYYMM("corpus"."ts")`, 0},
	{`ToYYYYMMDD`, `toYYYYMMDD("corpus"."ts")`, 0},
	{`DateDiff`, `dateDiff(?, "corpus"."ts", now())`, 1},
	{`As`, `sum("corpus"."score") AS "total"`, 0},
	{`Func/noargs`, `rand()`, 0},
	{`Func/args`, `multiIf(("corpus"."id" = ?), ?, ?)`, 3},
	{`Column.As`, `"corpus"."name" AS "n"`, 0},
	{`Column.Asc`, `"corpus"."ts" ASC`, 0},
	{`Column.Desc`, `"corpus"."ts" DESC`, 0},
	{`Column.WriteSQL`, `"corpus"."id"`, 0},
	{`CastAs`, `CAST("corpus"."id" AS UInt32)`, 0},
	{`Cast`, `CAST("corpus"."name" AS String)`, 0},
	{`Case/searched`, `CASE WHEN ("corpus"."id" = ?) THEN ? ELSE ? END`, 3},
	{`Case/simple`, `CASE "corpus"."id" WHEN ? THEN ? WHEN ? THEN ? END`, 4},
	{`Case/no-else`, `CASE WHEN ("corpus"."ts" IS NULL) THEN ? END`, 1},
	{`Over/partition`, `sum("corpus"."score") OVER (PARTITION BY "corpus"."name" ORDER BY "corpus"."ts" DESC)`, 0},
	{`Over/frame`, `avg("corpus"."score") OVER (ORDER BY "corpus"."ts" ASC ROWS BETWEEN 1 PRECEDING AND CURRENT ROW)`, 0},
	{`Over/empty`, `count() OVER ()`, 0},
	{`RowNumber`, `row_number()`, 0},
	{`Rank`, `rank()`, 0},
	{`DenseRank`, `dense_rank()`, 0},
	{`Lag`, `lagInFrame("corpus"."score", ?, ?)`, 2},
	{`Lead`, `leadInFrame("corpus"."score")`, 0},
	{`FirstValue`, `first_value("corpus"."score")`, 0},
	{`LastValue`, `last_value("corpus"."score")`, 0},
	{`NthValue`, `nth_value("corpus"."score", ?)`, 1},
	{`Exists/raw`, `EXISTS (SELECT 1)`, 0},
	{`NotExists/raw`, `NOT EXISTS (SELECT 1)`, 0},
	{`Subquery/raw`, `(SELECT 1)`, 0},
	{`InSub/raw`, `("corpus"."id" IN (SELECT 1))`, 0},
	{`NotInSub/raw`, `("corpus"."id" NOT IN (SELECT 1))`, 0},
	{`Col.Eq`, `("corpus"."id" = ?)`, 1},
	{`Col.In`, `("corpus"."id" IN (?, ?))`, 2},
	{`Col.IsNull`, `("corpus"."ts" IS NULL)`, 0},
	{`time-arg`, `("corpus"."ts" = ?)`, 1},
	{`CTE.Ref`, `"c"`, 0},
}

// TestCorpusRendersUnchanged is that promise, checked entry by entry.
//
// It asserts on SQL text and argument count rather than on a
// round-trip, for the reason every test in this round does: a
// round-trip tells you the rows came back, and the failure being
// guarded against is a statement that comes back with the right rows
// today and the wrong parenthesisation forever.
func TestCorpusRendersUnchanged(t *testing.T) {
	entries := corpusEntries()
	if len(entries) != len(corpusBefore) {
		t.Fatalf("corpus size changed: %d entries, %d captured", len(entries), len(corpusBefore))
	}
	for i, e := range entries {
		want := corpusBefore[i]
		if e.name != want.name {
			t.Fatalf("corpus entry %d is %q, captured %q", i, e.name, want.name)
		}
		sql, args := clickhouse.ToSQL(e.expr)
		if sql != want.sql {
			t.Errorf("%s\n  got:  %s\n  want: %s", e.name, sql, want.sql)
		}
		if len(args) != want.args {
			t.Errorf("%s: %d args, want %d", e.name, len(args), want.args)
		}
	}
}

// TestCorpusRendersUnchangedTwice pins the other half of the same
// promise: an expression is a value a caller may hold and render into
// two statements, and holding the operands in a field rather than in a
// closure must not have made one of them stateful.
func TestCorpusRendersUnchangedTwice(t *testing.T) {
	for _, e := range corpusEntries() {
		first, firstArgs := clickhouse.ToSQL(e.expr)
		second, secondArgs := clickhouse.ToSQL(e.expr)
		if first != second || len(firstArgs) != len(secondArgs) {
			t.Errorf("%s renders differently the second time\n  1: %s\n  2: %s", e.name, first, second)
		}
	}
}
