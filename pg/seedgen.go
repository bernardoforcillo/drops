package pg

import (
	"context"
	"fmt"
	"reflect"
)

// Generated seed data, shaped by the relations already declared.
//
// [SeedAdd] takes rows the caller wrote out. That is the right tool for
// a fixture with three named users in it and the wrong one for a table
// that needs to look populated, where writing the rows out is the work.
// What replaces it is a count, a template, and — because the
// interesting half of a schema is its edges — a way to say "each of
// these, with two to five of those":
//
//	seeder := pg.NewSeeder(db).WithSeed(42)
//
//	authors := pg.SeedMany(seeder, AuthorEntity, 10, func(g *pg.Gen, a *Author) {
//	    a.Name  = g.FullName()
//	    a.Email = g.Email()
//	})
//	pg.SeedRelated(authors, "books", BookEntity, 2, 5, func(g *pg.Gen, b *Book, a *Author) {
//	    b.Title     = g.Sentence()
//	    b.Published = g.Chance(0.7)
//	})
//
//	if err := seeder.Apply(ctx); err != nil { ... }
//
// Nothing in the second call restates how a book points at an author.
// The relation was declared once, with NewRelations, and that
// declaration is what SeedRelated reads: it takes the parent key off
// the author it just inserted and writes it into the book's foreign-key
// field. A schema whose relations are right cannot be seeded with the
// keys wrong, and a schema whose relations change does not leave a
// stale copy of the old wiring in the seed file.
//
// The whole run is drawn from the Seeder's [Gen], so the same seed
// produces the same rows — counts within the two-to-five range
// included. [Seeder.Apply] rewinds the Gen first, so applying twice
// against a clean database gives the same data twice.

// SeedSet is the handle to a group of generated rows. Hang related
// groups off it with [SeedRelated]; read what was created back with
// Rows after Apply.
type SeedSet[T any] struct {
	group *seedGroup[T]
}

// Rows returns the rows the last Apply created for this group, in
// creation order and carrying whatever the insert read back — the
// generated primary key above all, which is what makes them useful to
// assert against.
//
// Before Apply it is empty.
func (s *SeedSet[T]) Rows() []T { return s.group.rows }

// Entity returns the entity this group inserts through.
func (s *SeedSet[T]) Entity() *Entity[T] { return s.group.ent }

// seedRunner is one node of the plan, type-erased so a parent can hold
// children of a different row type.
type seedRunner interface {
	// run creates this node's rows beneath parent — an addressable
	// struct Value, or the zero Value at the root — and recurses.
	run(db *DB, ctx context.Context, g *Gen, parent reflect.Value) error
	// reset clears the accumulated rows so a second Apply reports what
	// that Apply created rather than both runs at once.
	reset()
}

type seedGroup[T any] struct {
	ent *Entity[T]

	// n is the count at the root; lo/hi are the per-parent range for a
	// related group, which related distinguishes. It is a flag of its
	// own rather than "rel != nil" because a relation this plan could
	// not resolve is still a related group — one that has to reach its
	// wiring in order to report why.
	n       int
	lo, hi  int
	related bool
	rel     *Relation

	// fill is the caller's template, closed over the parent's type.
	fill func(g *Gen, row *T, parent reflect.Value)
	// wire writes the relation's keys onto the row. nil at the root.
	wire func(parent reflect.Value, row *T) error

	children []seedRunner
	rows     []T
}

func (s *seedGroup[T]) reset() {
	s.rows = nil
	for _, c := range s.children {
		c.reset()
	}
}

func (s *seedGroup[T]) run(db *DB, ctx context.Context, g *Gen, parent reflect.Value) error {
	n := s.n
	if s.related {
		// Drawn from the same Gen as everything else, so the shape of
		// the tree is as reproducible as the values in it.
		n = g.IntRange(s.lo, s.hi)
	}
	for i := 0; i < n; i++ {
		row := new(T)
		if s.fill != nil {
			s.fill(g, row, parent)
		}
		// After the template, not before: the relation is the thing
		// this call exists to get right, and a template that happens to
		// touch the foreign key should not be able to undo it silently.
		if s.wire != nil {
			if err := s.wire(parent, row); err != nil {
				return err
			}
		}
		// Create rather than CreateMany, per row: the children below
		// need this row's generated key, and so does anything asserting
		// on Rows. Seeding is not a hot path, and a batch insert that
		// hands back no keys would make the relation wiring impossible.
		if err := s.ent.Create(db, ctx, row); err != nil {
			return err
		}
		s.rows = append(s.rows, *row)
		if len(s.children) == 0 {
			continue
		}
		pv := reflect.ValueOf(row).Elem()
		for _, c := range s.children {
			if err := c.run(db, ctx, g, pv); err != nil {
				return err
			}
		}
	}
	return nil
}

// seedTreeOp adapts a plan root to the Seeder's op list.
type seedTreeOp struct {
	s    *Seeder
	root seedRunner
}

func (o *seedTreeOp) run(db *DB, ctx context.Context) error {
	return o.root.run(db, ctx, o.s.Gen(), reflect.Value{})
}

func (o *seedTreeOp) reset() { o.root.reset() }

// SeedMany registers n generated rows for ent. fill receives the
// Seeder's Gen and a zero row to populate; it may be nil, which seeds n
// rows of whatever the table defaults to.
//
// The returned handle is what [SeedRelated] hangs children off, and
// what reports the created rows after Apply.
func SeedMany[T any](s *Seeder, ent *Entity[T], n int, fill func(*Gen, *T)) *SeedSet[T] {
	group := &seedGroup[T]{ent: ent, n: n}
	if fill != nil {
		group.fill = func(g *Gen, row *T, _ reflect.Value) { fill(g, row) }
	}
	s.ops = append(s.ops, &seedTreeOp{s: s, root: group})
	return &SeedSet[T]{group: group}
}

