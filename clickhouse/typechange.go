package clickhouse

import (
	"fmt"
	"strconv"
	"strings"
)

// TypeVerdict is what a difference between a column's live type and
// its declared one means for ALTER TABLE … MODIFY COLUMN.
//
// The distinction that matters is not "big change" against "small
// change" — it is whether the statement can lose a value, and whether
// ClickHouse will run it at all. A widening cannot lose anything and
// needs nobody's permission; a narrowing runs a mutation whose outcome
// depends on data no static analysis can see; a key column cannot be
// altered on any terms.
type TypeVerdict int

const (
	// TypeUnchanged means the two types are the same type, however
	// differently they are spelled.
	TypeUnchanged TypeVerdict = iota

	// TypeWidens means every value the column already holds is still
	// a value of the new type, so the cast can neither round,
	// overflow nor throw.
	TypeWidens

	// TypeUnprovable means ClickHouse will run the mutation and the
	// result depends on the stored data: the cast can round, overflow
	// or throw part-way through.
	TypeUnprovable

	// TypeRefused means ClickHouse will not make the change at all,
	// whatever the caller permits. The remedy is always a new table
	// and a copy.
	TypeRefused
)

// String renders the verdict for a log line.
func (v TypeVerdict) String() string {
	switch v {
	case TypeUnchanged:
		return "unchanged"
	case TypeWidens:
		return "widens"
	case TypeUnprovable:
		return "unprovable"
	case TypeRefused:
		return "refused"
	}
	return "unknown"
}

// TypeCause names why [ClassifyTypeChange] answered as it did, so a
// caller that knows more about the table than the classifier does can
// take its own view. drops/mirror does exactly that with
// [CauseNullRemoval]: a mirror writes NULL into every non-key column
// of every tombstone, so what is merely unprovable for a table in
// general is certain to fail there.
type TypeCause string

const (
	// CauseNone accompanies TypeUnchanged and TypeWidens.
	CauseNone TypeCause = ""

	// CauseKeyColumn is a column in the sorting, primary or partition
	// key. ClickHouse refuses to alter one because those keys are the
	// on-disk layout.
	CauseKeyColumn TypeCause = "key_column"

	// CauseNullRemoval is Nullable(T) becoming T. The mutation fails
	// on the first NULL it meets, and succeeds on a column that
	// happens to hold none.
	CauseNullRemoval TypeCause = "null_removal"

	// CauseLowCardinalityWrapper is a live LowCardinality wrapper that
	// cannot travel onto the declared type. Both answers lose
	// something, so neither is taken unasked.
	CauseLowCardinalityWrapper TypeCause = "low_cardinality_wrapper"

	// CauseUnprovableCast is a cast between two types where some value
	// of the first is not a value of the second.
	CauseUnprovableCast TypeCause = "unprovable_cast"
)

// TypeChange is [ClassifyTypeChange]'s answer: the verdict, the type a
// statement should actually set, and the reasoning behind both.
type TypeChange struct {
	// Verdict is what the change means for ALTER.
	Verdict TypeVerdict

	// From is the live type as it was given, in the server's own
	// spelling. Want is the declared one. Neither is normalised, so
	// they read back the way whoever has to approve the change wrote
	// them.
	From string
	Want string

	// To is the type a MODIFY COLUMN should set, which is Want except
	// where a live LowCardinality wrapper is carried onto it. It is
	// empty when there is no statement to write — an unchanged type,
	// or one ClickHouse refuses to alter.
	To string

	// Cause names the reason. See [TypeCause].
	Cause TypeCause

	// LowCardinality reports that the live type carries a
	// LowCardinality wrapper. The wrapper is an encoding rather than a
	// value domain, so it is preserved where ClickHouse accepts it on
	// the new type and reported as a loss where it does not.
	LowCardinality bool

	// ValuesAtRisk reports that the cast itself may not keep every
	// stored value, separately from whatever the encoding does. A
	// column can fail both at once, and a caller told only about the
	// encoding hears a storage-size problem where there is a data one.
	ValuesAtRisk bool

	// Why is the one-sentence explanation, phrased for whoever has to
	// approve the change.
	Why string
}

