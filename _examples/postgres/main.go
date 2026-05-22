// Example program demonstrating the drops/pg package against a real
// PostgreSQL instance via the database/sql adapter.
//
// This file lives under _examples/ so it is excluded from the parent
// module's build (and therefore avoids forcing pgx as a dependency on
// the core library). To run it:
//
//	cd _examples/postgres
//	go mod init example.com/drops-pg-demo
//	go mod edit -replace github.com/bernardoforcillo/drops=../..
//	go mod tidy
//	DROPS_DSN=postgres://... go run .
package main

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"os"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/bernardoforcillo/drops"
	"github.com/bernardoforcillo/drops/pg"
	"github.com/bernardoforcillo/drops/stdlib"
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

type User struct {
	ID   int64
	Name string
	Age  *int32
}

type Post struct {
	ID     int64
	UserID int64 `db:"user_id"`
	Title  string
}

func main() {
	dsn := os.Getenv("DROPS_DSN")
	if dsn == "" {
		dsn = "postgres://postgres:postgres@localhost:5432/drops?sslmode=disable"
	}
	sqlDB, err := sql.Open("pgx", dsn)
	if err != nil {
		log.Fatal(err)
	}
	defer sqlDB.Close()

	db := pg.New(stdlib.New(sqlDB))
	ctx := context.Background()

	for _, stmt := range []drops.Expression{
		pg.DropTableIfExists(Posts),
		pg.DropTableIfExists(Users),
		pg.CreateTable(Users),
		pg.CreateTable(Posts),
	} {
		if _, err := db.ExecExpr(ctx, stmt); err != nil {
			log.Fatalf("ddl: %v", err)
		}
	}

	var alice User
	if err := db.Insert(Users).
		Row(UserName.Val("Alice"), UserAge.Val(30)).
		Returning(UserID, UserName, UserAge).
		One(ctx, &alice); err != nil {
		log.Fatalf("insert: %v", err)
	}
	fmt.Printf("inserted: %+v\n", alice)

	if _, err := db.Insert(Users).
		Row(UserName.Val("Bob"), UserAge.Val(25)).
		Row(UserName.Val("Carol")). // age omitted → DEFAULT
		Exec(ctx); err != nil {
		log.Fatalf("batch insert: %v", err)
	}

	if _, err := db.Insert(Posts).
		Row(PostUserID.Val(alice.ID), PostTitle.Val("Hello")).
		Row(PostUserID.Val(alice.ID), PostTitle.Val("World")).
		Exec(ctx); err != nil {
		log.Fatalf("posts: %v", err)
	}

	var users []User
	if err := db.Select(UserID, UserName, UserAge).
		From(Users).
		Where(pg.Or(UserAge.Gte(25), UserAge.IsNull())).
		OrderBy(UserName.Asc()).
		All(ctx, &users); err != nil {
		log.Fatalf("select: %v", err)
	}
	for _, u := range users {
		fmt.Printf("user: %+v\n", u)
	}

	type counted struct {
		Name  string
		Count int64
	}
	var counts []counted
	if err := db.Select(
		pg.As(UserName, "name"),
		pg.As(pg.CountAll(), "count"),
	).
		From(Users).
		LeftJoin(Posts, PostUserID.EqCol(UserID)).
		GroupBy(UserID, UserName).
		OrderBy(UserName.Asc()).
		All(ctx, &counts); err != nil {
		log.Fatalf("join: %v", err)
	}
	for _, c := range counts {
		fmt.Printf("%-10s %d posts\n", c.Name, c.Count)
	}

	if _, err := db.Insert(Users).
		Row(UserID.Val(alice.ID), UserName.Val("Alice"), UserAge.Val(31)).
		OnConflictUpdate(UserID).
		Set(UserAge.Expr(UserAge.Excluded())).
		Done().
		Exec(ctx); err != nil {
		log.Fatalf("upsert: %v", err)
	}

	res, err := db.Update(Users).
		Set(UserAge.Val(26)).
		Where(UserName.Eq("Bob")).
		Exec(ctx)
	if err != nil {
		log.Fatalf("update: %v", err)
	}
	n, _ := res.RowsAffected()
	fmt.Printf("updated %d rows\n", n)

	if err := db.InTx(ctx, func(tx *pg.DB) error {
		if _, err := tx.Delete(Posts).Where(PostUserID.Eq(alice.ID)).Exec(ctx); err != nil {
			return err
		}
		_, err := tx.Delete(Users).Where(UserID.Eq(alice.ID)).Exec(ctx)
		return err
	}); err != nil {
		log.Fatalf("tx: %v", err)
	}
}
