package pg

import (
	"errors"
	"strings"
)

// The SQLSTATE table, and the errors that change a decision.
//
// pg/errors.go classifies the eight codes an application handles by
// hand — the constraint violations, the two rollbacks, the two
// "undefined" ones. This file covers the rest: the codes an
// *operator* handles, which is a different list and a longer one.
//
// The distinction is worth making because these codes do not tell the
// caller to show a nicer message. They tell the caller to do
// something else entirely:
//
//   - [ErrReadOnlyTransaction] means the statement reached a standby
//     or a demoted primary. Retrying it there fails forever; the fix
//     is to route it somewhere else. [Replicated] does exactly that.
//   - [ErrCompletionUnknown] means the write may or may not have
//     landed. It is the one rollback code that is *not* safe to retry,
//     and it sits one digit away from two that are.
//   - [ErrFeatureNotSupported] carries the cached-plan failure that
//     every migration causes and nobody expects — see
//     [IsCachedPlanChanged].
//   - [ErrTooManyConnections], [ErrDiskFull], [ErrAdminShutdown] are
//     not bugs in the query at all. A retry loop that treats them as
//     transient turns a capacity problem into an outage.
//
// [Condition] and [ClassName] give the canonical PostgreSQL names for
// a code, so a log line can say "read_only_sql_transaction" instead
// of "25006". The table is deliberately partial rather than invented:
// it covers the codes PostgreSQL actually raises in the paths a
// toolkit drives, and a code it does not know returns "" rather than
// a guess.

