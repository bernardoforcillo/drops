package pg

// RelationKind identifies the cardinality of a Relation.
type RelationKind int

const (
	// HasMany: the parent owns zero or more children. The relation is
	// loaded as a slice on the parent struct.
	HasManyKind RelationKind = iota
	// HasOne: the parent owns at most one child. The relation is loaded
	// as a struct or pointer-to-struct on the parent.
	HasOneKind
	// BelongsTo: this row references one parent. The relation is loaded
	// as a struct or pointer-to-struct.
	BelongsToKind
	// ManyToMany: parent and target are linked through a junction table.
	// The relation is loaded as a slice on the parent struct.
	ManyToManyKind
)

// Relation describes a foreign-key relationship between two tables.
//
//	HasMany / HasOne: From is the parent, To is the child.
//	  ParentKey is the column on From (typically the PK).
//	  ChildKey  is the FK column on To pointing back at ParentKey.
//
//	BelongsTo: From is the child, To is the parent.
//	  ChildKey  is the FK column on From.
//	  ParentKey is the column on To pointed at by ChildKey.
//
//	ManyToMany: From and To are joined through a junction Table.
//	  ParentKey  is the column on From (typically the PK).
//	  ChildKey   is the column on To (typically the PK).
//	  Through    is the junction table.
//	  ThroughFK1 is the FK on Through pointing back to ParentKey.
//	  ThroughFK2 is the FK on Through pointing to ChildKey.
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

// Relations is the registration handle for one table. Use NewRelations
// followed by HasMany/HasOne/BelongsTo to declare relations:
//
//	pg.NewRelations(Users).
//	    HasMany("posts", Posts, UserID, PostUserID).
//	    HasOne("profile", Profiles, UserID, ProfileUserID)
//
//	pg.NewRelations(Posts).
//	    BelongsTo("author", Users, PostUserID, UserID)
type Relations struct {
	t *Table
}

// NewRelations begins relation declarations for t. The returned handle
// stores its declarations on t directly.
func NewRelations(t *Table) *Relations {
	if t.relations == nil {
		t.relations = map[string]*Relation{}
	}
	return &Relations{t: t}
}

// HasMany declares a one-to-many relation.
//
//	parentKey: column on the current table (usually the PK).
//	childFK:   FK column on `child` pointing at parentKey.
func (r *Relations) HasMany(name string, child *Table, parentKey, childFK ColRef) *Relations {
	r.t.relations[name] = &Relation{
		Name:      name,
		Kind:      HasManyKind,
		From:      r.t,
		To:        child,
		ParentKey: parentKey.col(),
		ChildKey:  childFK.col(),
	}
	return r
}

// HasOne declares a one-to-one relation. The cardinality is enforced by
// data conventions, not by SQL — it loads at most one row per parent.
func (r *Relations) HasOne(name string, child *Table, parentKey, childFK ColRef) *Relations {
	r.t.relations[name] = &Relation{
		Name:      name,
		Kind:      HasOneKind,
		From:      r.t,
		To:        child,
		ParentKey: parentKey.col(),
		ChildKey:  childFK.col(),
	}
	return r
}

// BelongsTo declares a many-to-one relation.
//
//	childFK:   FK column on the current table.
//	parentKey: column on `parent` pointed at by childFK.
func (r *Relations) BelongsTo(name string, parent *Table, childFK, parentKey ColRef) *Relations {
	r.t.relations[name] = &Relation{
		Name:      name,
		Kind:      BelongsToKind,
		From:      r.t,
		To:        parent,
		ChildKey:  childFK.col(),
		ParentKey: parentKey.col(),
	}
	return r
}

// ManyToMany declares a many-to-many relation through a junction table.
//
//	target:        the related table
//	through:       the junction table
//	throughLocal:  FK on through pointing back at the current table
//	throughRemote: FK on through pointing at the target table
//	localKey:      key on the current table (typically the PK)
//	targetKey:     key on the target table (typically the PK)
//
// Loading runs two queries: one to fetch the junction rows for the
// parent IDs, one to fetch the target rows for the unique remote IDs.
// The relation field on the parent struct must be a slice.
func (r *Relations) ManyToMany(
	name string,
	target *Table,
	through *Table,
	throughLocal, throughRemote ColRef,
	localKey, targetKey ColRef,
) *Relations {
	r.t.relations[name] = &Relation{
		Name:       name,
		Kind:       ManyToManyKind,
		From:       r.t,
		To:         target,
		ParentKey:  localKey.col(),
		ChildKey:   targetKey.col(),
		Through:    through,
		ThroughFK1: throughLocal.col(),
		ThroughFK2: throughRemote.col(),
	}
	return r
}
