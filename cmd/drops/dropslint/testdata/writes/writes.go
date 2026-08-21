// Package writes holds the statements unfilteredwrite must report:
// a DELETE or an UPDATE that reaches the server with no WHERE.
package writes

import (
	"context"

	"github.com/bernardoforcillo/drops"

	"dropslint.corpus/shop"
	"github.com/bernardoforcillo/drops/mysql"
	"github.com/bernardoforcillo/drops/pg"
	"github.com/bernardoforcillo/drops/sqlite"
)

var (
	db   *pg.DB
	lite *sqlite.DB
	my   *mysql.DB
)

// theClassic is the statement the whole rule exists for.
func theClassic(ctx context.Context) error {
	_, err := db.Delete(shop.Users).Exec(ctx) // want `DELETE on Users has no Where`
	return err
}

func unfilteredUpdate(ctx context.Context) error {
	_, err := db.Update(shop.Users).Set(shop.UserEmail.Val("")).Exec(ctx) // want `UPDATE on Users has no Where`
	return err
}

// throughAVariable is the same mistake spread over four statements,
// which is how it usually reaches review unnoticed.
func throughAVariable(ctx context.Context) error {
	q := db.Delete(shop.Users) // want `DELETE on Users has no Where`
	q = q.Returning(shop.UserID)
	var ids []int64
	return q.All(ctx, &ids)
}

// ignoringFiltersIsNotFiltering: Unscoped drops the table's global
// filters. It does not add a predicate, it removes them.
func ignoringFiltersIsNotFiltering(ctx context.Context) error {
	_, err := db.Delete(shop.Posts).Unscoped().Exec(ctx) // want `DELETE on Posts has no Where`
	return err
}

// wrongRuleNamed silences a rule that is not the one firing.
func wrongRuleNamed(ctx context.Context) error {
	//drops:lint ignore unboundedread
	_, err := db.Delete(shop.Posts).Exec(ctx) // want `DELETE on Posts has no Where`
	return err
}

// otherDialects: the rule reads types, and every dialect spells the
// builder the same way.
func otherDialects(ctx context.Context, t *sqlite.Table, m *mysql.Table) error {
	if _, err := lite.Delete(t).Exec(ctx); err != nil { // want `DELETE on the table has no Where`
		return err
	}
	_, err := my.Delete(m).Exec(ctx) // want `DELETE on the table has no Where`
	return err
}

// The builders drop nil predicates rather than panicking on them, so
// a Where the caller believed was a filter can be nothing at all. The
// rule counts the call by what it carries, not by its name — a
// spelling that silences the rule while deleting every row is the one
// case that needs it most.
func whereNil(ctx context.Context) error {
	_, err := db.Delete(shop.Users).Where(nil).Exec(ctx) // want `DELETE on Users has no Where`
	return err
}

func whereAllNil(ctx context.Context) error {
	_, err := db.Update(shop.Users).Set(shop.UserEmail.Val("")).Where(nil, nil).Exec(ctx) // want `UPDATE on Users has no Where`
	return err
}

func whereEmpty(ctx context.Context) error {
	_, err := db.Delete(shop.Users).Where().Exec(ctx) // want `DELETE on Users has no Where`
	return err
}

// A variadic expansion carries a predicate as far as the analyzer can
// tell, and a rule that fires on a caller who accumulated predicates
// properly is a rule people switch off.
func whereSpread(ctx context.Context, preds []drops.Expression) error {
	_, err := db.Delete(shop.Users).Where(preds...).Exec(ctx)
	return err
}
