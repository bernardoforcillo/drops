package integration_test

import (
	"context"
	"testing"

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

// The four cardinalities the rest of the live suite never reaches.
//
// examples/schemagen declares a HasMany and a BelongsTo, so those two
// are loaded from generated structs elsewhere in this file's
// neighbour. HasOne, ManyToMany, MorphMany and MorphTo are the other
// four the generator decides a field type for, and the decision is
// worth nothing unless pg/find.go agrees. The structs below are
// written by hand in exactly the shape cmd/dropsgen emits — the
// generator test TestRelsShapeFollowsTheLoader pins that text — so
// what this asserts is the other half: that the loader accepts that
// shape and fills it, including the parent that matches nothing.
func TestPGLoaderFillsEveryShapeTheGeneratorEmits(t *testing.T) {
	db := openPG(t)
	ctx := context.Background()

	users := pg.NewTable(integration.UniqueName(t, "pr_users"))
	uid := pg.Add(users, pg.BigSerial("id").PrimaryKey())
	uname := pg.Add(users, pg.Text("name").NotNull())

	profiles := pg.NewTable(integration.UniqueName(t, "pr_profiles"))
	prid := pg.Add(profiles, pg.BigSerial("id").PrimaryKey())
	pruid := pg.Add(profiles, pg.BigInt("userId").NotNull())
	prbio := pg.Add(profiles, pg.Text("bio").NotNull())

	tags := pg.NewTable(integration.UniqueName(t, "pr_tags"))
	tid := pg.Add(tags, pg.BigSerial("id").PrimaryKey())
	tlabel := pg.Add(tags, pg.Text("label").NotNull())

	userTags := pg.NewTable(integration.UniqueName(t, "pr_user_tags"))
	utu := pg.Add(userTags, pg.BigInt("userId").NotNull())
	utt := pg.Add(userTags, pg.BigInt("tagId").NotNull())

	notes := pg.NewTable(integration.UniqueName(t, "pr_notes"))
	nid := pg.Add(notes, pg.BigSerial("id").PrimaryKey())
	nbody := pg.Add(notes, pg.Text("body").NotNull())
	ntyp := pg.Add(notes, pg.Text("ownerType").NotNull())
	nown := pg.Add(notes, pg.BigInt("ownerId").NotNull())
	_ = nid

	type usersRow struct {
		ID   int64  `drop:"id"`
		Name string `drop:"name"`
	}
	morphs := pg.NewMorphMap()
	pg.RegisterMorph[usersRow](morphs, "users", users)

	pg.NewRelations(users).
		HasOne("profile", profiles, uid, pruid).
		ManyToMany("tags", tags, userTags, utu, utt, uid, tid).
		MorphMany("notes", notes, ntyp, nown, uid, "users")
	pg.NewRelations(notes).MorphTo("owner", ntyp, nown, morphs)

	for _, tbl := range []*pg.Table{notes, userTags, tags, profiles, users} {
		dropPG(t, db, tbl)
	}
	for _, tbl := range []*pg.Table{users, profiles, tags, userTags, notes} {
		execPG(t, db, pg.CreateTable(tbl))
	}

	var back struct {
		ID int64 `drop:"id"`
	}
	if err := db.Insert(users).Row(uname.Val("full")).Returning(uid).One(ctx, &back); err != nil {
		t.Fatal(err)
	}
	full := back.ID
	if err := db.Insert(users).Row(uname.Val("bare")).Returning(uid).One(ctx, &back); err != nil {
		t.Fatal(err)
	}
	bare := back.ID

	if _, err := db.Insert(profiles).Row(pruid.Val(full), prbio.Val("bio")).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	if err := db.Insert(tags).Row(tlabel.Val("go")).Returning(tid).One(ctx, &back); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Insert(userTags).Row(utu.Val(full), utt.Val(back.ID)).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Insert(notes).Row(nbody.Val("note"), ntyp.Val("users"), nown.Val(full)).Exec(ctx); err != nil {
		t.Fatal(err)
	}
	_ = prid

	type profilesRow struct {
		ID     int64  `drop:"id"`
		UserID int64  `drop:"userId"`
		Bio    string `drop:"bio"`
	}
	type tagsRow struct {
		ID    int64  `drop:"id"`
		Label string `drop:"label"`
	}
	type notesRow struct {
		ID        int64  `drop:"id"`
		Body      string `drop:"body"`
		OwnerType string `drop:"ownerType"`
		OwnerID   int64  `drop:"ownerId"`
	}
	// Exactly the shape -rels emits for these three kinds.
	type usersShape struct {
		usersRow
		Notes   []notesRow   `drop:"-" dropRel:"notes"`
		Profile *profilesRow `drop:"-" dropRel:"profile"`
		Tags    []tagsRow    `drop:"-" dropRel:"tags"`
	}
	var rows []usersShape
	if err := db.Find(users).With("profile", "tags", "notes").OrderBy(uname.Asc()).All(ctx, &rows); err != nil {
		t.Fatalf("hasOne/manyToMany/morphMany shape refused by the loader: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("loaded %d rows", len(rows))
	}
	bareRow, fullRow := rows[0], rows[1]
	if fullRow.Name != "full" || bareRow.Name != "bare" {
		t.Fatalf("ordering: %+v", rows)
	}
	if fullRow.Profile == nil || fullRow.Profile.Bio != "bio" {
		t.Errorf("HasOne pointer did not load: %+v", fullRow.Profile)
	}
	if bareRow.Profile != nil {
		t.Errorf("HasOne with no match is not nil: %+v", bareRow.Profile)
	}
	if len(fullRow.Tags) != 1 || fullRow.Tags[0].Label != "go" {
		t.Errorf("ManyToMany slice did not load: %+v", fullRow.Tags)
	}
	if bareRow.Tags == nil {
		t.Errorf("ManyToMany with no match came back nil, not empty")
	}
	if len(fullRow.Notes) != 1 || fullRow.Notes[0].Body != "note" {
		t.Errorf("MorphMany slice did not load: %+v", fullRow.Notes)
	}
	if bareRow.Notes == nil {
		t.Errorf("MorphMany with no match came back nil, not empty")
	}

	// And the MorphTo side: the generator emits `any`.
	type notesShape struct {
		notesRow
		Owner any `drop:"-" dropRel:"owner"`
	}
	var ns []notesShape
	if err := db.Find(notes).With("owner").All(ctx, &ns); err != nil {
		t.Fatalf("morphTo shape refused by the loader: %v", err)
	}
	if len(ns) != 1 {
		t.Fatalf("loaded %d notes", len(ns))
	}
	owner, ok := ns[0].Owner.(*usersRow)
	if !ok {
		t.Fatalf("MorphTo any holds %T, not a *usersRow", ns[0].Owner)
	}
	if owner.ID != full {
		t.Errorf("MorphTo loaded user %d, want %d", owner.ID, full)
	}
	_ = bare

	// And the other half of "the shape follows the loader": the
	// shapes the generator does not emit are refused rather than
	// quietly filled, which is what makes the choice above a decision
	// and not a preference.
	type morphManyAsPointer struct {
		usersRow
		Notes *notesRow `drop:"-" dropRel:"notes"`
	}
	var wrongMany []morphManyAsPointer
	if err := db.Find(users).With("notes").All(ctx, &wrongMany); err == nil {
		t.Error("a MorphMany bound to a pointer was accepted")
	} else {
		t.Logf("MorphMany as a pointer: %v", err)
	}

	type morphToAsPointer struct {
		notesRow
		Owner *usersRow `drop:"-" dropRel:"owner"`
	}
	var wrongMorph []morphToAsPointer
	if err := db.Find(notes).With("owner").All(ctx, &wrongMorph); err == nil {
		t.Error("a MorphTo bound to a concrete pointer was accepted")
	} else {
		t.Logf("MorphTo as a concrete pointer: %v", err)
	}

	type manyToManyAsPointer struct {
		usersRow
		Tags *tagsRow `drop:"-" dropRel:"tags"`
	}
	var wrongM2M []manyToManyAsPointer
	if err := db.Find(users).With("tags").All(ctx, &wrongM2M); err == nil {
		t.Error("a ManyToMany bound to a pointer was accepted")
	} else {
		t.Logf("ManyToMany as a pointer: %v", err)
	}
}