// Operator-facing SQLSTATE sentinels. Each is attached to a
// [PgError] by the same classifier that attaches the constraint ones,
// so errors.Is reaches them identically:
//
//	if errors.Is(err, pg.ErrTooManyConnections) {
//	    // shed load; do not retry
//	}
var (
	// ErrReadOnlyTransaction — SQLSTATE 25006. The statement tried
	// to write inside a read-only transaction, which in practice
	// means it reached a hot standby or a primary that has been
	// demoted under it. The remedy is routing, not retrying:
	// [Replicated] treats this as proof that a node it believed
	// writable is not, and sends the statement to the primary.
	ErrReadOnlyTransaction = errors.New("drops/pg: read-only transaction")

	// ErrCompletionUnknown — SQLSTATE 40003. The server lost the
	// connection at a moment where it cannot say whether the
	// statement committed.
	//
	// It is in class 40 with [ErrSerializationFailure] (40001) and
	// [ErrDeadlock] (40P01), and it is the one that must not join
	// them in a [RetryPolicy]: those two are guaranteed rolled back,
	// this one is guaranteed unknown. Retrying a transfer that may
	// already have moved the money is worse than failing it.
	// [RetryableErrors] leaves it out for that reason.
	ErrCompletionUnknown = errors.New("drops/pg: transaction completion unknown")

	// ErrFeatureNotSupported — SQLSTATE 0A000. Mostly what it says,
	// but it is also the code PostgreSQL uses for "cached plan must
	// not change result type", which is a migration artefact rather
	// than a missing feature. Use [IsCachedPlanChanged] to tell them
	// apart.
	ErrFeatureNotSupported = errors.New("drops/pg: feature not supported")

	// ErrQueryCanceled — SQLSTATE 57014. statement_timeout elapsed,
	// or somebody called pg_cancel_backend. A [StatementRegistry]
	// draining for failover produces this on purpose, which is why
	// it is worth being able to recognise.
	ErrQueryCanceled = errors.New("drops/pg: query canceled")

	// ErrAdminShutdown — SQLSTATE 57P01. The server is going away
	// (or the DBA terminated this backend). Reconnecting is right;
	// retrying on the same connection is not.
	ErrAdminShutdown = errors.New("drops/pg: server shutting down")

	// ErrCannotConnectNow — SQLSTATE 57P03. The server is still
	// starting up or is in recovery. Unlike most failures this one
	// really does clear on its own, so it is the rare case where
	// backing off and trying again is the whole remedy.
	ErrCannotConnectNow = errors.New("drops/pg: server not accepting connections")

	// ErrIdleInTransactionTimeout — SQLSTATE 25P03. The transaction
	// sat idle past idle_in_transaction_session_timeout and was
	// killed. It names a bug in the caller — a transaction held open
	// across an HTTP call or a lock acquisition — not a transient.
	ErrIdleInTransactionTimeout = errors.New("drops/pg: idle in transaction timeout")

	// ErrInFailedTransaction — SQLSTATE 25P02. A statement was
	// issued after an earlier one in the same transaction failed.
	// Every statement from here to the rollback returns this, so it
	// is the code that hides the real error: the first failure is
	// the one to look at.
	ErrInFailedTransaction = errors.New("drops/pg: current transaction is aborted")

	// ErrTooManyConnections — SQLSTATE 53300. The pool is asking for
	// more backends than max_connections allows. Retrying makes it
	// worse; the answer is a smaller pool ceiling or a pooler.
	ErrTooManyConnections = errors.New("drops/pg: too many connections")

	// ErrOutOfMemory — SQLSTATE 53200.
	ErrOutOfMemory = errors.New("drops/pg: out of memory")

	// ErrDiskFull — SQLSTATE 53100. Often the downstream symptom of
	// an abandoned replication slot pinning WAL, which is why
	// [SlotLag] and [DropInactiveSlots] exist.
	ErrDiskFull = errors.New("drops/pg: disk full")

	// ErrConfigurationLimitExceeded — SQLSTATE 53400.
	ErrConfigurationLimitExceeded = errors.New("drops/pg: configuration limit exceeded")

	// ErrLockNotAvailable — SQLSTATE 55P03. A NOWAIT lock request
	// found the row or table already locked. This is a *result*, not
	// a failure: SELECT ... FOR UPDATE NOWAIT and LOCK ... NOWAIT
	// report contention this way, and a queue worker is expected to
	// move on to the next row.
	ErrLockNotAvailable = errors.New("drops/pg: lock not available")

	// ErrObjectInUse — SQLSTATE 55006. Typically a DROP or ALTER
	// blocked by an open session holding the object.
	ErrObjectInUse = errors.New("drops/pg: object in use")

	// ErrObjectNotInPrerequisiteState — SQLSTATE 55000. The catch-all
	// of its class; replication-slot operations raise it when a slot
	// is active elsewhere.
	ErrObjectNotInPrerequisiteState = errors.New("drops/pg: object not in prerequisite state")

	// ErrExclusionViolation — SQLSTATE 23P01. An EXCLUDE constraint
	// refused the row — the code behind "this room is already booked
	// for that interval". It belongs with the other integrity
	// violations in pg/errors.go and is only here because that file
	// predates ranges.
	ErrExclusionViolation = errors.New("drops/pg: exclusion constraint violation")

	// ErrRestrictViolation — SQLSTATE 23001.
	ErrRestrictViolation = errors.New("drops/pg: restrict violation")

	// ErrInsufficientPrivilege — SQLSTATE 42501. The role may not
	// touch the object. In a migration this usually means the
	// migration ran as the application role instead of the owner.
	ErrInsufficientPrivilege = errors.New("drops/pg: insufficient privilege")

	// ErrSyntaxError — SQLSTATE 42601.
	ErrSyntaxError = errors.New("drops/pg: syntax error")

	// ErrUndefinedObject — SQLSTATE 42704. An unknown type, enum
	// label, index, collation or extension. [Push] surfaces it when
	// a migration references something an earlier step should have
	// created.
	ErrUndefinedObject = errors.New("drops/pg: undefined object")

	// ErrUndefinedFunction — SQLSTATE 42883. Also what PostgreSQL
	// says when no *overload* matches, which is how a wrong argument
	// type usually presents.
	ErrUndefinedFunction = errors.New("drops/pg: undefined function")

	// ErrDuplicateTable — SQLSTATE 42P07. CREATE TABLE over one that
	// exists; the code that makes a re-run of a half-applied
	// migration recognisable.
	ErrDuplicateTable = errors.New("drops/pg: duplicate table")

	// ErrDuplicateColumn — SQLSTATE 42701.
	ErrDuplicateColumn = errors.New("drops/pg: duplicate column")

	// ErrDuplicateObject — SQLSTATE 42710. Raised by
	// pg_create_logical_replication_slot when the slot already
	// exists, which is what makes [EnsureSlot] idempotent.
	ErrDuplicateObject = errors.New("drops/pg: duplicate object")

	// ErrDependentObjectsStillExist — SQLSTATE 2BP01. A DROP without
	// CASCADE that something still references.
	ErrDependentObjectsStillExist = errors.New("drops/pg: dependent objects still exist")

	// ErrDatatypeMismatch — SQLSTATE 42804.
	ErrDatatypeMismatch = errors.New("drops/pg: datatype mismatch")

	// ErrInvalidTextRepresentation — SQLSTATE 22P02. A string would
	// not parse as the target type — the classic "invalid input
	// syntax for type uuid".
	ErrInvalidTextRepresentation = errors.New("drops/pg: invalid text representation")

	// ErrStringTooLong — SQLSTATE 22001. A value overflowed a
	// varchar(n). Worth its own sentinel because the remedy is a
	// migration, not a retry.
	ErrStringTooLong = errors.New("drops/pg: string data right truncation")

	// ErrNumericOutOfRange — SQLSTATE 22003.
	ErrNumericOutOfRange = errors.New("drops/pg: numeric value out of range")

	// ErrDivisionByZero — SQLSTATE 22012.
	ErrDivisionByZero = errors.New("drops/pg: division by zero")

	// ErrProgramLimitExceeded — SQLSTATE 54000. The one a query
	// builder can cause on its own: too many parameters, a target
	// list too wide, an expression nested too deep. Generated SQL
	// reaches these limits and hand-written SQL does not, so a
	// toolkit owes it a name.
	ErrProgramLimitExceeded = errors.New("drops/pg: program limit exceeded")

	// ErrConnectionFailure — SQLSTATE class 08. The connection broke.
	// Every code in the class maps here: for a caller the difference
	// between "never established" and "died mid-statement" is not
	// actionable, and the one that is — did my write land? — is
	// 40003, which is [ErrCompletionUnknown] and deliberately
	// separate.
	ErrConnectionFailure = errors.New("drops/pg: connection failure")

	// ErrInvalidPassword — SQLSTATE 28P01.
	ErrInvalidPassword = errors.New("drops/pg: invalid password")

	// ErrInvalidCatalogName — SQLSTATE 3D000. The database does not
	// exist. In a DSN typo this is the error; in a multi-tenant
	// setup it means the tenant was never provisioned.
	ErrInvalidCatalogName = errors.New("drops/pg: database does not exist")

	// ErrInvalidSchemaName — SQLSTATE 3F000. See also
	// [ErrInvalidCatalogName]; with [WithTenant] a missing schema is
	// the same class of mistake one level down.
	ErrInvalidSchemaName = errors.New("drops/pg: schema does not exist")

	// ErrRaiseException — SQLSTATE P0001. A PL/pgSQL RAISE without
	// an explicit code. Application invariants enforced in triggers
	// arrive here, so it is the sentinel a caller checks to turn a
	// trigger's message into a domain error.
	ErrRaiseException = errors.New("drops/pg: raised exception")

	// ErrInternalError — SQLSTATE XX000, and its data_corrupted /
	// index_corrupted neighbours. Never the caller's fault and never
	// retryable.
	ErrInternalError = errors.New("drops/pg: internal error")
)

