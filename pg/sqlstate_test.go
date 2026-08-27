package pg_test

import (
	"context"
	"errors"
	"testing"

	"github.com/bernardoforcillo/drops/pg"
)

// classified runs a statement against a driver that fails with the
// given SQLSTATE, which is the only way in: classification happens on
// the way out of Exec, so a test that called it directly would not be
// testing the path callers take.
func classified(t *testing.T, code, msg string) error {
	t.Helper()
	db := pg.New(&errDriver{err: &sqlStateOnlyError{code: code, msg: msg}})
	_, err := db.Exec(context.Background(), "SELECT 1")
	if err == nil {
		t.Fatalf("classify(%q) returned nil", code)
	}
	return err
}

func TestOperationalSentinels(t *testing.T) {
	cases := []struct {
		code string
		want error
	}{
		{"25006", pg.ErrReadOnlyTransaction},
		{"40003", pg.ErrCompletionUnknown},
		{"0A000", pg.ErrFeatureNotSupported},
		{"57014", pg.ErrQueryCanceled},
		{"57P01", pg.ErrAdminShutdown},
		{"57P02", pg.ErrAdminShutdown},
		{"57P03", pg.ErrCannotConnectNow},
		{"25P02", pg.ErrInFailedTransaction},
		{"25P03", pg.ErrIdleInTransactionTimeout},
		{"53100", pg.ErrDiskFull},
		{"53200", pg.ErrOutOfMemory},
		{"53300", pg.ErrTooManyConnections},
		{"54000", pg.ErrProgramLimitExceeded},
		{"55P03", pg.ErrLockNotAvailable},
		{"55006", pg.ErrObjectInUse},
		{"23P01", pg.ErrExclusionViolation},
		{"42501", pg.ErrInsufficientPrivilege},
		{"42P07", pg.ErrDuplicateTable},
		{"42710", pg.ErrDuplicateObject},
		{"2BP01", pg.ErrDependentObjectsStillExist},
		{"22001", pg.ErrStringTooLong},
		{"22P02", pg.ErrInvalidTextRepresentation},
		{"3D000", pg.ErrInvalidCatalogName},
		{"3F000", pg.ErrInvalidSchemaName},
		{"P0001", pg.ErrRaiseException},
		{"XX001", pg.ErrInternalError},
		// The eight that predate this table must still classify.
		{"23505", pg.ErrUniqueViolation},
		{"40001", pg.ErrSerializationFailure},
		{"40P01", pg.ErrDeadlock},
		{"42P01", pg.ErrUndefinedTable},
	}
	for _, tc := range cases {
		err := classified(t, tc.code, "boom")
		if !errors.Is(err, tc.want) {
			t.Errorf("%s: errors.Is(%v) = false, want true", tc.code, tc.want)
		}
	}
}

// Every code in class 08 collapses to one sentinel, including codes
// the table does not list individually.
func TestConnectionFailureIsClassWide(t *testing.T) {
	for _, code := range []string{"08000", "08001", "08003", "08004", "08006", "08007", "08P01"} {
		if err := classified(t, code, "gone"); !pg.IsConnectionFailure(err) {
			t.Errorf("%s: IsConnectionFailure = false, want true", code)
		}
	}
	// 40003 is the ambiguity that class 08 must not absorb.
	if pg.IsConnectionFailure(classified(t, "40003", "unknown")) {
		t.Error("40003 classified as a connection failure; it is ErrCompletionUnknown")
	}
}

// The whole point of separating 40003 from its class-40 neighbours.
func TestCompletionUnknownIsNotRetryable(t *testing.T) {
	safe := pg.RetryableErrors()
	unknown := classified(t, "40003", "connection lost during commit")
	for _, s := range safe {
		if errors.Is(unknown, s) {
			t.Fatalf("ErrCompletionUnknown matches retryable sentinel %v", s)
		}
	}
	// And the two that are safe must be in the list.
	for _, code := range []string{"40001", "40P01"} {
		err := classified(t, code, "retry me")
		var matched bool
		for _, s := range safe {
			if errors.Is(err, s) {
				matched = true
			}
		}
		if !matched {
			t.Errorf("%s is not in RetryableErrors()", code)
		}
	}
	// A NOWAIT answer is not a failure to retry either.
	lock := classified(t, "55P03", "could not obtain lock")
	for _, s := range safe {
		if errors.Is(lock, s) {
			t.Fatalf("ErrLockNotAvailable matches retryable sentinel %v", s)
		}
	}
}

func TestIsCachedPlanChanged(t *testing.T) {
	changed := classified(t, "0A000", `ERROR: cached plan must not change result type`)
	if !pg.IsCachedPlanChanged(changed) {
		t.Error("cached-plan error not recognised")
	}
	// 0A000 is a general code; an actual unsupported feature must not
	// be mistaken for a plan invalidation and retried.
	other := classified(t, "0A000", `ERROR: DEFERRABLE is not supported here`)
	if pg.IsCachedPlanChanged(other) {
		t.Error("unrelated 0A000 reported as a cached-plan change")
	}
	if pg.IsCachedPlanChanged(errors.New("plain error")) {
		t.Error("non-SQLSTATE error reported as a cached-plan change")
	}
}

func TestIsReadOnly(t *testing.T) {
	if !pg.IsReadOnly(classified(t, "25006", "cannot execute UPDATE in a read-only transaction")) {
		t.Error("25006 not recognised as read-only")
	}
	if pg.IsReadOnly(classified(t, "42601", "syntax error")) {
		t.Error("42601 reported as read-only")
	}
}

func TestConditionAndClassName(t *testing.T) {
	if got := pg.Condition("23505"); got != "unique_violation" {
		t.Errorf("Condition(23505) = %q", got)
	}
	if got := pg.Condition("55P03"); got != "lock_not_available" {
		t.Errorf("Condition(55P03) = %q", got)
	}
	// An unknown code returns nothing rather than a guess.
	if got := pg.Condition("99Z99"); got != "" {
		t.Errorf("Condition(99Z99) = %q, want empty", got)
	}
	if got := pg.ClassName("53100"); got != "insufficient_resources" {
		t.Errorf("ClassName(53100) = %q", got)
	}
	// The class is still available for a code the condition table
	// does not carry.
	if got := pg.ClassName("22P07"); got != "data_exception" {
		t.Errorf("ClassName(22P07) = %q", got)
	}
	if got := pg.ClassName("x"); got != "" {
		t.Errorf("ClassName(short) = %q, want empty", got)
	}
}

func TestPgErrorAccessors(t *testing.T) {
	err := classified(t, "55P03", "could not obtain lock on row")
	var pe *pg.PgError
	if !errors.As(err, &pe) {
		t.Fatal("not a *PgError")
	}
	if pe.Class() != "55" {
		t.Errorf("Class() = %q", pe.Class())
	}
	if pe.ClassName() != "object_not_in_prerequisite_state" {
		t.Errorf("ClassName() = %q", pe.ClassName())
	}
	if pe.Condition() != "lock_not_available" {
		t.Errorf("Condition() = %q", pe.Condition())
	}
}
