package schemagen

import "github.com/bernardoforcillo/drops/pg"

// Schema is the convention the tools read: a function that takes
// nothing and returns the tables this package wants managed.
//
// A CLI cannot read a Go variable, and a table declaration is one. So
// `drops` and `dropsgen -rows` both compile a throwaway program that
// imports this package, calls this function, and prints what it finds
// — the real compiler against the real declarations, which is the
// only answer that matches the one the application gets.
//
// It is also the boundary: a table this package declares but does not
// register here is deliberately invisible to both tools, which is how
// a table another tool owns stays out of drops's way.
//
//go:generate go run github.com/bernardoforcillo/drops/cmd/dropsgen -rows .
func Schema() *pg.Schema {
	return pg.NewSchema(Users, Posts, Profiles, Tags, PostTags, Notes)
}
