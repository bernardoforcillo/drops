// Package clean is the half of the corpus that matters most: every
// query here is written the way a careful author writes it, and no
// rule may say a word about any of it. A linter is judged on this
// file, not on the ones that follow.
package clean

import (
	"context"

	"dropslint.corpus/shop"
	"github.com/bernardoforcillo/drops/mysql"
	"github.com/bernardoforcillo/drops/pg"
)

var (
	db *pg.DB
	my *mysql.DB
)

// ---- unfilteredwrite: writes that are scoped ----

func deleteOne(ctx context.Context, id int64) error {
	_, err := db.Delete(shop.Users).Where(shop.UserID.Eq(id)).Exec(ctx)
	return err
}

func updateOne(ctx context.Context, id int64, email string) error {
	_, err := db.Update(shop.Users).
		Set(shop.UserEmail.Val(email)).
		Where(shop.UserID.Eq(id)).
		Exec(ctx)
	return err
}

// deleteViaVar narrows through a variable rather than in one chain.
func deleteViaVar(ctx context.Context, id int64) error {
	q := db.Delete(shop.Users)
	q = q.Where(shop.UserID.Eq(id))
	_, err := q.Exec(ctx)
	return err
}

// deleteConditionally is the shape the flow-insensitive union exists
// for: the Where is only reached on one path, and the rule still says
// nothing. Reporting it would mean reporting the careful spelling of
// the code and not the careless one.
func deleteConditionally(ctx context.Context, id int64, narrow bool) error {
	q := db.Delete(shop.Users)
	if narrow {
		q = q.Where(shop.UserID.Eq(id))
	}
	_, err := q.Exec(ctx)
	return err
}

// renderOnly builds the statement and never runs it. Rendering a
// DELETE is what every dialect test in the tree does; it is not a
// delete.
func renderOnly() (string, []any) {
	return db.Delete(shop.Users).ToSQL()
}

// handOff sends the builder somewhere the analyzer cannot follow.
func handOff(ctx context.Context) error {
	q := db.Delete(shop.Users)
	narrow(q)
	_, err := q.Exec(ctx)
	return err
}

func narrow(q *pg.DeleteBuilder) { q.Where(shop.UserOrgID.Eq(1)) }

// fromHelper executes a builder built in another function: the origin
// is across a boundary the rules do not cross.
func fromHelper(ctx context.Context) error {
	_, err := pending().Exec(ctx)
	return err
}

func pending() *pg.DeleteBuilder {
	return db.Delete(shop.Users).Where(shop.UserOrgID.Eq(1))
}

// byPrimaryKey goes through the entity, which takes a key and builds
// the predicate itself.
func byPrimaryKey(ctx context.Context, id int64) error {
	_, err := shop.UserEntity.Delete(db, ctx, id)
	return err
}

// truncateSessions means every row, and says so.
//
//drops:lint ignore unfilteredwrite
func truncateSessions(ctx context.Context) error {
	_, err := db.Delete(shop.Posts).Exec(ctx)
	return err
}

func truncateInline(ctx context.Context) error {
	//drops:lint ignore unfilteredwrite
	_, err := db.Delete(shop.Posts).Exec(ctx)
	return err
}

// truncateWithReason writes the reason on the directive itself, which
// is where a reviewer will look for it.
func truncateWithReason(ctx context.Context) error {
	//drops:lint ignore unfilteredwrite — the table is a scratch buffer
	_, err := db.Delete(shop.Posts).Exec(ctx)
	return err
}

// batched is how MySQL deletes a large table without holding one
// enormous transaction: no Where, and a LIMIT that bounds the
// statement exactly as a Where would. pg has no such method, so this
// shape only arises where the dialect allows it.
func batched(ctx context.Context, tbl *mysql.Table) error {
	_, err := my.Delete(tbl).Limit(1000).Exec(ctx)
	return err
}

// wipeBoth silences two rules at once: it empties a table and reads
// what is left, and means both.
//
//drops:lint ignore unfilteredwrite,unboundedread
func wipeBoth(ctx context.Context) ([]shop.User, error) {
	if _, err := db.Delete(shop.Users).Exec(ctx); err != nil {
		return nil, err
	}
	return shop.UserEntity.Query(db).All(ctx)
}

// nukeEverything names no rule, which silences all of them. The blunt
// instrument, and it reads like one.
func nukeEverything(ctx context.Context) error {
	//drops:lint ignore
	_, err := db.Delete(shop.Users).Exec(ctx)
	return err
}

// ---- unboundedread: reads that are bounded ----

func recentPosts(ctx context.Context) ([]shop.Post, error) {
	return shop.PostEntity.Query(db).Where(shop.PostUserID.Eq(1)).All(ctx)
}

// budgeted has neither Where nor Limit, and needs neither: the entity
// carries a Budget whose MaxRows renders the LIMIT for it.
func budgeted(ctx context.Context) ([]shop.Post, error) {
	return shop.PostEntity.Query(db).All(ctx)
}

// lookupTable reads a table the schema has declared small.
func lookupTable(ctx context.Context) ([]shop.Currency, error) {
	return shop.CurrencyEntity.Query(db).All(ctx)
}

func lookupTableViaFind(ctx context.Context) error {
	var cs []shop.Currency
	return db.Find(shop.Currencies).All(ctx, &cs)
}

