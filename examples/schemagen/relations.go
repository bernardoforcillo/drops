package schemagen

import "github.com/bernardoforcillo/drops/pg"

// The relations, declared once. Everything an eager load needs to
// know about how users and posts are joined is here, and the shapes
// in schemagen_drops_rels.go are derived from it — which is the
// point: the field name, the slice, the dropRel tag and the nested
// type are four things that have to agree with this declaration, and
// three of them fail silently when they do not.
//
// The blank identifiers are because a relation is registered on the
// table by the call itself; the returned handle is only useful when
// a query wants to name a relation as a symbol rather than a string,
// which is what Users.Rel("posts") is for.
//
//go:generate go run github.com/bernardoforcillo/drops/cmd/dropsgen -rels . -shape users:posts -shape posts:author -shape posts:author.posts
var (
	_ = pg.NewRelations(Users).HasMany("posts", Posts, UserID, PostUserID)
	_ = pg.NewRelations(Posts).BelongsTo("author", Users, PostUserID, UserID)
)
