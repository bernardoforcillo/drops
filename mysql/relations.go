package mysql

import "fmt"

// RelationKind distinguishes the shapes a relation can take.
type RelationKind int

const (
	HasManyKind RelationKind = iota
	HasOneKind
	BelongsToKind
	ManyToManyKind
)

// Relation describes one declared edge between two tables.
type Relation struct {
	Name      string
	Kind      RelationKind
	From      *Table
	To        *Table
	ParentKey *Column
	ChildKey  *Column

	// Junction-table fields, populated only for ManyToManyKind.
	Through    *Table
	ThroughFK1 *Column
	ThroughFK2 *Column
}

// Relations is the registration handle for one table:
//
//	mysql.NewRelations(Users).
//	    HasMany("posts", Posts, UserID, PostUserID).
//	    HasOne("profile", Profiles, UserID, ProfileUserID)
type Relations struct{ t *Table }

// NewRelations begins relation declarations for t, storing them on the
// table itself.
func NewRelations(t *Table) *Relations {
	if t.relations == nil {
		t.relations = map[string]*Relation{}
	}
	return &Relations{t: t}
}

func (r *Relations) declare(rel *Relation) *Relations {
	if _, dup := r.t.relations[rel.Name]; dup {
		panic(fmt.Sprintf("drops/mysql: table %q already declares a relation named %q", r.t.name, rel.Name))
	}
	r.t.relations[rel.Name] = rel
	return r
}

// HasMany declares a one-to-many relation. parentKey is the column on
// this table (usually the primary key); childFK is the foreign key on
// child pointing back at it.
func (r *Relations) HasMany(name string, child *Table, parentKey, childFK ColRef) *Relations {
	return r.declare(&Relation{
		Name: name, Kind: HasManyKind, From: r.t, To: child,
		ParentKey: parentKey.col(), ChildKey: childFK.col(),
	})
}

// HasOne declares a one-to-one relation in the same direction as
// HasMany.
func (r *Relations) HasOne(name string, child *Table, parentKey, childFK ColRef) *Relations {
	return r.declare(&Relation{
		Name: name, Kind: HasOneKind, From: r.t, To: child,
		ParentKey: parentKey.col(), ChildKey: childFK.col(),
	})
}

// BelongsTo declares the inverse edge: childFK on this table points at
// parentKey on parent.
func (r *Relations) BelongsTo(name string, parent *Table, childFK, parentKey ColRef) *Relations {
	return r.declare(&Relation{
		Name: name, Kind: BelongsToKind, From: r.t, To: parent,
		ChildKey: childFK.col(), ParentKey: parentKey.col(),
	})
}

// ManyToMany declares a relation through a junction table. fk1 is the
// junction column pointing at this table, fk2 the one pointing at the
// far side.
func (r *Relations) ManyToMany(name string, target, through *Table, parentKey, fk1, fk2, targetKey ColRef) *Relations {
	return r.declare(&Relation{
		Name: name, Kind: ManyToManyKind, From: r.t, To: target,
		ParentKey: parentKey.col(), ChildKey: targetKey.col(),
		Through: through, ThroughFK1: fk1.col(), ThroughFK2: fk2.col(),
	})
}
