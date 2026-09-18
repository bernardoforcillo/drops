package d1_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"

	"github.com/bernardoforcillo/drops"
	"github.com/bernardoforcillo/drops/cloudflare"
	"github.com/bernardoforcillo/drops/cloudflare/d1"
)

// Reaching D1 from outside Cloudflare's network — a migration runner,
// a CI job, a laptop.
func ExampleNew() {
	cf, err := cloudflare.New("your-account-id", cloudflare.WithAPIToken("your-api-token"))
	if err != nil {
		log.Fatal(err)
	}
	drv := d1.New(cf, "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee")

	// From here it is the SQLite dialect, unchanged:
	//
	//	db := sqlite.New(drv)
	//	users, err := sqlite.All[User](Users.Select(), db, ctx)
	_ = drv
}

// Reaching D1 from inside Cloudflare — a Container talking to the
// Worker that holds the binding. No API token: the binding is the
// authorisation.
func ExampleNewBridge() {
	drv, err := d1.NewBridge("http://d1.internal",
		d1.WithBridgeOptions(d1.WithBridgeHeader("X-Service", "tenders")))
	if err != nil {
		log.Fatal(err)
	}
	_ = drv
}

// A transaction is a buffer that ships at commit, so a write-only
// unit of work reads exactly as it would against PostgreSQL.
func ExampleDriver_Begin() {
	var drv *d1.Driver // from d1.New or d1.NewBridge
	ctx := context.Background()

	err := drops.InTx(ctx, drv, func(tx drops.Tx) error {
		if _, err := tx.Exec(ctx,
			"INSERT INTO orders (id, total) VALUES (?, ?)", "o-1", 4200); err != nil {
			return err
		}
		_, err := tx.Exec(ctx,
			"INSERT INTO order_lines (order_id, sku) VALUES (?, ?)", "o-1", "SKU-9")
		return err
	})
	_ = err
}

