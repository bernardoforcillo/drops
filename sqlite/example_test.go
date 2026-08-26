package sqlite_test

import (
	"fmt"

	"github.com/bernardoforcillo/drops"
	"github.com/bernardoforcillo/drops/sqlite"
)

// A schema is a var block: each Add returns a typed handle, so
// comparisons against it are checked at compile time.
var (
	Notes        = sqlite.NewTable("notes")
	NoteID       = sqlite.Add(Notes, sqlite.Integer("id").PrimaryKey().AutoIncrement())
	NoteTitle    = sqlite.Add(Notes, sqlite.Text("title").NotNull())
	NoteArchived = sqlite.Add(Notes, sqlite.Boolean("archived").NotNull().Default("0"))
)

func ExampleCreateTable() {
	sql, _ := drops.StringWithDialect(sqlite.Dialect, sqlite.CreateTable(Notes))
	fmt.Println(sql)
	// Output:
	// CREATE TABLE "notes" (
	//   "id" INTEGER PRIMARY KEY AUTOINCREMENT NOT NULL,
	//   "title" TEXT NOT NULL,
	//   "archived" BOOLEAN NOT NULL DEFAULT 0
	// )
}

func ExampleSelectBuilder() {
	db := sqlite.New(&entDriver{})
	sql, args := db.Select(NoteID, NoteTitle).
		From(Notes).
		Where(NoteArchived.Eq(false)).
		OrderBy(NoteID.Desc()).
		Limit(20).
		ToSQL()
	fmt.Println(sql)
	fmt.Println(args...)
	// Output:
	// SELECT "notes"."id", "notes"."title" FROM "notes" WHERE ("notes"."archived" = ?) ORDER BY "notes"."id" DESC LIMIT ?
	// false 20
}

// A DefaultFilter is AND-ed onto every SELECT from the table, which is
// how a soft-delete or tenant guard becomes impossible to forget.
func ExampleTable_DefaultFilter() {
	tbl := sqlite.NewTable("tasks")
	sqlite.Add(tbl, sqlite.Integer("id").PrimaryKey())
	done := sqlite.Add(tbl, sqlite.Boolean("done").NotNull().Default("0"))
	tbl.DefaultFilter(done.Eq(false))

	db := sqlite.New(&entDriver{})
	scoped, _ := db.Select().From(tbl).ToSQL()
	unscoped, _ := db.Select().From(tbl).Unscoped().ToSQL()
	fmt.Println(scoped)
	fmt.Println(unscoped)
	// Output:
	// SELECT * FROM "tasks" WHERE ("tasks"."done" = ?)
	// SELECT * FROM "tasks"
}

// A tenant guard renders the triggers that hold a table's tenant axis
// against SQL drops did not build.
//
// What is NOT in these statements is the point of them: they say
// nothing about who may read the file, because SQLite has no principal
// to say it about. They refuse a write that would leave a row with no
// tenant or move one between tenants, whoever sends it. Read the file
// comment in tenantguard.go for where that stops.
func ExampleCreateTenantGuard() {
	docs := sqlite.NewTable("docs")
	sqlite.Add(docs, sqlite.Integer("id").PrimaryKey())
	tenant := sqlite.Add(docs, sqlite.Text("tenantId").NotNull())
	docs.ScopeWritesByTenant(tenant)

	for _, stmt := range sqlite.CreateTenantGuard(sqlite.TenantGuardFor(docs)) {
		sql, _ := drops.StringWithDialect(sqlite.Dialect, stmt)
		fmt.Println(sql)
	}
	// Output:
	// CREATE TRIGGER "docs_tenantGuard_ins" BEFORE INSERT ON "docs" FOR EACH ROW WHEN NEW."tenantId" IS NULL BEGIN SELECT RAISE(ABORT, 'drops/sqlite: "docs"."tenantId" is null; the row would belong to no tenant'); END
	// CREATE TRIGGER "docs_tenantGuard_upd" BEFORE UPDATE OF "tenantId" ON "docs" FOR EACH ROW WHEN NEW."tenantId" IS NOT OLD."tenantId" BEGIN SELECT RAISE(ABORT, 'drops/sqlite: "docs"."tenantId" is immutable; this statement would move a row between tenants'); END
}
