package clickhouse

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// The guarantee renderedAtLeast has to keep is one-directional: its
// answer must never be larger than what clickhouse-go will actually
// render, because the estimate decides a REFUSAL. Under-estimating
// costs a statement that is sent and fails with the server's message,
// which is the failure that already existed. Over-estimating refuses a
// statement that would have worked, which is a new one drops would have
// invented.
//
// So this compares the floor against a rendering modelled on
// clickhouse-go's formatValue, for the shapes drops names and for the
// awkward values in each: the string that is all quotes, the time that
// renders shortest, the empty slice.
//
// It is not a copy of the driver kept in sync — it is the specific
// claim "no shorter than this", checked against what the driver does
// for values chosen to be as short as they can be. It earned its place
// immediately: the first version of renderedAtLeast answered 21 for a
// time.Time, reasoning from a datetime literal, and clickhouse-go
// renders the Unix epoch as toDateTime(0), which is 13.

// renderLikeDriver mirrors the parts of clickhouse-go's formatValue
// that drops estimates. Where the driver's answer depends on the
// connection — the timezone a time is rendered in, the scale — this
// takes the SHORTEST branch, since that is the one the floor must sit
// under.
func renderLikeDriver(v any) string {
	quote := func(s string) string {
		return "'" + strings.NewReplacer(`\`, `\\`, `'`, `\'`).Replace(s) + "'"
	}
	switch v := v.(type) {
	case nil:
		return "NULL"
	case string:
		return quote(v)
	case time.Time:
		// The shortest branch the driver has: a Local-zone time at the
		// Unix epoch.
		if v.Unix() == 0 {
			return "toDateTime(0)"
		}
		return fmt.Sprintf("toDateTime('%d')", v.Unix())
	case bool:
		if v {
			return "1"
		}
		return "0"
	case []byte:
		// Not a String on this path: a byte slice reaches the driver's
		// reflect.Slice branch and renders as a list of numbers.
		parts := make([]string, 0, len(v))
		for _, b := range v {
			parts = append(parts, fmt.Sprint(b))
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case []string:
		parts := make([]string, 0, len(v))
		for _, s := range v {
			parts = append(parts, quote(s))
		}
		return "[" + strings.Join(parts, ", ") + "]"
	default:
		return fmt.Sprint(v)
	}
}

func TestTheSizeFloorIsNeverAboveWhatTheDriverRenders(t *testing.T) {
	epoch := time.Unix(0, 0)
	cases := []any{
		nil,
		"",
		"ada@example.com",
		"'''''''", // every character needs escaping, so the driver's is longest
		strings.Repeat("x", 4096),
		epoch,                    // the shortest time there is
		time.Unix(1735689600, 0), // an ordinary one
		true, false,
		[]byte(nil), []byte("a"), []byte("abc"),
		[]string(nil), []string{"a"}, []string{"a", "b", "c"},
		int64(0), int64(-9223372036854775808), uint8(7),
		float64(0), float32(1.5),
	}
	for _, c := range cases {
		floor := renderedAtLeast(c)
		actual := len(renderLikeDriver(c))
		if floor > actual {
			t.Errorf("renderedAtLeast(%#v) = %d, but the driver renders %q (%d bytes): "+
				"the floor is above the real size, so it would refuse a statement that fits",
				c, floor, renderLikeDriver(c), actual)
		}
	}
}

// The whole-statement estimate has the same direction, and one extra
// thing to get right: each argument replaces its "?", so the byte the
// placeholder occupied must not be counted twice.
func TestTheStatementFloorAccountsForThePlaceholdersItReplaces(t *testing.T) {
	sql := "SELECT * FROM t WHERE a = ? AND b = ? AND c = ?"
	args := []any{"x", "y", "z"}

	rendered := sql
	for _, a := range args {
		rendered = strings.Replace(rendered, "?", renderLikeDriver(a), 1)
	}
	floor := renderedSizeAtLeast(sql, args)
	if floor > len(rendered) {
		t.Errorf("renderedSizeAtLeast = %d, driver would send %d bytes (%q)", floor, len(rendered), rendered)
	}
	// And it must be worth something: a floor that answered zero would
	// pass the line above and catch nothing.
	if floor < len(sql) {
		t.Errorf("renderedSizeAtLeast = %d is below the query text itself (%d bytes)", floor, len(sql))
	}
}

// A statement with no arguments is measured exactly, because there is
// nothing to substitute — the floor and the truth are the same string.
func TestAStatementWithNoArgumentsIsMeasuredExactly(t *testing.T) {
	sql := "OPTIMIZE TABLE " + strings.Repeat("x", 1000) + " FINAL"
	if got, want := renderedSizeAtLeast(sql, nil), len(sql); got != want {
		t.Errorf("renderedSizeAtLeast = %d, want %d", got, want)
	}
	if err := checkQuerySize(sql, nil); err != nil {
		t.Errorf("a statement well under the ceiling was refused: %v", err)
	}
	big := "SELECT " + strings.Repeat("x", maxQuerySize)
	if err := checkQuerySize(big, nil); err == nil {
		t.Error("a statement over the ceiling was not refused")
	}
}
