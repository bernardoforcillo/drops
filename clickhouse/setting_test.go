package clickhouse_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/bernardoforcillo/drops"

	"github.com/bernardoforcillo/drops/clickhouse"
)

func settingTable() *clickhouse.Table {
	t := clickhouse.NewTable("settings_probe")
	id := clickhouse.Add(t, clickhouse.UInt64("id"))
	return t.Engine(clickhouse.MergeTree()).OrderBy(id)
}

func mustPanic(t *testing.T, what string, f func()) {
	t.Helper()
	defer func() {
		r := recover()
		if r == nil {
			t.Fatalf("%s was accepted", what)
		}
		if !strings.Contains(strings.ToLower(toString(r)), "setting") {
			t.Errorf("%s panicked, but not about the setting: %v", what, r)
		}
	}()
	f()
}

func toString(v any) string {
	if err, ok := v.(error); ok {
		return err.Error()
	}
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

// SETTINGS is a comma-separated list, so a value carrying a top-level
// comma is not one value — it is this setting and whatever the rest of
// the string says. MODIFY SETTING has the same shape.
func TestSettingRejectsAValueThatIsSeveralSettings(t *testing.T) {
	mustPanic(t, "a value with a top-level comma", func() {
		settingTable().Setting("index_granularity", "8192, allow_nullable_key = 1")
	})
	mustPanic(t, "a value that closes the list and starts a clause", func() {
		settingTable().Setting("index_granularity", "8192, storage_policy = 'other'")
	})
}

// A value that opens a string literal and never closes it swallows
// whatever the renderer writes after it.
func TestSettingRejectsAnUnterminatedLiteral(t *testing.T) {
	mustPanic(t, "an unterminated literal", func() {
		settingTable().Setting("storage_policy", "'default")
	})
}

func TestSettingRejectsAKeyThatIsNotAName(t *testing.T) {
	mustPanic(t, "a key with a comma", func() {
		settingTable().Setting("index_granularity = 1, allow_nullable_key", "1")
	})
	mustPanic(t, "an empty key", func() {
		settingTable().Setting("", "1")
	})
}

// The check is about the value being one value, not about its shape.
// A comma inside a literal or inside a function call is part of the
// value, and ClickHouse takes both.
func TestSettingAcceptsTheValuesClickHouseTakes(t *testing.T) {
	for _, c := range []struct{ key, value string }{
		{"index_granularity", "8192"},
		{"min_bytes_for_wide_part", "0"},
		{"storage_policy", "'default'"},
		{"storage_policy", "'tier_a,tier_b'"},
		{"merge_with_ttl_timeout", "-1"},
		{"disk", "disk(name = 'd', type = cache, max_size = '1G')"},
	} {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("%s = %s was rejected: %v", c.key, c.value, r)
				}
			}()
			settingTable().Setting(c.key, c.value)
		}()
	}
}

// Rendering is what the check protects, in both the clause that
// creates the table and the ALTER that changes it.
func TestAcceptedSettingsStillRender(t *testing.T) {
	tbl := settingTable().
		Setting("index_granularity", "8192").
		Setting("storage_policy", "'tier_a,tier_b'")
	sql, _ := clickhouse.ToSQL(clickhouse.CreateTable(tbl))
	if !strings.Contains(sql, "SETTINGS index_granularity = 8192, storage_policy = 'tier_a,tier_b'") {
		t.Errorf("settings clause:\n%s", sql)
	}
}

// A query's SETTINGS clause is the same comma-separated list, so it
// has the same hole and the same check.
func TestSelectSettingRejectsAValueThatIsSeveralSettings(t *testing.T) {
	tbl := settingTable()
	mustPanic(t, "a query setting with a top-level comma", func() {
		clickhouse.New(&settingDriver{}).Select(tbl.Col("id")).From(tbl).Setting("max_threads", "4, max_memory_usage = 1")
	})
	mustPanic(t, "a query setting whose key is not a name", func() {
		clickhouse.New(&settingDriver{}).Select(tbl.Col("id")).From(tbl).Setting("max_threads = 4, x", "1")
	})
}

type settingDriver struct{}

func (*settingDriver) Query(context.Context, string, ...any) (drops.Rows, error) {
	return nil, errors.New("not used")
}
func (*settingDriver) Exec(context.Context, string, ...any) (drops.Result, error) {
	return nil, errors.New("not used")
}
func (*settingDriver) Begin(context.Context) (drops.Tx, error) { return nil, errors.New("not used") }

// A comment opener ends the SETTINGS list without ending the
// statement, so it loses every setting written after it and the
// server reports nothing. That is the same failure the comma check
// exists for, and it arrives the same way — through a value read from
// configuration.
func TestSettingRejectsAValueThatOpensAComment(t *testing.T) {
	for _, v := range []string{"8192 --", "8192 /*", "8192 #", "8192 -- and the rest"} {
		mustPanic(t, "a value opening a comment ("+v+")", func() {
			settingTable().Setting("index_granularity", v)
		})
	}
}

// The markers are only markers outside a literal, and a lone dash or
// slash is arithmetic.
func TestSettingAcceptsValuesThatMerelyContainTheCharacters(t *testing.T) {
	for _, c := range []struct{ key, value string }{
		{"merge_with_ttl_timeout", "-1"},
		{"storage_policy", "'a--b'"},
		{"storage_policy", "'a/*b'"},
		{"storage_policy", "'tier#1'"},
	} {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("%s = %s was rejected: %v", c.key, c.value, r)
				}
			}()
			settingTable().Setting(c.key, c.value)
		}()
	}
}
