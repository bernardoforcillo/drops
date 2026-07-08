package sqlite

// Relations declaration layer, mirroring drops/pg. A Relation names an
// association between two tables so Find().With(name) can eager-load it
// with a single batched query per edge (implemented in find.go).

// RelationKind enumerates the supported association shapes.
type RelationKind int

const (
	// HasMany: parent has many children; the child holds the FK.
	HasMany RelationKind = iota
	// HasOne: parent has one child; the child holds the FK.
	HasOne
	// BelongsTo: this table holds the FK pointing at the parent.
	BelongsTo
	// ManyToMany: association through a junction table.
	ManyToMany
)

// Relation describes one association edge.
type Relation struct {
	Name   string
	Kind   RelationKind
	Target *Table

	// Local / Foreign are the join columns for HasMany / HasOne /
	// BelongsTo: Local lives on the owning table, Foreign on Target.
	Local   *Column
	Foreign *Column

	// ManyToMany junction wiring: Through is the junction table;
	// ThroughLocal / ThroughForeign are its FK columns pointing at the
	// owning table and the target respectively; LocalKey / TargetKey
	// are the referenced keys on the owning and target tables.
	Through        *Table
	ThroughLocal   *Column
	ThroughForeign *Column
	LocalKey       *Column
	TargetKey      *Column
}

// RelationsBuilder declares relations on a table fluently.
type RelationsBuilder struct{ t *Table }

// NewRelations begins declaring relations on t.
func NewRelations(t *Table) *RelationsBuilder { return &RelationsBuilder{t: t} }

// HasMany declares that t has many Target rows, joined local == foreign
// (foreign is the FK column on Target).
func (b *RelationsBuilder) HasMany(name string, target *Table, local, foreign ColRef) *RelationsBuilder {
	b.t.setRelation(name, &Relation{
		Name: name, Kind: HasMany, Target: target,
		Local: local.col(), Foreign: foreign.col(),
	})
	return b
}

// HasOne declares a one-to-one where Target holds the FK.
func (b *RelationsBuilder) HasOne(name string, target *Table, local, foreign ColRef) *RelationsBuilder {
	b.t.setRelation(name, &Relation{
		Name: name, Kind: HasOne, Target: target,
		Local: local.col(), Foreign: foreign.col(),
	})
	return b
}

// BelongsTo declares that t holds the FK (local) pointing at Target's
// key (foreign).
func (b *RelationsBuilder) BelongsTo(name string, target *Table, local, foreign ColRef) *RelationsBuilder {
	b.t.setRelation(name, &Relation{
		Name: name, Kind: BelongsTo, Target: target,
		Local: local.col(), Foreign: foreign.col(),
	})
	return b
}

// ManyToMany declares an association through a junction table.
// throughLocal / throughForeign are the junction FK columns pointing at
// the owning table and target; localKey / targetKey are the referenced
// keys on the owning and target tables.
func (b *RelationsBuilder) ManyToMany(name string, target, through *Table, throughLocal, throughForeign, localKey, targetKey ColRef) *RelationsBuilder {
	b.t.setRelation(name, &Relation{
		Name: name, Kind: ManyToMany, Target: target, Through: through,
		ThroughLocal: throughLocal.col(), ThroughForeign: throughForeign.col(),
		LocalKey: localKey.col(), TargetKey: targetKey.col(),
	})
	return b
}