// codeSentinels maps a SQLSTATE to the sentinel [classifyError]
// attaches. Codes classified in pg/errors.go are repeated here so the
// two paths cannot drift; the switch there consults this table.
var codeSentinels = map[string]error{
	// 0A — feature not supported
	"0A000": ErrFeatureNotSupported,

	// 22 — data exception
	"22001": ErrStringTooLong,
	"22003": ErrNumericOutOfRange,
	"22012": ErrDivisionByZero,
	"22P02": ErrInvalidTextRepresentation,

	// 23 — integrity constraint violation
	"23001": ErrRestrictViolation,
	"23502": ErrNotNullViolation,
	"23503": ErrForeignKeyViolation,
	"23505": ErrUniqueViolation,
	"23514": ErrCheckViolation,
	"23P01": ErrExclusionViolation,

	// 25 — invalid transaction state
	"25006": ErrReadOnlyTransaction,
	"25P02": ErrInFailedTransaction,
	"25P03": ErrIdleInTransactionTimeout,

	// 28 — invalid authorization specification
	"28P01": ErrInvalidPassword,

	// 2B — dependent objects
	"2BP01": ErrDependentObjectsStillExist,

	// 3D / 3F — missing database / schema
	"3D000": ErrInvalidCatalogName,
	"3F000": ErrInvalidSchemaName,

	// 40 — transaction rollback
	"40001": ErrSerializationFailure,
	"40003": ErrCompletionUnknown,
	"40P01": ErrDeadlock,

	// 42 — syntax error or access rule violation
	"42501": ErrInsufficientPrivilege,
	"42601": ErrSyntaxError,
	"42701": ErrDuplicateColumn,
	"42703": ErrUndefinedColumn,
	"42704": ErrUndefinedObject,
	"42710": ErrDuplicateObject,
	"42804": ErrDatatypeMismatch,
	"42883": ErrUndefinedFunction,
	"42P01": ErrUndefinedTable,
	"42P07": ErrDuplicateTable,

	// 53 — insufficient resources
	"53100": ErrDiskFull,
	"53200": ErrOutOfMemory,
	"53300": ErrTooManyConnections,
	"53400": ErrConfigurationLimitExceeded,

	// 54 — program limit exceeded
	"54000": ErrProgramLimitExceeded,
	"54001": ErrProgramLimitExceeded,
	"54011": ErrProgramLimitExceeded,
	"54023": ErrProgramLimitExceeded,

	// 55 — object not in prerequisite state
	"55000": ErrObjectNotInPrerequisiteState,
	"55006": ErrObjectInUse,
	"55P03": ErrLockNotAvailable,

	// 57 — operator intervention
	"57014": ErrQueryCanceled,
	"57P01": ErrAdminShutdown,
	"57P02": ErrAdminShutdown,
	"57P03": ErrCannotConnectNow,

	// P0 — PL/pgSQL
	"P0001": ErrRaiseException,

	// XX — internal error
	"XX000": ErrInternalError,
	"XX001": ErrInternalError,
	"XX002": ErrInternalError,
}