// ClassifyTypeChange decides what ALTER TABLE … MODIFY COLUMN can do
// about a column whose live type is not the one the declaration says.
//
// want is the declared type, live is the type the server reports, and
// inKey says whether the column takes part in the sorting, primary or
// partition key — [ColumnSnapshot.InKey] answers that from
// system.columns.
//
// Spelling is not a difference: both sides are compared through
// [NormalizeType], so the "Decimal(10, 2)" ClickHouse echoes back is
// the "Decimal(10,2)" that was declared.
//
// A LowCardinality wrapper on the live column is treated as a tuning
// decision rather than as drift. It changes how values are stored, not
// which values can be stored, and a declaration says nothing about
// encodings — so a column whose type is otherwise unchanged is left
// alone, and where the type does change the wrapper is carried onto
// the new type wherever ClickHouse accepts it there. It does not
// accept it on a fixed-size type of eight bytes or less without
// allow_suspicious_low_cardinality_types, which of the ordinary
// scalar types leaves String; anywhere else the change is reported
// [TypeUnprovable] with [CauseLowCardinalityWrapper], because both
// available answers destroy something.
func ClassifyTypeChange(want, live string, inKey bool) TypeChange {
	out := TypeChange{From: live, Want: want}

	w := NormalizeType(want)
	l := NormalizeType(live)
	if w == l {
		return out
	}
	stripped := StripLowCardinality(l)
	out.LowCardinality = stripped != l
	if stripped == w {
		// The wrapper is the whole of the difference. It is not the
		// declaration's to have an opinion about.
		return out
	}
	if inKey {
		out.Verdict, out.Cause = TypeRefused, CauseKeyColumn
		out.Why = fmt.Sprintf(
			"%s is part of the sorting, primary or partition key, and ClickHouse will not ALTER such a column: "+
				"the key is the on-disk layout every part is written in. Changing it to %s means creating a "+
				"table with the key you want and copying into it", live, want)
		return out
	}

	wBase, wNull := SplitNullable(w)
	lBase, lNull := SplitNullable(stripped)
	if lNull && !wNull {
		out.Verdict, out.Cause = TypeUnprovable, CauseNullRemoval
		out.To = want
		out.ValuesAtRisk = true
		out.Why = fmt.Sprintf(
			"going from %s to %s means casting every stored NULL, and there is no value of %s for the cast to "+
				"produce: the mutation throws on the first NULL row it reaches and leaves the column half "+
				"rewritten. It succeeds only on a column that happens to hold none", live, want, want)
		return out
	}

	// Whether the values themselves survive is a separate question
	// from whether the encoding does, and a column can fail both at
	// once. Settle it first so neither answer can hide the other.
	keepsEveryValue := wBase == lBase || Widens(lBase, wBase)
	out.ValuesAtRisk = !keepsEveryValue

	if out.LowCardinality && !LowCardinalitySafe(wBase) {
		out.Verdict, out.Cause = TypeUnprovable, CauseLowCardinalityWrapper
		out.To = want
		out.Why = fmt.Sprintf(
			"the column is %s where the declaration says %s, and the wrapper cannot come along: ClickHouse "+
				"refuses LowCardinality(%s) unless allow_suspicious_low_cardinality_types is set, so the "+
				"statement would rewrite the whole column back to an unencoded %s and lose an encoding somebody "+
				"chose deliberately; keeping it means a MODIFY COLUMN run by hand",
			live, want, want, want)
		if !keepsEveryValue {
			out.Why += fmt.Sprintf(
				". The values are the larger risk: ClickHouse would cast every one of them from %s to %s, and "+
					"drops cannot prove that conversion keeps them — it can round, overflow, or throw part-way "+
					"through the mutation", live, want)
		}
		return out
	}

	if out.LowCardinality {
		out.To = "LowCardinality(" + want + ")"
	} else {
		out.To = want
	}
	if keepsEveryValue {
		out.Verdict = TypeWidens
		out.Why = fmt.Sprintf(
			"%s widens to %s, so every value ClickHouse already holds is still a value of the new type", live, out.To)
		if out.LowCardinality {
			out.Why += ", and the LowCardinality wrapper is kept rather than undone"
		}
		return out
	}
	out.Verdict, out.Cause = TypeUnprovable, CauseUnprovableCast
	out.Why = fmt.Sprintf(
		"ClickHouse would cast every stored value from %s to %s, and drops cannot prove that conversion keeps "+
			"them — it can round, overflow, or throw part-way through the mutation", live, out.To)
	return out
}

