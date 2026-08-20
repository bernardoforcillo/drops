package integration

import (
	"fmt"
	"os"
	"strconv"
	"testing"
)

// Environment variables naming each server. Absent means "skip"; see
// [DSN].
const (
	EnvPostgres   = "DROPS_PG_DSN"
	EnvMySQL      = "DROPS_MYSQL_DSN"
	EnvClickHouse = "DROPS_CLICKHOUSE_DSN"
	EnvQdrant     = "DROPS_QDRANT_URL"

	// EnvRequireAll turns a missing DSN from a skip into a failure.
	// CI sets it. A silently skipped integration job is worse than no
	// integration job, because it reports green.
	EnvRequireAll = "DROPS_REQUIRE_ALL"
)

// DSN returns the connection string for a server, skipping the test
// when it is not configured.
//
// Skipping is the right default locally: a contributor without Docker
// should still get real signal from the SQLite half rather than a wall
// of failures about services they never asked for. It is the wrong
// default in CI, which is what [EnvRequireAll] is for — there, a
// missing DSN means the job is not testing what it claims to.
func DSN(t *testing.T, env string) string {
	t.Helper()
	v := os.Getenv(env)
	if v != "" {
		return v
	}
	if RequireAll() {
		t.Fatalf("%s is unset, and %s is on — this run is supposed to cover every server", env, EnvRequireAll)
	}
	t.Skipf("%s unset; start the servers with docker compose -f integration/docker-compose.yml up -d", env)
	return ""
}

// RequireAll reports whether a missing server should fail rather than
// skip.
func RequireAll() bool {
	v, err := strconv.ParseBool(os.Getenv(EnvRequireAll))
	return err == nil && v
}

// UniqueName returns an identifier unique to a test, for the tables and
// collections it creates.
//
// Tests share one database, and Go runs them in one process in a
// defined order but a test that leaves a table behind still poisons a
// later one that assumes it is absent. Deriving the name from t.Name
// means a collision is impossible and a leftover table says which test
// left it.
func UniqueName(t *testing.T, prefix string) string {
	t.Helper()
	safe := make([]byte, 0, len(t.Name()))
	for i := 0; i < len(t.Name()); i++ {
		c := t.Name()[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
			safe = append(safe, c)
		default:
			safe = append(safe, '_')
		}
	}
	name := fmt.Sprintf("%s_%s", prefix, safe)
	// MySQL caps identifiers at 64 bytes and drops the excess with an
	// error, so trim from the front — the tail carries the subtest
	// name, which is the part that distinguishes.
	if len(name) > 60 {
		name = name[len(name)-60:]
	}
	return name
}