// SeedRelated registers between lo and hi rows of ent for every row the
// parent group creates, wiring the foreign key from the named relation.
//
//	pg.SeedRelated(authors, "books", BookEntity, 2, 5, func(g *pg.Gen, b *Book, a *Author) {
//	    b.Title = g.Sentence()
//	})
//
// The relation must be declared on the parent's table and must be a
// HasMany, a HasOne or a MorphMany — the kinds where the child holds
// the key. A BelongsTo points the other way, and its parent has to
// exist before the child does; seed that side first and hang this one
// off it. A ManyToMany needs junction rows of its own, which are rows
// like any other: seed them with their own entity.
//
// fill receives the parent that this row will belong to, so a child can
// be generated in terms of it. The relation's own key columns are
// written after fill returns.
//
// The count is drawn per parent, so ten authors with 2..5 books each
// gives ten different numbers, not one number ten times.
func SeedRelated[P, C any](
	parent *SeedSet[P], name string, ent *Entity[C], lo, hi int,
	fill func(g *Gen, row *C, parent *P),
) *SeedSet[C] {
	rel := parent.group.ent.Table().Relation(name)
	return seedRelated(parent, rel, name, ent, lo, hi, fill)
}

// SeedRelatedRel is [SeedRelated] taking a relation handle rather than
// a name, the way [FindBuilder.LoadRel] does.
func SeedRelatedRel[P, C any](
	parent *SeedSet[P], rel *Relation, ent *Entity[C], lo, hi int,
	fill func(g *Gen, row *C, parent *P),
) *SeedSet[C] {
	name := ""
	if rel != nil {
		name = rel.Name
	}
	return seedRelated(parent, rel, name, ent, lo, hi, fill)
}

func seedRelated[P, C any](
	parent *SeedSet[P], rel *Relation, name string, ent *Entity[C], lo, hi int,
	fill func(g *Gen, row *C, parent *P),
) *SeedSet[C] {
	group := &seedGroup[C]{ent: ent, lo: lo, hi: hi, related: true, rel: rel}
	if fill != nil {
		group.fill = func(g *Gen, row *C, pv reflect.Value) {
			fill(g, row, pv.Addr().Interface().(*P))
		}
	}
	group.wire = seedWire[P, C](rel, name, parent.group.ent.Table())
	parent.group.children = append(parent.group.children, group)
	return &SeedSet[C]{group: group}
}

// seedWire builds the closure that copies the parent's key onto the
// child, resolving the struct fields from the relation's columns with
// the same rules the relation loaders use.
//
// Every failure it can detect is detected here, at declaration, and
// returned from the first row it is asked to wire — a seed plan that
// cannot possibly work should say so on the first insert, not leave a
// table full of zero foreign keys behind.
func seedWire[P, C any](rel *Relation, name string, on *Table) func(reflect.Value, *C) error {
	fail := func(err error) func(reflect.Value, *C) error {
		return func(reflect.Value, *C) error { return err }
	}
	if rel == nil {
		return fail(fmt.Errorf("drops/pg: unknown relation %q on table %q", name, on.Name()))
	}
	switch rel.Kind {
	case HasManyKind, HasOneKind, MorphManyKind:
	default:
		return fail(fmt.Errorf("drops/pg: relation %q is not a HasMany, HasOne or MorphMany — only the side that holds the foreign key can be seeded beneath its parent",
			rel.Name))
	}

	parentType := reflect.TypeOf((*P)(nil)).Elem()
	childType := reflect.TypeOf((*C)(nil)).Elem()

	parentKey, ok := relationKeyField(parentType, rel.ParentKey)
	if !ok {
		return fail(fmt.Errorf("drops/pg: relation %q: parent struct %s missing field for column %q",
			rel.Name, parentType.Name(), rel.ParentKey.Name()))
	}
	childKey, ok := relationKeyField(childType, rel.ChildKey)
	if !ok {
		return fail(fmt.Errorf("drops/pg: relation %q: child struct %s missing field for column %q",
			rel.Name, childType.Name(), rel.ChildKey.Name()))
	}

	var morphField []int
	if rel.Kind == MorphManyKind {
		morphField, ok = relationKeyField(childType, rel.MorphTypeCol)
		if !ok {
			return fail(fmt.Errorf("drops/pg: relation %q: child struct %s missing field for the morph type column %q",
				rel.Name, childType.Name(), rel.MorphTypeCol.Name()))
		}
	}

	return func(pv reflect.Value, row *C) error {
		src := pv.FieldByIndex(parentKey)
		dst := reflect.ValueOf(row).Elem().FieldByIndex(childKey)
		switch {
		case src.Type() == dst.Type():
			dst.Set(src)
		case src.Type().ConvertibleTo(dst.Type()):
			dst.Set(src.Convert(dst.Type()))
		default:
			return fmt.Errorf("drops/pg: relation %q: cannot write %s key into %s field",
				rel.Name, src.Type(), dst.Type())
		}
		if morphField != nil {
			mv := reflect.ValueOf(row).Elem().FieldByIndex(morphField)
			if mv.Kind() != reflect.String {
				return fmt.Errorf("drops/pg: relation %q: morph type field is %s, want string",
					rel.Name, mv.Kind())
			}
			mv.SetString(rel.MorphType)
		}
		return nil
	}
}
