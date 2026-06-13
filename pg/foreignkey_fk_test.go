package pg_test

import (
	"strings"
	"testing"

	"github.com/bernardoforcillo/drops"
	"github.com/bernardoforcillo/drops/pg"
)

func TestTableForeignKeyUntyped(t *testing.T) {
	type parent struct {
		ID string `drop:"id,primaryKey"`
	}
	type child struct {
		ID       string `drop:"id,primaryKey"`
		ParentID string `drop:"parentId,notNull"`
	}
	parents := pg.AutoTable[parent]("parents")
	children := pg.AutoTable[child]("children")

	children.ForeignKey(children.Col("parentId"), parents.Col("id"), pg.OnDelete("cascade"))

	got, err := drops.String(pg.CreateTable(children))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, `REFERENCES "parents" ("id") ON DELETE cascade`) {
		t.Fatalf("missing FK DDL in:\n%s", got)
	}
}