// sentinelFor returns the sentinel for a SQLSTATE, or nil when the
// code is recognised by name but not worth branching on.
//
// Class 08 is handled as a class rather than per-code: every member
// means the connection broke, and no caller acts differently on
// which member it was.
func sentinelFor(code string) error {
	if s, ok := codeSentinels[code]; ok {
		return s
	}
	if strings.HasPrefix(code, "08") {
		return ErrConnectionFailure
	}
	return nil
}

// classNames maps a two-character SQLSTATE class to its canonical
// PostgreSQL name.
var classNames = map[string]string{
	"00": "successful_completion",
	"01": "warning",
	"02": "no_data",
	"03": "sql_statement_not_yet_complete",
	"08": "connection_exception",
	"09": "triggered_action_exception",
	"0A": "feature_not_supported",
	"0B": "invalid_transaction_initiation",
	"0F": "locator_exception",
	"0L": "invalid_grantor",
	"0P": "invalid_role_specification",
	"0Z": "diagnostics_exception",
	"20": "case_not_found",
	"21": "cardinality_violation",
	"22": "data_exception",
	"23": "integrity_constraint_violation",
	"24": "invalid_cursor_state",
	"25": "invalid_transaction_state",
	"26": "invalid_sql_statement_name",
	"27": "triggered_data_change_violation",
	"28": "invalid_authorization_specification",
	"2B": "dependent_privilege_descriptors_still_exist",
	"2D": "invalid_transaction_termination",
	"2F": "sql_routine_exception",
	"34": "invalid_cursor_name",
	"38": "external_routine_exception",
	"39": "external_routine_invocation_exception",
	"3B": "savepoint_exception",
	"3D": "invalid_catalog_name",
	"3F": "invalid_schema_name",
	"40": "transaction_rollback",
	"42": "syntax_error_or_access_rule_violation",
	"44": "with_check_option_violation",
	"53": "insufficient_resources",
	"54": "program_limit_exceeded",
	"55": "object_not_in_prerequisite_state",
	"57": "operator_intervention",
	"58": "system_error",
	"72": "snapshot_failure",
	"F0": "config_file_error",
	"HV": "foreign_data_wrapper_error",
	"P0": "plpgsql_error",
	"XX": "internal_error",
}

