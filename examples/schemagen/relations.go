package schemagen

import "github.com/bernardoforcillo/drops/pg"

// The relations, declared once. Everything an eager load needs to
// know about how these tables are joined is here, and the shapes in
// schemagen_drops_rels.go are derived from it — which is the point:
// the field name, the slice, the dropRel tag and the nested type are
// four things that have to agree with this declaration, and three of
// them fail silently when they do not.
//
// All six kinds are declared, because the field type is the decision
// the generator makes and it makes a different one for each: a slice
// for HasMany, ManyToMany and MorphMany, a pointer for HasOne and
// BelongsTo, an interface for MorphTo. A kind that is declared here
// but never loaded from a generated struct is a decision nothing
// checks, so the integration suite loads every one of them.
//
// The blank identifiers are because a relation is registered on the
// table by the call itself; the returned handle is only useful when
// a query wants to name a relation as a symbol rather than a string,
// which is what Users.Rel("posts") is for.
//
//go:generate go run github.com/bernardoforcillo/drops/cmd/dropsgen -rels . -shape users:posts -shape users:profile -shape users:notes -shape posts:author -shape posts:author.posts -shape posts:tags -shape notes:owner
var (
	_ = pg.NewRelations(Users).
		HasMany("posts", Posts, UserID, PostUserID).
		HasOne("profile", Profiles, UserID, ProfileUserID).
		MorphMany("notes", Notes, NoteOwnerType, NoteOwnerID, UserID, morphUsers)
	_ = pg.NewRelations(Posts).
		BelongsTo("author", Users, PostUserID, UserID).
		ManyToMany("tags", Tags, PostTags, PostTagPostID, PostTagTagID, PostID, TagID)
	_ = pg.NewRelations(Notes).MorphTo("owner", NoteOwnerType, NoteOwnerID, morphs)
)

// morphUsers is the discriminator a note carries when it hangs off a
// user. It is a constant rather than two string literals because the
// MorphMany and the MorphTo have to agree on it and nothing else
// would notice if they stopped.
const morphUsers = "users"

// morphs binds that discriminator to the table and to the Go struct
// the loader scans a row of it into.
//
// The struct is User — the hand-written one, not the generated
// UsersRow — and that is forced rather than chosen: `dropsgen -rows`
// evaluates this package by compiling it with its own output moved
// aside, so a declaration here that named UsersRow would be one the
// generator could never get past. A MorphTo's row type has to be a
// type that exists before anything is generated.
var morphs = pg.RegisterMorph[User](pg.NewMorphMap(), morphUsers, Users)
