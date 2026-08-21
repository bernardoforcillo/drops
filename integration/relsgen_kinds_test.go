package integration_test

import (
	"context"
	"strings"
	"testing"

	"github.com/bernardoforcillo/drops/examples/schemagen"
	"github.com/bernardoforcillo/drops/integration"
	"github.com/bernardoforcillo/drops/pg"
)

// Why the generated relation field carries drop:"-" as well as
// dropRel, against a live server.
//
// The scanner claims a field's own name and its camelCase form, and a
// relation field sits at depth 0 — above the real column promoted out
// of the embedded row struct. A table with a column named like one of
// its relations is legal, and until the column meets the slice
// nothing says otherwise. The generator's own test pins the two tags
// as bytes; only a server hands back a row that proves what the
// second one is for.
func TestPGGeneratedShapeKeepsTheRelationFieldOutOfTheScan(t *testing.T) {
	db := openPG(t)
	ctx := context.Background()

	users := pg.NewTable(integration.UniqueName(t, "probe_users"))
	uid := pg.Add(users, pg.BigSerial("id").PrimaryKey())
	uposts := pg.Add(users, pg.Integer("posts").NotNull())
	posts := pg.NewTable(integration.UniqueName(t, "probe_posts"))
	pid := pg.Add(posts, pg.BigSerial("id").PrimaryKey())
	puid := pg.Add(posts, pg.BigInt("userId").NotNull())
	_ = pid
	pg.NewRelations(users).HasMany("posts", posts, uid, puid)

	dropPG(t, db, posts)
	dropPG(t, db, users)
	execPG(t, db, pg.CreateTable(users))
	execPG(t, db, pg.CreateTable(posts))

	var back struct {
		ID int64 `drop:"id"`
	}
	if err := db.Insert(users).Row(uposts.Val(7)).Returning(uid).One(ctx, &back); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Insert(posts).Row(puid.Val(back.ID)).Exec(ctx); err != nil {
		t.Fatal(err)
	}

	type usersRow struct {
		ID    int64 `drop:"id"`
		Posts int32 `drop:"posts"`
	}
	type postsRow struct {
		ID     int64 `drop:"id"`
		UserID int64 `drop:"userId"`
	}
	// What the generator emits.
	type generated struct {
		usersRow
		Posts []postsRow `drop:"-" dropRel:"posts"`
	}
	// The same without drop:"-".
	type unguarded struct {
		usersRow
		Posts []postsRow `dropRel:"posts"`
	}

	var good []generated
	if err := db.Find(users).With("posts").All(ctx, &good); err != nil {
		t.Fatalf("generated shape: %v", err)
	}
	if len(good) != 1 || good[0].usersRow.Posts != 7 || len(good[0].Posts) != 1 {
		t.Fatalf("generated shape scanned wrong: %+v", good)
	}

	var bad []unguarded
	err := db.Find(users).With("posts").All(ctx, &bad)
	t.Logf("without drop:\"-\": err=%v rows=%+v", err, bad)
	if err == nil && len(bad) == 1 && bad[0].usersRow.Posts == 7 {
		t.Errorf("drop:\"-\" made no difference here — the claim it is load-bearing is unsupported")
	}
}

// The shapes the generator does not emit, refused by the loader.
//
// Every kind's *positive* case now loads a generated struct against a
// live server — see relsgen_test.go, which reaches all six. What
// cannot be asserted from generated code is the other half of the
// same decision: that binding a relation to the wrong kind of field
// is refused rather than quietly filled with something. A generator
// that chose the field type by preference rather than by what the
// loader requires would still pass every test above; it fails here.
func TestPGLoaderRefusesTheShapesTheGeneratorDoesNotEmit(t *testing.T) {
	db := openPG(t)
	ctx := context.Background()
	f := relsgenTables(t, db)

	owner := insertUser(t, db, f, "owner@example.com", "Owner")
	post := insertPostReturning(t, db, f, owner, "post")
	insertProfile(t, db, f, owner, "bio")
	insertPostTag(t, db, f, post, insertTag(t, db, f, "go"))
	insertNote(t, db, f, relsgenMorphUsers, owner, "note")

	for _, tc := range []struct {
		what string
		load func() error
	}{
		{"a HasMany bound to a pointer", func() error {
			var rows []struct {
				schemagen.UsersRow
				Posts *schemagen.PostsRow `drop:"-" dropRel:"posts"`
			}
			return db.Find(f.Users).With("posts").All(ctx, &rows)
		}},
		{"a HasOne bound to a slice", func() error {
			var rows []struct {
				schemagen.UsersRow
				Profile []schemagen.ProfilesRow `drop:"-" dropRel:"profile"`
			}
			return db.Find(f.Users).With("profile").All(ctx, &rows)
		}},
		{"a ManyToMany bound to a pointer", func() error {
			var rows []struct {
				schemagen.PostsRow
				Tags *schemagen.TagsRow `drop:"-" dropRel:"tags"`
			}
			return db.Find(f.Posts).With("tags").All(ctx, &rows)
		}},
		{"a MorphMany bound to a pointer", func() error {
			var rows []struct {
				schemagen.UsersRow
				Notes *schemagen.NotesRow `drop:"-" dropRel:"notes"`
			}
			return db.Find(f.Users).With("notes").All(ctx, &rows)
		}},
		{"a MorphTo bound to a concrete pointer", func() error {
			var rows []struct {
				schemagen.NotesRow
				Owner *schemagen.UsersRow `drop:"-" dropRel:"owner"`
			}
			return db.Find(f.Notes).With("owner").All(ctx, &rows)
		}},
	} {
		if err := tc.load(); err == nil {
			t.Errorf("%s was accepted", tc.what)
		} else {
			t.Logf("%s: %v", tc.what, err)
		}
	}
}

