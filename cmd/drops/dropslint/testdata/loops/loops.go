// Package loops holds the N+1s loopload must report. Each finding is
// anchored where the query starts, which is where a reader looks to
// see what it fetches.
package loops

import (
	"context"

	"dropslint.corpus/shop"
	"github.com/bernardoforcillo/drops/pg"
)

var db *pg.DB

// perOrg is the textbook shape: one query per iteration, and each one
// drags the relation load behind it.
func perOrg(ctx context.Context, orgs []int64) error {
	for _, org := range orgs {
		users, err := shop.UserEntity.Query(db). // want `eager-loads a relation inside a loop`
								Where(shop.UserOrgID.Eq(org)).
								With("posts").
								All(ctx)
		if err != nil {
			return err
		}
		_ = users
	}
	return nil
}

// viaFind is the same thing on the FindBuilder.
func viaFind(ctx context.Context, ids []int64) error {
	for _, id := range ids {
		var us []shop.User
		err := db.Find(shop.Users).Where(shop.UserID.Eq(id)).Load(shop.Users.Rel("posts")).All(ctx, &us) // want `eager-loads a relation inside a loop`
		if err != nil {
			return err
		}
	}
	return nil
}

// hoistedQueryStillLooped builds the query once but runs it inside the
// loop, which is the same number of round trips.
func hoistedQueryStillLooped(ctx context.Context, ids []int64) error {
	q := shop.UserEntity.Query(db).With("posts")
	for _, id := range ids {
		if _, err := q.Where(shop.UserID.Eq(id)).One(ctx); err != nil { // want `eager-loads a relation inside a loop`
			return err
		}
	}
	return nil
}

// nested reports once, at the query, not once per enclosing loop.
func nested(ctx context.Context, orgs [][]int64) error {
	for _, org := range orgs {
		for _, id := range org {
			_, err := shop.UserEntity.Query(db).Where(shop.UserID.Eq(id)).With("posts").One(ctx) // want `eager-loads a relation inside a loop`
			if err != nil {
				return err
			}
		}
	}
	return nil
}

// streamedPerRow still issues the relation queries per iteration.
func streamedPerRow(ctx context.Context, ids []int64) error {
	for _, id := range ids {
		q := shop.UserEntity.Query(db).Where(shop.UserID.Eq(id)).With("posts")
		if err := q.Stream(ctx, func(u *shop.User) error { return nil }); err != nil { // want `eager-loads a relation inside a loop`
			return err
		}
	}
	return nil
}