// regionsAreSmall reads a table whose directive sits on the var spec
// rather than on the group around it.
func regionsAreSmall(ctx context.Context) ([]shop.Region, error) {
	return shop.RegionEntity.Query(db).All(ctx)
}

func firstPage(ctx context.Context) ([]shop.User, error) {
	return shop.UserEntity.Query(db).Limit(100).All(ctx)
}

// oneRow asks for a row, not a table.
func oneRow(ctx context.Context) (shop.User, error) {
	return shop.UserEntity.Query(db).One(ctx)
}

// streamed hands rows back one at a time, which is what Stream is for
// and why pg.Budget lets it past MaxRows too.
func streamed(ctx context.Context) error {
	return shop.UserEntity.Query(db).Stream(ctx, func(u *shop.User) error { return nil })
}

// counted asks the server for one number.
func counted(ctx context.Context) (int64, error) {
	return db.Select().From(shop.Users).Count(ctx)
}

// projected names its columns, so it is not a read of whole rows and
// the rule does not look at it.
func projected(ctx context.Context) error {
	var totals []struct{ N int64 }
	return db.Select(pg.CountAll()).From(shop.Users).All(ctx, &totals)
}

// exported is a full scan on purpose, once a night.
//
//drops:lint ignore unboundedread
func exported(ctx context.Context) ([]shop.User, error) {
	return shop.UserEntity.Query(db).All(ctx)
}

// relationFiltered is bounded by a predicate all the same: WhereHas
// renders WHERE EXISTS (SELECT 1 FROM posts ...), so this is a query
// with a WHERE, not a read of every row.
func relationFiltered(ctx context.Context) ([]shop.User, error) {
	return shop.UserEntity.Query(db).WhereHas("posts", func(q *pg.RelQuery) {}).All(ctx)
}

// relationFilteredFind is the same predicate on the FindBuilder.
func relationFilteredFind(ctx context.Context) error {
	var us []shop.User
	return db.Find(shop.Users).WhereHas("posts", func(q *pg.RelQuery) {}).All(ctx, &us)
}

// absenceIsAlsoAPredicate: WHERE NOT EXISTS narrows a result set as
// surely as WHERE EXISTS does.
func absenceIsAlsoAPredicate(ctx context.Context) ([]shop.User, error) {
	return shop.UserEntity.Query(db).WhereDoesntHave("posts", func(q *pg.RelQuery) {}).All(ctx)
}

// scratchTable reads all of a table built inside the function. It is
// the shape every dialect's own tests are written in, and the rule
// stays quiet on it deliberately: the analyzer cannot know how big an
// anonymous table is, and nobody can hang a lookup directive or a
// Budget on one, so the finding would have no fix.
func scratchTable(ctx context.Context) error {
	t := pg.NewTable("scratch")
	pg.Add(t, pg.BigSerial("id").PrimaryKey())
	var rows []struct {
		ID int64 `drop:"id"`
	}
	return db.Find(t).All(ctx, &rows)
}

// ---- loopload: loops that are not N+1 ----

// buildThenRun assembles the eager loads in a loop and runs the query
// once, outside it. Nothing here happens per iteration.
func buildThenRun(ctx context.Context, names []string) ([]shop.User, error) {
	q := shop.UserEntity.Query(db).Where(shop.UserID.Gt(0))
	for _, name := range names {
		q = q.With(name)
	}
	return q.All(ctx)
}

// loadOnceThenLoop does the eager load before the loop, which is the
// fix the rule asks for.
func loadOnceThenLoop(ctx context.Context) (int, error) {
	users, err := shop.UserEntity.Query(db).Where(shop.UserID.Gt(0)).With("posts").All(ctx)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, u := range users {
		n += len(u.Posts)
	}
	return n, nil
}

// perIDNoLoad queries in a loop without eager-loading anything. That
// may still be a repeated query, but it is pg.WithN1Detector's finding
// at run time, not this rule's at build time.
func perIDNoLoad(ctx context.Context, ids []int64) error {
	for _, id := range ids {
		if _, err := shop.UserEntity.Query(db).Where(shop.UserID.Eq(id)).One(ctx); err != nil {
			return err
		}
	}
	return nil
}

// pageThrough walks the table a page at a time. The eager load runs
// once per page, which is the point of paging, not an N+1.
func pageThrough(ctx context.Context) error {
	for off := int64(0); ; off += 100 {
		batch, err := shop.UserEntity.Query(db).
			With("posts").
			Limit(100).
			Offset(off).
			All(ctx)
		if err != nil {
			return err
		}
		if len(batch) < 100 {
			return nil
		}
	}
}

// keysetPage walks the table a page at a time without an Offset: each
// pass asks for the rows after the last one it saw. drops' own cursor
// API pages this way, and the relation load runs once per page exactly
// as it does under Offset paging.
func keysetPage(ctx context.Context) error {
	last := int64(0)
	for {
		batch, err := shop.UserEntity.Query(db).
			Where(shop.UserID.Gt(last)).
			With("posts").
			OrderBy(shop.UserID).
			Limit(100).
			All(ctx)
		if err != nil {
			return err
		}
		if len(batch) < 100 {
			return nil
		}
		last = batch[len(batch)-1].ID
	}
}
