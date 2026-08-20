// Package integration runs drops against real database servers.
//
// Everything else in this repository tests the SQL that drops
// *generates*, by string-comparing it against what the test author
// expected. That has a blind spot with a track record: three separate
// bugs have shipped in which drops generated syntactically invalid SQL
// — a CREATE INDEX with table-qualified column names, a ClickHouse
// CREATE TABLE with qualified names in its sorting key, and PostGIS
// helpers that emitted every placeholder twice. All three passed their
// unit tests, because the expectation was written by the same reasoning
// that wrote the bug.
//
// The only test that cannot make that mistake is one where a server
// parses the statement. That is what lives here.
//
// # Separate module
//
// This directory is its own Go module. drops has no dependencies and
// the CI `go mod tidy` job asserts it; testing against real servers
// needs real drivers, so they live here where they cannot reach a
// user's build.
//
// # Running
//
// SQLite needs nothing — the driver is pure Go, so the suite runs
// against a real engine anywhere, including in a plain `go test`:
//
//	cd integration && go test ./...
//
// The server-backed engines run when a DSN is in the environment and
// skip with a clear message when it is not:
//
//	docker compose -f integration/docker-compose.yml up -d
//	cd integration && \
//	  DROPS_PG_DSN='postgres://drops:drops@localhost:5433/drops?sslmode=disable' \
//	  DROPS_MYSQL_DSN='drops:drops@tcp(localhost:3307)/drops?parseTime=true' \
//	  DROPS_CLICKHOUSE_DSN='clickhouse://localhost:9001/default' \
//	  DROPS_QDRANT_URL='http://localhost:6334' \
//	  go test ./...
//
// Skipping rather than failing is deliberate: a contributor without
// Docker should still be able to run the SQLite half and get real
// signal, and a suite that fails when a service is absent is a suite
// people stop running.
package integration
