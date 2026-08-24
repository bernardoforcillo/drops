package mysql_test

import (
	"testing"

	"github.com/bernardoforcillo/drops/mysql"
)

// A nil predicate is how a conditional filter says "no restriction":
// the `if q != ""` that every caller writes around an accumulator
// slice exists only because nil used to be a dereference or a dangling
// AND. See the same tests in the pg package — the answer has to be the
// same in every dialect or the boilerplate comes back.

func nilPredFixture() (*mysql.Table, *mysql.Table) {
	users := mysql.NewTable("users")
	mysql.Add(users, mysql.BigInt("id").PrimaryKey())
	mysql.Add(users, mysql.Varchar("name", 255).NotNull())
	mysql.Add(users, mysql.Integer("age").NotNull())

	posts := mysql.NewTable("posts")
	mysql.Add(posts, mysql.BigInt("id").PrimaryKey())
	mysql.Add(posts, mysql.BigInt("userId").NotNull())
	return users, posts
}

func TestAndOrDropNilPredicates(t *testing.T) {
	users, _ := nilPredFixture()
	x := mysql.Gte(users.Col("age"), 18)
	y := mysql.Ne(users.Col("name"), "")

	wantSQL(t, mysql.And(nil, x, nil), "(`users`.`age` >= ?)")
	wantSQL(t, mysql.Or(nil, x, nil), "(`users`.`age` >= ?)")
	wantSQL(t, mysql.And(nil, x, nil, y), "((`users`.`age` >= ?) AND (`users`.`name` <> ?))")
	wantSQL(t, mysql.Or(x, nil, y), "((`users`.`age` >= ?) OR (`users`.`name` <> ?))")

	wantSQL(t, mysql.And(nil, nil), "TRUE")
	wantSQL(t, mysql.Or(nil, nil), "FALSE")
	wantSQL(t, mysql.And(), "TRUE")
	wantSQL(t, mysql.Or(), "FALSE")

	wantSQL(t, mysql.And(nil, x, mysql.Or(nil, y, nil)),
		"((`users`.`age` >= ?) AND (`users`.`name` <> ?))")
	wantSQL(t, mysql.Not(nil), "(NOT TRUE)")
}

func TestWhereAndHavingDropNilPredicates(t *testing.T) {
	users, _ := nilPredFixture()
	db := mysql.New(nil)
	age := users.Col("age")

	got, _ := db.Select().From(users).Where(nil).ToSQL()
	if got != "SELECT * FROM `users`" {
		t.Errorf("an all-nil Where left a clause behind: %s", got)
	}

	got, _ = db.Select().From(users).Where(nil, mysql.Gte(age, 18), nil).ToSQL()
	if got != "SELECT * FROM `users` WHERE (`users`.`age` >= ?)" {
		t.Errorf("SQL = %s", got)
	}

	got, _ = db.Select(age).From(users).GroupBy(age).Having(nil).ToSQL()
	if got != "SELECT `users`.`age` FROM `users` GROUP BY `users`.`age`" {
		t.Errorf("an all-nil Having left a clause behind: %s", got)
	}

	got, _ = db.Delete(users).Where(nil, mysql.Gte(age, 18)).ToSQL()
	if got != "DELETE FROM `users` WHERE (`users`.`age` >= ?)" {
		t.Errorf("SQL = %s", got)
	}
}

func TestJoinOnNilRendersTheIdentity(t *testing.T) {
	users, posts := nilPredFixture()
	db := mysql.New(nil)
	got, _ := db.Select(users.Col("id")).From(users).LeftJoin(posts, nil).ToSQL()
	if got != "SELECT `users`.`id` FROM `users` LEFT JOIN `posts` ON TRUE" {
		t.Errorf("SQL = %s", got)
	}
}
