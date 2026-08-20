// The CLI is a module of its own, for the same reason the integration
// suite is one: drops itself has no dependencies, and CI asserts it.
//
// A binary that migrates a database has to open a connection, and the
// honest way to open one is pgx. Keeping the CLI inside the root
// module would have put pgx in every user's build — so for a year it
// spoke the v3 wire protocol by hand instead, SCRAM and all. This
// module is the cheaper answer: pgx lives here, where nothing a user
// imports can reach it. `drops lint` added golang.org/x/tools on the
// same terms — a linter needs a go/analysis driver, and a driver is
// not something a library should make anyone download.
//
// Those two together make this module's build list wider than a bare
// drops+pgx one (x/tools raises golang.org/x/sync above what pgx asks
// for), which matters to integration/cli_test.go: the throwaway
// projects it builds borrow this go.sum, so they borrow these
// requirements too.
//
// The replace below builds the CLI against the drops in this
// checkout, so the binary the integration suite exercises is the one
// the library in the tree produces. It has to come out before the
// module is tagged: `go install pkg@version` refuses a go.mod that
// replaces anything. The release procedure is in docs/cli.md.
module github.com/bernardoforcillo/drops/cmd/drops

go 1.25.0

replace github.com/bernardoforcillo/drops => ../../

require (
	github.com/bernardoforcillo/drops v0.6.0
	github.com/jackc/pgx/v5 v5.10.0
	golang.org/x/tools v0.49.0
)

require (
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/jackc/puddle/v2 v2.2.2 // indirect
	golang.org/x/mod v0.39.0 // indirect
	golang.org/x/sync v0.22.0 // indirect
	golang.org/x/text v0.29.0 // indirect
)