// conditionNames maps a SQLSTATE to its canonical PostgreSQL
// condition name — the spelling used by PL/pgSQL's EXCEPTION WHEN and
// by the server's own errcodes table.
//
// The table covers the codes a toolkit's own paths can raise and the
// ones an operator reads in an incident. It is not the complete
// errcodes list, and [Condition] returns "" for a code that is not in
// it rather than a plausible-looking guess: a wrong condition name in
// a log line is harder to catch than a bare code.
var conditionNames = map[string]string{
	"08000": "connection_exception",
	"08001": "sqlclient_unable_to_establish_sqlconnection",
	"08003": "connection_does_not_exist",
	"08004": "sqlserver_rejected_establishment_of_sqlconnection",
	"08006": "connection_failure",
	"08007": "transaction_resolution_unknown",
	"08P01": "protocol_violation",
	"0A000": "feature_not_supported",
	"21000": "cardinality_violation",
	"22000": "data_exception",
	"22001": "string_data_right_truncation",
	"22003": "numeric_value_out_of_range",
	"22004": "null_value_not_allowed",
	"22007": "invalid_datetime_format",
	"22008": "datetime_field_overflow",
	"22012": "division_by_zero",
	"22023": "invalid_parameter_value",
	"2201W": "invalid_row_count_in_limit_clause",
	"2201X": "invalid_row_count_in_result_offset_clause",
	"22P02": "invalid_text_representation",
	"22P03": "invalid_binary_representation",
	"22P04": "bad_copy_file_format",
	"23000": "integrity_constraint_violation",
	"23001": "restrict_violation",
	"23502": "not_null_violation",
	"23503": "foreign_key_violation",
	"23505": "unique_violation",
	"23514": "check_violation",
	"23P01": "exclusion_violation",
	"24000": "invalid_cursor_state",
	"25000": "invalid_transaction_state",
	"25001": "active_sql_transaction",
	"25006": "read_only_sql_transaction",
	"25P01": "no_active_sql_transaction",
	"25P02": "in_failed_sql_transaction",
	"25P03": "idle_in_transaction_session_timeout",
	"28000": "invalid_authorization_specification",
	"28P01": "invalid_password",
	"2BP01": "dependent_objects_still_exist",
	"3D000": "invalid_catalog_name",
	"3F000": "invalid_schema_name",
	"40000": "transaction_rollback",
	"40001": "serialization_failure",
	"40002": "transaction_integrity_constraint_violation",
	"40003": "statement_completion_unknown",
	"40P01": "deadlock_detected",
	"42000": "syntax_error_or_access_rule_violation",
	"42501": "insufficient_privilege",
	"42601": "syntax_error",
	"42602": "invalid_name",
	"42611": "invalid_column_definition",
	"42622": "name_too_long",
	"42701": "duplicate_column",
	"42702": "ambiguous_column",
	"42703": "undefined_column",
	"42704": "undefined_object",
	"42710": "duplicate_object",
	"42712": "duplicate_alias",
	"42723": "duplicate_function",
	"42803": "grouping_error",
	"42804": "datatype_mismatch",
	"42809": "wrong_object_type",
	"42830": "invalid_foreign_key",
	"42846": "cannot_coerce",
	"42883": "undefined_function",
	"42939": "reserved_name",
	"42P01": "undefined_table",
	"42P02": "undefined_parameter",
	"42P04": "duplicate_database",
	"42P06": "duplicate_schema",
	"42P07": "duplicate_table",
	"42P10": "invalid_column_reference",
	"42P16": "invalid_table_definition",
	"42P17": "invalid_object_definition",
	"42P18": "indeterminate_datatype",
	"44000": "with_check_option_violation",
	"53000": "insufficient_resources",
	"53100": "disk_full",
	"53200": "out_of_memory",
	"53300": "too_many_connections",
	"53400": "configuration_limit_exceeded",
	"54000": "program_limit_exceeded",
	"54001": "statement_too_complex",
	"54011": "too_many_columns",
	"54023": "too_many_arguments",
	"55000": "object_not_in_prerequisite_state",
	"55006": "object_in_use",
	"55P02": "cant_change_runtime_param",
	"55P03": "lock_not_available",
	"55P04": "unsafe_new_enum_value_usage",
	"57000": "operator_intervention",
	"57014": "query_canceled",
	"57P01": "admin_shutdown",
	"57P02": "crash_shutdown",
	"57P03": "cannot_connect_now",
	"57P04": "database_dropped",
	"57P05": "idle_session_timeout",
	"58000": "system_error",
	"58030": "io_error",
	"58P01": "undefined_file",
	"58P02": "duplicate_file",
	"72000": "snapshot_too_old",
	"P0001": "raise_exception",
	"P0002": "no_data_found",
	"P0003": "too_many_rows",
	"P0004": "assert_failure",
	"XX000": "internal_error",
	"XX001": "data_corrupted",
	"XX002": "index_corrupted",
}