// LowCardinalitySafe reports whether ClickHouse accepts a
// LowCardinality wrapper around this type with its default settings.
//
// Everything of fixed size up to eight bytes — every number, Date,
// Date32, DateTime, DateTime64, Bool, UUID — needs
// allow_suspicious_low_cardinality_types, because the dictionary costs
// more than the values it indexes. Of the ordinary scalar types that
// leaves the variable-width ones, which here means String; a
// FixedString is variable enough in principle but drops does not claim
// it without a server to ask.
//
// base is expected without a Nullable wrapper: LowCardinality(
// Nullable(String)) is accepted, and it is the String that decides it.
func LowCardinalitySafe(base string) bool {
	return NormalizeType(base) == "String"
}

// StripLowCardinality removes one LowCardinality wrapper, or returns
// the type unchanged when it has none.
func StripLowCardinality(t string) string {
	if inner, ok := unwrapType(t, "LowCardinality"); ok {
		return inner
	}
	return t
}

// SplitNullable splits Nullable(T) into T and true, and anything else
// into itself and false.
func SplitNullable(t string) (base string, nullable bool) {
	if inner, ok := unwrapType(t, "Nullable"); ok {
		return inner, true
	}
	return t, false
}

// unwrapType returns the inside of "Wrapper(inner)". It runs against a
// normalised type, where the wrapper is the outermost token and there
// is no whitespace to skip.
func unwrapType(t, wrapper string) (string, bool) {
	t = NormalizeType(t)
	if !strings.HasPrefix(t, wrapper+"(") || !strings.HasSuffix(t, ")") {
		return "", false
	}
	return t[len(wrapper)+1 : len(t)-1], true
}

// Widens reports whether every value of type from is also a value of
// type to, which is the condition under which a MODIFY COLUMN can
// neither lose a value nor fail on one.
//
// The rule is deliberately narrow. Only same-family numeric widening
// and Decimal precision growth qualify; Int64 to Float64 does not,
// because floats above 2^53 stop being exact, and UInt32 to Int64 does
// not — not because it is unsound, but because the pair that looks
// like it, UInt64 to Int64, wraps at the top of the range, and a rule
// that has to reason about pairs is a rule that will one day get one
// wrong.
func Widens(from, to string) bool {
	from, to = NormalizeType(from), NormalizeType(to)
	if f, ok := numericType(from); ok {
		t, ok := numericType(to)
		return ok && f.family == t.family && t.bits > f.bits
	}
	fp, fs, ok := decimalType(from)
	if !ok {
		return false
	}
	tp, ts, ok := decimalType(to)
	return ok && ts == fs && tp > fp
}

// numeric is a ClickHouse fixed-width number.
type numeric struct {
	family string
	bits   int
}

// numericType parses "Int32", "UInt8", "Float64" and friends.
func numericType(t string) (numeric, bool) {
	// UInt before Int: "UInt8" carries both prefixes and only the
	// first is the family.
	for _, family := range []string{"UInt", "Int", "Float"} {
		if !strings.HasPrefix(t, family) {
			continue
		}
		bits, err := strconv.Atoi(t[len(family):])
		if err != nil {
			return numeric{}, false
		}
		return numeric{family: family, bits: bits}, true
	}
	return numeric{}, false
}

// decimalType parses "Decimal(p,s)".
func decimalType(t string) (precision, scale int, ok bool) {
	args, found := unwrapType(t, "Decimal")
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
