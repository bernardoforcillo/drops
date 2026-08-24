// Package reads holds the reads unboundedread must report, next to the
// reads of the same shape it must not: the schema facts that separate
// them arrive across an import edge, which is the part worth testing.
package reads

import (
	"context"

	"dropslint.corpus/shop"
	"github.com/bernardoforcillo/drops/pg"
)

var db *pg.DB

func everyUser(ctx context.Context) ([]shop.User, error) {
	return shop.UserEntity.Query(db).All(ctx) // want `reads every row of UserEntity`
}

func everyUserViaFind(ctx context.Context) error {
	var us []shop.User
	return db.Find(shop.Users).All(ctx, &us) // want `reads every row of Users`
}

// selectStar gives no projection at all, so it renders SELECT * and is
// a read of whole rows like any other.
func selectStar(ctx context.Context) error {
	var us []shop.User
	return db.Select().From(shop.Users).All(ctx, &us) // want `reads every row of Users`
}

// offsetWithoutLimit skips rows; it does not bound them.
func offsetWithoutLimit(ctx context.Context) ([]shop.User, error) {
	return shop.UserEntity.Query(db).Offset(500).All(ctx) // want `reads every row of UserEntity`
}

func throughAVariable(ctx context.Context, sorted bool) ([]shop.User, error) {
	q := shop.UserEntity.Query(db) // want `reads every row of UserEntity`
	if sorted {
		q = q.OrderBy(shop.UserEmail)
	}
	return q.All(ctx)
}

// bothTables reads one table the schema declared small and one it did
// not, in the same function. Only one of them is a finding, and the
// fact that says so came from the imported package.
func bothTables(ctx context.Context) error {
	if _, err := shop.CurrencyEntity.Query(db).All(ctx); err != nil {
		return err
	}
	_, err := shop.UserEntity.Query(db).All(ctx) // want `reads every row of UserEntity`
	return err
}
