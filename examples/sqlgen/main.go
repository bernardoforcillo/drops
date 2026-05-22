// sqlgen prints the SQL produced by the drops/pg builders. It does not
// connect to a database, so it has no external dependencies.
//
// Run with:
//
//	go run ./examples/sqlgen
package main

import (
	"fmt"

	"github.com/bernardoforcillo/drops"
	"github.com/bernardoforcillo/drops/pg"
)

var (
	Users    = pg.NewTable("users")
	UserID   = pg.Add(Users, pg.BigSerial("id").PrimaryKey())
	UserName = pg.Add(Users, pg.Text("name").NotNull())
	UserAge  = pg.Add(Users, pg.Integer("age"))

	Posts      = pg.NewTable("posts")
	PostID     = pg.Add(Posts, pg.BigSerial("id").PrimaryKey())
	PostUserID = pg.Add(Posts, pg.BigInt("user_id").NotNull().References(UserID, pg.OnDelete("CASCADE")))
	PostTitle  = pg.Add(Posts, pg.Text("title").NotNull())
)

type renderable interface {
	ToSQL() (string, []any)
}

func show(label string, q any) {
	fmt.Println("--", label)
	switch v := q.(type) {
	case renderable:
		s, args := v.ToSQL()
		fmt.Println(s)
		if len(args) > 0 {
			fmt.Printf("args: %v\n", args)
		}
	case drops.Expression:
		s, args := drops.String(v)
		fmt.Println(s)
		if len(args) > 0 {
			fmt.Printf("args: %v\n", args)
		}
	}
	fmt.Println()
}

func main() {
	// We don't need a real driver to render SQL — pass nil and avoid
	// calling any Exec/Query methods.
	db := pg.New(nil)

	show("CREATE TABLE users", pg.CreateTable(Users))
	show("CREATE TABLE posts", pg.CreateTable(Posts))

	show("SELECT * with WHERE/ORDER (typed ops)",
		db.Select().
			From(Users).
			Where(pg.Or(UserAge.Gte(18), UserAge.IsNull())).
			OrderBy(UserName.Asc()).
			Limit(10),
	)

	show("LEFT JOIN with COUNT and GROUP BY",
		db.Select(
			pg.As(UserName, "name"),
			pg.As(pg.CountAll(), "count"),
		).
			From(Users).
			LeftJoin(Posts, PostUserID.EqCol(UserID)).
			GroupBy(UserID, UserName).
			OrderBy(UserName.Asc()),
	)

	show("INSERT with RETURNING",
		db.Insert(Users).
			Row(UserName.Val("Alice"), UserAge.Val(30)).
			Returning(UserID, UserName, UserAge),
	)

	show("Batch INSERT",
		db.Insert(Users).
			Row(UserName.Val("Alice"), UserAge.Val(30)).
			Row(UserName.Val("Bob"), UserAge.Val(25)),
	)

	show("UPSERT",
		db.Insert(Users).
			Row(UserID.Val(1), UserName.Val("Alice"), UserAge.Val(31)).
			OnConflictUpdate(UserID).
			Set(UserAge.Expr(UserAge.Excluded())).
			Done(),
	)

	show("UPDATE with RETURNING",
		db.Update(Users).
			Set(UserAge.Val(26)).
			Where(UserName.Eq("Bob")).
			Returning(UserID),
	)

	show("DELETE",
		db.Delete(Users).
			Where(UserName.In("Carol", "Dave")),
	)
}
