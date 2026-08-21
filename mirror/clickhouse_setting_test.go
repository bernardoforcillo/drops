package mirror_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/bernardoforcillo/drops/clickhouse"
	"github.com/bernardoforcillo/drops/mirror"
)

// WithSetting's arguments arrive from outside the program — that is
// the case the check exists for — and DeriveClickHouse reports
// everything else about its options as an error. A bad pair has to
// come back the same way rather than unwind the caller.
func TestDeriveReportsABadSettingAsAnError(t *testing.T) {
	for _, c := range []struct{ key, value string }{
		{"index_granularity", "8192, allow_nullable_key = 1"},
		{"index_granularity", "8192 --"},
		{"storage_policy", "'default"},
		{"index_granularity = 1, x", "8192"},
	} {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("WithSetting(%q, %q) panicked out of DeriveClickHouse: %v", c.key, c.value, r)
				}
			}()
			out, err := mirror.DeriveClickHouse(sourceTable(), mirror.WithSetting(c.key, c.value))
			if err == nil {
				t.Fatalf("WithSetting(%q, %q) was accepted; the derived table renders %s",
					c.key, c.value, ddl(t, out))
			}
			if !errors.Is(err, clickhouse.ErrInvalidSetting) {
				t.Errorf("err = %v, want one wrapping ErrInvalidSetting", err)
			}
			if !strings.Contains(err.Error(), "WithSetting") {
				t.Errorf("the error should name the option that carried it: %v", err)
			}
		}()
	}
}

// The pairs ClickHouse takes still reach the table.
func TestDeriveStillForwardsGoodSettings(t *testing.T) {
	out := derive(t, sourceTable(),
		mirror.WithSetting("index_granularity", "8192"),
		mirror.WithSetting("storage_policy", "'tier_a,tier_b'"))
	sql := ddl(t, out)
	if !strings.Contains(sql, "SETTINGS index_granularity = 8192, storage_policy = 'tier_a,tier_b'") {
		t.Errorf("settings clause:\n%s", sql)
	}
}
