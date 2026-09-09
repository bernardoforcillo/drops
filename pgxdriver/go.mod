// The pgx driver is a module of its own, for the same reason the CLI
// and the integration suite are.
//
// drops itself has no dependencies, and CI asserts it by diffing the
// tree after `go mod tidy`. A driver has to have one — it speaks a wire
// protocol — so importing pgx from the root module would put pgx in the
// build of every user of drops, including the ones on MySQL, SQLite or
// ClickHouse who will never open a PostgreSQL connection.
//
// Here it costs only the people who ask for it, and asking is one line:
//
//	go get github.com/bernardoforcillo/drops/pgxdriver
module github.com/bernardoforcillo/drops/pgxdriver

go 1.25.0

require (
	github.com/bernardoforcillo/drops v0.0.0
	github.com/jackc/pgx/v5 v5.10.0
)

require (
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/jackc/puddle/v2 v2.2.2 // indirect
	golang.org/x/sync v0.17.0 // indirect
	golang.org/x/text v0.29.0 // indirect
)

replace github.com/bernardoforcillo/drops => ..
