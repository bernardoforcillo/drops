package pg_test

import (
	"context"
	"fmt"

	"github.com/bernardoforcillo/drops"
	"github.com/bernardoforcillo/drops/pg"
)

// Schema fixtures used by the examples. Declaring them at package level
// mirrors how a real application stores its schema and lets every
// example reuse the same definitions.
var (
	exUsers    = pg.NewTable("users")
	exUserID   = pg.Add(exUsers, pg.BigSerial("id").PrimaryKey())
	exUserName = pg.Add(exUsers, pg.Text("name").NotNull())
	exUserAge  = pg.Add(exUsers, pg.Integer("age"))
)

// ExampleAdd shows the typical schema-declaration pattern: NewTable
// followed by package-level pg.Add calls, each binding a typed column
// to the table. Type inference keeps the column handles typed
// (*pg.Col[int64], *pg.Col[string], …) so subsequent comparisons and
// value bindings are compile-time checked.
func ExampleAdd() {
	products := pg.NewTable("products")
	id := pg.Add(products, pg.BigSerial("id").PrimaryKey())
	name := pg.Add(products, pg.Text("name").NotNull())
	priceCents := pg.Add(products, pg.Integer("price_cents").NotNull())

	fmt.Printf("%s.%s, %s.%s, %s.%s\n",
		products.Name(), id.Name(),
		products.Name(), name.Name(),
		products.Name(), priceCents.Name(),
	)
	// Output: products.id, products.name, products.price_cents
}

// ExampleDB_Select shows a typical filtered + ordered SELECT and how
// the typed column helpers produce parameter-safe SQL.
func ExampleDB_Select() {
	db := pg.New(nil) // no driver — we only render SQL
	sql, args := db.Select(exUserID, exUserName).
		From(exUsers).
		Where(exUserAge.Gte(18)).
		OrderBy(exUserName.Asc()).
		Limit(10).
		ToSQL()
	fmt.Println(sql)
	fmt.Println(args)
	// Output:
	// SELECT "users"."id", "users"."name" FROM "users" WHERE ("users"."age" >= $1) ORDER BY "users"."name" ASC LIMIT $2
	// [18 10]
}

// ExampleDB_Insert demonstrates a typed INSERT with RETURNING.
func ExampleDB_Insert() {
	db := pg.New(nil)
	sql, args := db.Insert(exUsers).
		Row(exUserName.Val("Alice"), exUserAge.Val(30)).
		Returning(exUserID).
		ToSQL()
	fmt.Println(sql)
	fmt.Println(args)
	// Output:
	// INSERT INTO "users" ("name", "age") VALUES ($1, $2) RETURNING "users"."id"
	// [Alice 30]
}

// ExampleDB_WithHook shows attaching a tiny observability hook for
// query logging. Production code usually pairs LoggerHook with a
// structured logger and a SlowQuery threshold.
func ExampleDB_WithHook() {
	hook := func(_ context.Context, e drops.QueryEvent) {
		fmt.Printf("%s in %v err=%v\n", e.Kind, e.Duration > 0, e.Err)
	}
	db := pg.New(&exampleNoopDriver{}).WithHook(hook)
	_, _ = db.Exec(context.Background(), "SELECT 1")
	// Output: exec in true err=<nil>
}

// ExampleEq shows the type-safe comparison shorthand on a typed column.
func ExampleCol_Eq() {
	sql, args := drops.String(exUserName.Eq("Alice"))
	fmt.Println(sql)
	fmt.Println(args)
	// Output:
	// ("users"."name" = $1)
	// [Alice]
}

// exampleNoopDriver lets the examples render SQL through DB.Exec
// without depending on the test-only fakeDriver. It is purely for
// documentation; production code never embeds a no-op driver.
type exampleNoopDriver struct{}

func (exampleNoopDriver) Exec(context.Context, string, ...any) (drops.Result, error) {
	return exampleNoopResult{}, nil
}
func (exampleNoopDriver) Query(context.Context, string, ...any) (drops.Rows, error) {
	return nil, fmt.Errorf("not implemented")
}
func (exampleNoopDriver) Begin(context.Context) (drops.Tx, error) {
	return nil, fmt.Errorf("not implemented")
}

type exampleNoopResult struct{}

func (exampleNoopResult) RowsAffected() (int64, error) { return 0, nil }