// Condition returns the canonical PostgreSQL condition name for a
// SQLSTATE — "unique_violation" for "23505" — or "" when the code is
// not in the table.
//
// It is what turns an alert from "SQLSTATE 55P03 on the payments
// worker" into "lock_not_available on the payments worker", which is
// the difference between a search and a diagnosis.
func Condition(code string) string { return conditionNames[code] }

// ClassName returns the canonical name of a SQLSTATE's two-character
// class — "insufficient_resources" for anything starting "53" — or ""
// when the class is unknown. Every code belongs to a class even when
// [Condition] does not know the code itself, so this is the coarse
// answer that is almost always available.
func ClassName(code string) string {
	if len(code) < 2 {
		return ""
	}
	return classNames[code[:2]]
}

// Condition returns the condition name for the error's SQLSTATE, or
// "" when the code is unrecognised.
func (e *PgError) Condition() string { return Condition(e.Code) }

// Class returns the two-character SQLSTATE class, e.g. "23" for
// "23505". Empty when no SQLSTATE was reported.
func (e *PgError) Class() string {
	if len(e.Code) < 2 {
		return ""
	}
	return e.Code[:2]
}

// ClassName returns the canonical name of the error's SQLSTATE class.
func (e *PgError) ClassName() string { return ClassName(e.Code) }

