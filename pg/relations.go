package pg

import "reflect"

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
	// MorphTo: a polymorphic belongs-to — each row references a parent
	// in one of several possible tables, identified by a type
	// discriminator column. The relation field is typed `any` and
	// holds a pointer to the loaded parent struct after eager loading.
	MorphToKind
	// MorphMany: the inverse of MorphTo — fetches every child whose
	// morph_type equals a fixed value and morph_id equals the parent's
	// PK. The relation is loaded as a slice on the parent struct.
	MorphManyKind
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

	// Polymorphic fields, populated for MorphToKind / MorphManyKind.
	// MorphTypeCol is the discriminator column.
	// MorphMap maps the discriminator value to a target table + Go
	// struct type (MorphToKind only).
	// MorphType is the discriminator value identifying this side of
	// the polymorphic edge (MorphManyKind only).
	MorphTypeCol *Column
	MorphMap     *MorphMap
	MorphType    string
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

// MorphTo declares a polymorphic belongs-to relation. The current
// table has two columns — typeCol holding a discriminator string and
// idCol holding the target's primary-key value. Each entry registered
// on morphs binds a discriminator value to a target table and the
// Go struct used to scan its rows.
//
//	morphs := pg.NewMorphMap()
//	pg.RegisterMorph[User](morphs, "users", Users)
//	pg.RegisterMorph[Post](morphs, "posts", Posts)
//
//	pg.NewRelations(Comments).
//	    MorphTo("commentable", CommentCommentableType, CommentCommentableID, morphs)
//
// The relation field on the child struct must be declared `any` (or
// any interface type) — the loader stores a *Parent pointer into it
// and the caller dispatches with a type switch.
func (r *Relations) MorphTo(name string, typeCol, idCol ColRef, morphs *MorphMap) *Relations {
	r.t.relations[name] = &Relation{
		Name:         name,
		Kind:         MorphToKind,
		From:         r.t,
		ChildKey:     idCol.col(),
		MorphTypeCol: typeCol.col(),
		MorphMap:     morphs,
	}
	return r
}

// MorphMany declares the inverse of MorphTo: every row in `child`
// whose typeCol equals typeValue and whose idCol equals the parent's
// PK becomes a member of the parent's named relation.
//
//	pg.NewRelations(Users).
//	    MorphMany("comments", Comments,
//	        CommentCommentableType, CommentCommentableID,
//	        UserID, "users")
//
// typeValue is the literal string the child's typeCol carries when it
// points at this side of the morph — typically the parent's table
// name in the MorphMap registered on the MorphTo side.
func (r *Relations) MorphMany(
	name string,
	child *Table,
	childTypeCol, childIDCol ColRef,
	parentKey ColRef,
	typeValue string,
) *Relations {
	r.t.relations[name] = &Relation{
		Name:         name,
		Kind:         MorphManyKind,
		From:         r.t,
		To:           child,
		ParentKey:    parentKey.col(),
		ChildKey:     childIDCol.col(),
		MorphTypeCol: childTypeCol.col(),
		MorphType:    typeValue,
	}
	return r
}

// MorphMap binds a polymorphic discriminator string to the target
// table and Go struct it identifies. Build it once and share it
// between the MorphTo declarations that need it.
type MorphMap struct {
	entries map[string]morphEntry
}

type morphEntry struct {
	table   *Table
	rowType reflect.Type
}

// NewMorphMap returns an empty MorphMap.
func NewMorphMap() *MorphMap {
	return &MorphMap{entries: map[string]morphEntry{}}
}

// RegisterMorph binds value to a target table and the Go struct type
// T used to scan that table's rows. It is a free function rather than
// a method because Go does not allow generic methods.
func RegisterMorph[T any](m *MorphMap, value string, t *Table) *MorphMap {
	var zero T
	rt := reflect.TypeOf(zero)
	for rt.Kind() == reflect.Ptr {
		rt = rt.Elem()
	}
	m.entries[value] = morphEntry{table: t, rowType: rt}
	return m
}

// lookup returns the entry registered for value, or ok=false.
func (m *MorphMap) lookup(value string) (morphEntry, bool) {
	if m == nil {
		return morphEntry{}, false
	}
	e, ok := m.entries[value]
	return e, ok
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