// Reading inside a transaction is refused rather than run outside it,
// because running it outside would break read-your-writes silently.
// Do the read first, and put the guard in the statement.
func ExampleDriver_Begin_readModifyWrite() {
	var drv *d1.Driver
	ctx := context.Background()

	// Not: read the version inside the transaction, compare, write.
	// Instead: make the write itself conditional, and let the row
	// count report whether it won.
	res, err := drv.Exec(ctx,
		"UPDATE tenders SET status = ?, version = version + 1 WHERE id = ? AND version = ?",
		"awarded", "t-7", 3)
	if err != nil {
		log.Fatal(err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		log.Fatal(err)
	}
	if n == 0 {
		fmt.Println("someone else got there first")
	}
}

// A batch is the same mechanism without the drops.Tx clothing, and
// the clearer spelling when the unit of work is already a list.
func ExampleBatch() {
	var drv *d1.Driver
	ctx := context.Background()

	b := d1.NewBatch(drv)
	b.Add("INSERT INTO chunks (doc, n, body) VALUES (?, ?, ?)", "doc-1", 0, "…")
	b.Add("INSERT INTO chunks (doc, n, body) VALUES (?, ?, ?)", "doc-1", 1, "…")
	b.Add("UPDATE documents SET chunk_count = ? WHERE id = ?", 2, "doc-1")

	results, err := b.Run(ctx)
	if err != nil {
		log.Fatal(err)
	}
	for i, r := range results {
		n, _ := r.RowsAffected()
		fmt.Printf("statement %d wrote %d row(s)\n", i, n)
	}
}

// A violated index answers as a sentinel, not as a string to match
// on, and names the column that was refused.
func ExampleClassifyError() {
	var drv *d1.Driver
	ctx := context.Background()

	_, err := drv.Exec(ctx, "INSERT INTO users (email) VALUES (?)", "ada@example.com")
	switch {
	case errors.Is(err, d1.ErrUniqueViolation):
		_, columns := d1.ConstraintColumns(err)
		fmt.Printf("%v is already taken\n", columns)
	case errors.Is(err, cloudflare.ErrUnauthorized):
		fmt.Println("the API token cannot write this database")
	case err != nil:
		log.Fatal(err)
	}
}

// The parameter ceiling is met by ordinary code sooner than anyone
// expects: an IN over a slice reaches it at a hundred values. The
// way through is one JSON parameter.
func ExampleDriver_Query_manyIDs() {
	var drv *d1.Driver
	ctx := context.Background()

	ids := make([]string, 1500)
	for i := range ids {
		ids[i] = fmt.Sprintf("t-%d", i)
	}
	// Not: strings.Repeat("?,", len(ids)) — that is 1500 parameters
	// against a limit of 100.
	encoded, err := jsonArray(ids)
	if err != nil {
		log.Fatal(err)
	}
	rows, err := drv.Query(ctx,
		"SELECT id, title FROM tenders WHERE id IN (SELECT value FROM json_each(?))",
		encoded)
	if err != nil {
		log.Fatal(err)
	}
	for rows.Next() {
		var id, title string
		if err := rows.Scan(&id, &title); err != nil {
			log.Fatal(err)
		}
	}
	_ = rows.Close()
}

// RowsRead is the number D1 bills on, and the cheapest index-coverage
// check there is: compare it against the rows you expected to match.
func ExampleResult_Meta() {
	var drv *d1.Driver
	ctx := context.Background()

	res, err := drv.Exec(ctx, "DELETE FROM sessions WHERE expires_at < ?", 1700000000)
	if err != nil {
		log.Fatal(err)
	}
	if r, ok := res.(*d1.Result); ok {
		fmt.Printf("scanned %d rows to delete %d\n", r.Meta().RowsRead, r.Meta().Changes)
	}
}

func jsonArray(values []string) (string, error) {
	raw, err := json.Marshal(values)
	return string(raw), err
}

// A replica is allowed to be behind the primary, so without a session
// two consecutive reads can be served by two instances and the second
// can see less than the first. A Session is a drops.Driver, so the
// whole dialect runs inside one.
func ExampleDriver_Session() {
	// Sessions are a Worker binding feature, so they need the bridge.
	drv, err := d1.NewBridge("http://d1.internal")
	if err != nil {
		log.Fatal(err)
	}
	sess, err := drv.Session(d1.FirstUnconstrained)
	if err != nil {
		// d1.ErrSessionsUnsupported here means the driver is the REST
		// one: Cloudflare does not offer sessions over that API.
		log.Fatal(err)
	}
	defer func() { _ = sess.Close() }()

	rows, err := sess.Query(context.Background(), "SELECT id, email FROM users WHERE id = ?", 42)
	if err != nil {
		log.Print(err)
		return
	}
	defer rows.Close()

	// Which instance answered, for when a read-your-writes bug is
	// suspected.
	if r, ok := rows.(*d1.Rows); ok {
		fmt.Println("served by the primary:", r.Meta().ServedByPrimary)
	}
}

// Carrying read-your-writes across the end of a request: the request
// that wrote stores the bookmark, the one that reads resumes from it.
func ExampleDriver_Resume() {
	var drv *d1.Driver
	ctx := context.Background()

	// … in the request that writes:
	writer, err := drv.Session(d1.FirstPrimary)
	if err != nil {
		log.Fatal(err)
	}
	if _, err := writer.Exec(ctx, "UPDATE users SET email = ? WHERE id = ?", "ada@example.com", 42); err != nil {
		log.Fatal(err)
	}
	bookmark := writer.Bookmark() // into a cookie, a header, a queue message

	// … in the request that reads it back, possibly on another
	// continent:
	reader, err := drv.Resume(bookmark)
	if err != nil {
		log.Fatal(err)
	}
	rows, err := reader.Query(ctx, "SELECT email FROM users WHERE id = ?", 42)
	if err != nil {
		log.Fatal(err)
	}
	rows.Close()
}

// D1's 10 GB ceiling is what makes database-per-tenant a real design
// rather than an eccentric one. Names are unique within an account,
// so the tenant's identifier names the database and the UUID never
// has to be stored anywhere.
func ExampleAdmin_Create() {
	cf, err := cloudflare.New("your-account-id", cloudflare.WithAPIToken("your-api-token"))
	if err != nil {
		log.Fatal(err)
	}
	admin := d1.NewAdmin(cf)
	ctx := context.Background()

	db, err := admin.Create(ctx, d1.CreateOptions{
		Name:            "tenant-42",
		PrimaryLocation: d1.LocationWesternEurope,
	})
	if err != nil {
		log.Fatal(err)
	}
	// The schema goes in through the dialect's migrations, like any
	// other SQLite database.
	drv := admin.Driver(db.UUID)
	_ = drv
}

// The way back from a migration that went wrong.
func ExampleAdmin_Restore() {
	var admin *d1.Admin
	ctx := context.Background()
	const databaseID = "your-database-id"

	before, err := admin.Bookmark(ctx, databaseID)
	if err != nil {
		log.Fatal(err)
	}

	if err := runMigration(ctx); err != nil {
		res, rErr := admin.Restore(ctx, databaseID, d1.RestoreOptions{Bookmark: before})
		if rErr != nil {
			log.Fatal(rErr)
		}
		// A restore is itself a change to the database's history, so
		// this is the only handle on the state it replaced. Record it
		// before anything else.
		log.Printf("restored; undo with bookmark %s", res.PreviousBookmark)
	}
}

func runMigration(context.Context) error { return nil }