// RetryableErrors returns the errors it is *safe* to retry a whole
// transaction on — the ones PostgreSQL guarantees left no trace.
//
//	db := pg.New(drv).WithRetry(pg.RetryPolicy{
//	    MaxAttempts: 3,
//	    Errors:      pg.RetryableErrors(),
//	    Backoff:     pg.ExponentialJitter(10*time.Millisecond, time.Second),
//	})
//
// The list is short on purpose, and what it leaves out is the point:
//
//   - [ErrCompletionUnknown] (40003) — the write may have committed.
//   - [ErrConnectionFailure] (class 08) — same problem whenever the
//     break lands near the commit, and from the client the two are
//     indistinguishable.
//   - [ErrTooManyConnections], [ErrDiskFull], [ErrOutOfMemory] — real
//     and retrying is what turns them into an outage.
//   - [ErrLockNotAvailable] (55P03) — a NOWAIT answer, not a failure.
//     A worker that retries it spins on the lock it asked not to wait
//     for.
//
// A caller that knows its transaction is idempotent — no side effects
// outside the database, or an idempotency key covering them — can add
// [ErrCompletionUnknown] and [ErrConnectionFailure] itself. That is a
// property of the caller's transaction, which is why this function
// will not assume it.
func RetryableErrors() []error {
	return []error{ErrSerializationFailure, ErrDeadlock}
}

// IsCachedPlanChanged reports whether err is PostgreSQL refusing to
// reuse a prepared statement whose result type changed under it —
// SQLSTATE 0A000 with the message "cached plan must not change result
// type".
//
// It is the error a migration causes and nobody predicts. Adding a
// column to a table that a pooled connection has a prepared SELECT *
// against invalidates that connection's plan, and the *next* use of
// it fails — after the migration succeeded, in a different request,
// on a connection nobody touched. Every pooled connection carries its
// own plan cache, so the failures trickle in one connection at a
// time.
//
// The remedy is to discard the plans, which [DiscardPlans] does and
// [Push] does for you. This predicate exists for the window before
// that lands: it is safe to retry the statement once, because the
// failure is the invalidation, and the retry re-plans.
//
// The message is matched because 0A000 is a general code — most of
// its uses really are unsupported features, and those must not be
// retried.
func IsCachedPlanChanged(err error) bool {
	if !errors.Is(withSentinel(err), ErrFeatureNotSupported) {
		return false
	}
	return strings.Contains(err.Error(), "cached plan must not change result type")
}

// IsReadOnly reports whether err is the server refusing a write
// because the session is read-only (SQLSTATE 25006) — a standby, or a
// primary demoted under the connection.
//
// [Replicated] uses it to correct its own routing; a caller running
// against a single node uses it to notice a failover it was not told
// about.
func IsReadOnly(err error) bool { return errors.Is(withSentinel(err), ErrReadOnlyTransaction) }

// IsConnectionFailure reports whether err belongs to SQLSTATE class
// 08 — the connection broke, at any point and for any reason.
//
// It deliberately says nothing about whether an in-flight write
// landed. When that question matters, the answer is
// [ErrCompletionUnknown] or an idempotency key, never this.
func IsConnectionFailure(err error) bool { return errors.Is(withSentinel(err), ErrConnectionFailure) }

// withSentinel makes the predicates above work on a raw driver error
// as well as on one drops has already classified.
//
// It has to, because the two places that ask these questions ask them
// early. [Replicated] and [RetryCachedPlans] are drivers: they see
// what the connection returned, and *DB attaches the sentinel on the
// way out, after them. A predicate that only understood classified
// errors would silently answer false in exactly the two callers that
// need it.
//
// An error that already carries a *PgError is returned untouched, so
// the common path costs one errors.As and no allocation.
func withSentinel(err error) error {
	if err == nil {
		return nil
	}
	var pe *PgError
	if errors.As(err, &pe) {
		return err
	}
	return classifyError(err)
}
