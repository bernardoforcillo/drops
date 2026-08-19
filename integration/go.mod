// The integration suite is a separate module on purpose.
//
// drops itself has no dependencies, and the CI `go mod tidy` job
// asserts it by diffing the tree. Testing against real servers needs
// real drivers — pgx, go-sql-driver/mysql, clickhouse-go — so those
// live here, where they cannot leak into what a user of the library
// pulls in.
module github.com/bernardoforcillo/drops/integration

go 1.25.0

replace github.com/bernardoforcillo/drops => ../

require (
	github.com/bernardoforcillo/drops v0.0.0-00010101000000-000000000000
	modernc.org/sqlite v1.57.0
)

require (
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/mattn/go-isatty v0.0.24 // indirect
	github.com/ncruces/go-strftime v1.0.0 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	golang.org/x/sys v0.47.0 // indirect
	modernc.org/libc v1.74.4 // indirect
	modernc.org/mathutil v1.7.1 // indirect
	modernc.org/memory v1.11.0 // indirect
)