// What the dropRel tag is for, which no other live test in this suite
// could tell you.
//
// Every generated shape names its relation field after the relation,
// so pg/find.go's case-insensitive name fallback would have filled it
// with the tag deleted, and every one of those tests would still
// pass. The tag only becomes load-bearing when the field is named
// something else — so that is what this loads.
func TestPGRelationIsBoundByItsTagAndNotByTheFieldName(t *testing.T) {
	db := openPG(t)
	ctx := context.Background()
	f := relsgenTables(t, db)

	author := insertUser(t, db, f, "author@example.com", "Author")
	insertPost(t, db, f, author, "first")

	// "articles" is not a relation this schema declares, and Articles
	// is not a name the fallback could reach "posts" from. Only the
	// tag connects them.
	type shapeWithARenamedField struct {
		schemagen.UsersRow
		Articles []schemagen.PostsRow `drop:"-" dropRel:"posts"`
	}
	var tagged []shapeWithARenamedField
	if err := db.Find(f.Users).With("posts").All(ctx, &tagged); err != nil {
		t.Fatalf("a field bound by its tag was refused: %v", err)
	}
	if len(tagged) != 1 || len(tagged[0].Articles) != 1 || tagged[0].Articles[0].Title != "first" {
		t.Fatalf("the tagged field did not load: %+v", tagged)
	}

	// The same struct with the tag removed. Nothing else changes, and
	// the relation no longer has anywhere to go.
	type shapeWithoutTheTag struct {
		schemagen.UsersRow
		Articles []schemagen.PostsRow `drop:"-"`
	}
	var untagged []shapeWithoutTheTag
	err := db.Find(f.Users).With("posts").All(ctx, &untagged)
	if err == nil {
		t.Fatalf("the same field without the tag loaded anyway: %+v", untagged)
	}
	if !strings.Contains(err.Error(), "dropRel") {
		t.Errorf("the refusal does not name the tag that was missing: %v", err)
	}
}

// And the cost of the fallback existing at all, which is why the
// generator writes the tag on every field rather than relying on the
// name.
//
// A field is claimed for a relation because it is *named* like one,
// whether or not anybody meant it as a relation field. The struct
// below declares no relation — no dropRel anywhere — and the loader
// fills it regardless, overwriting whatever the caller had put there.
// The same rule reaches through embedding into the row struct, where
// the fields are columns.
func TestPGRelationFallbackClaimsAFieldThatOnlySharesTheName(t *testing.T) {
	db := openPG(t)
	ctx := context.Background()
	f := relsgenTables(t, db)

	author := insertUser(t, db, f, "author@example.com", "Author")
	insertPost(t, db, f, author, "first")

	// Posts here is the caller's own field — a cache, a projection,
	// anything. It carries no dropRel and declares no relation.
	type notMeantAsARelation struct {
		schemagen.UsersRow
		Posts []schemagen.PostsRow
	}
	var rows []notMeantAsARelation
	if err := db.Find(f.Users).With("posts").All(ctx, &rows); err != nil {
		t.Fatalf("find: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("loaded %d users, want 1", len(rows))
	}
	if len(rows[0].Posts) != 1 {
		t.Fatalf("the fallback no longer fills an untagged field — pg/find.go's doc says it does: %+v", rows[0])
	}
	t.Logf("an untagged field named after the relation was filled with %d rows", len(rows[0].Posts))
}
