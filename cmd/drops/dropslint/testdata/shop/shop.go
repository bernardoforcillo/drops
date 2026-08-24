// Package shop is the schema the corpus queries against. It is a
// normal drops schema — tables, relations, entities — because the
// rules read the declarations, and a fixture that declared them any
// other way would be testing something nobody writes.
package shop

import "github.com/bernardoforcillo/drops/pg"

// Users is the big table: no budget, no lookup directive, so a read of
// all of it is a finding.
var (
	Users     = pg.NewTable("users")
	UserID    = pg.Add(Users, pg.BigSerial("id").PrimaryKey())
	UserEmail = pg.Add(Users, pg.Text("email").NotNull())
	UserOrgID = pg.Add(Users, pg.BigInt("org_id").NotNull())
)

// Posts hangs off Users.
var (
	Posts      = pg.NewTable("posts")
	PostID     = pg.Add(Posts, pg.BigSerial("id").PrimaryKey())
	PostUserID = pg.Add(Posts, pg.BigInt("user_id").NotNull().References(UserID))
	PostTitle  = pg.Add(Posts, pg.Text("title").NotNull())
)

// Currencies holds one row per ISO 4217 code and will never hold many
// more, so reading all of it is the intended use.
//
//drops:lint lookup
var (
	Currencies   = pg.NewTable("currencies")
	CurrencyCode = pg.Add(Currencies, pg.Text("code").PrimaryKey())
	CurrencyName = pg.Add(Currencies, pg.Text("name").NotNull())
)

func init() {
	pg.NewRelations(Users).HasMany("posts", Posts, UserID, PostUserID)
	pg.NewRelations(Posts).BelongsTo("author", Users, PostUserID, UserID)
}

// User is the row Users scans into.
type User struct {
	ID    int64  `drop:"id"`
	Email string `drop:"email"`
	OrgID int64  `drop:"org_id"`
	Posts []Post `dropRel:"posts"`
}

// Post is the row Posts scans into.
type Post struct {
	ID     int64  `drop:"id"`
	UserID int64  `drop:"user_id"`
	Title  string `drop:"title"`
}

// Currency is the row Currencies scans into.
type Currency struct {
	Code string `drop:"code"`
	Name string `drop:"name"`
}

// UserEntity reads a table nobody has bounded.
var UserEntity = pg.NewEntity[User](Users)

// PostEntity caps what one query may return, which is the run-time
// half of the same answer unboundedread gives at build time.
var PostEntity = pg.NewEntity[Post](Posts).WithBudget(pg.Budget{MaxRows: 5000})

// CurrencyEntity is bounded by the table, not by a budget.
var CurrencyEntity = pg.NewEntity[Currency](Currencies)

// Regions is declared on its own rather than in a group, so the
// directive sits on the spec instead of the decl.
var (
	//drops:lint lookup
	Regions = pg.NewTable("regions")

	RegionCode = pg.Add(Regions, pg.Text("code").PrimaryKey())
	RegionName = pg.Add(Regions, pg.Text("name").NotNull())
)

// Region is the row Regions scans into.
type Region struct {
	Code string `drop:"code"`
	Name string `drop:"name"`
}

// RegionEntity inherits the table's mark.
var RegionEntity = pg.NewEntity[Region](Regions)

// Schema is the convention the CLI reads.
func Schema() *pg.Schema { return pg.NewSchema(Users, Posts, Currencies, Regions) }
