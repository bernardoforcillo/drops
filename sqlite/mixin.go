package sqlite

import "github.com/bernardoforcillo/drops"

// Mixin is the richer companion of the plain template functions. While a
// template function only contributes columns, a Mixin can also register
// lifecycle hooks and default filters in a single Apply call.
//
//	sqlite.ApplyMixins(Users,
//	    sqlite.UUIDPrimaryKeyMixin{},
//	    &sqlite.TimestampsMixin{},  // adds timestamps + bumps updatedAt on UPDATE
//	    &sqlite.SoftDeleteMixin{},  // adds deletedAt, default-scopes queries,
//	                                // and rewrites DELETE into UPDATE
//	)
type Mixin interface {
	Apply(*Table)
}

// MixinFunc adapts a plain function to the Mixin interface.
type MixinFunc func(*Table)

// Apply implements Mixin.
func (f MixinFunc) Apply(t *Table) { f(t) }

// ApplyMixins runs each mixin against t in order and returns t. Later
// mixins observe the columns / hooks installed by earlier ones.
func ApplyMixins(t *Table, mixins ...Mixin) *Table {
	for _, m := range mixins {
		m.Apply(t)
	}
	return t
}

// TimestampsMixin registers "createdAt" / "updatedAt" columns and an
// UpdateHook that bumps updatedAt to CURRENT_TIMESTAMP on every UPDATE
// the caller hasn't already touched. INSERT relies on the column DEFAULT.
type TimestampsMixin struct {
	Cols TimestampsCols
}

// Apply implements Mixin.
func (m *TimestampsMixin) Apply(t *Table) {
	m.Cols = Timestamps(t)
	updatedAt := m.Cols.UpdatedAt.Column
	t.OnUpdate(UpdateHookFunc(func(ctx *UpdateHookCtx) {
		ctx.SetExpr(updatedAt, drops.Raw("CURRENT_TIMESTAMP"))
	}))
}

// SoftDeleteMixin registers a "deletedAt" column, a filter named
// [FilterSoftDelete] (deletedAt IS NULL), and a DeleteHook that
// rewrites DELETE as UPDATE deletedAt = CURRENT_TIMESTAMP — the row
// stays but is hidden by default.
//
// To read the hidden rows, name the guard:
// IgnoreFilters(sqlite.FilterSoftDelete), which leaves every other
// filter on the table in force. Unscoped() also works and drops all of
// them.
type SoftDeleteMixin struct {
	Cols SoftDeleteCols
}

// Apply implements Mixin.
func (m *SoftDeleteMixin) Apply(t *Table) {
	// SoftDelete has already registered the FilterSoftDelete guard;
	// the mixin only adds the DELETE rewrite on top of it.
	m.Cols = SoftDelete(t)
	deletedAt := m.Cols.DeletedAt.Column
	t.OnDelete(DeleteHookFunc(func(d *DeleteBuilder) drops.Expression {
		upd := d.DB().Update(d.Table()).
			Set(&exprBinding{col: deletedAt, expr: drops.Raw("CURRENT_TIMESTAMP")})
		for _, w := range d.Wheres() {
			upd = upd.Where(w)
		}
		return upd
	}))
}

// AuditMixin registers nullable "createdBy" / "updatedBy" foreign keys
// to target (typically a users.id PK) and exposes typed handles. The
// values are populated by the caller; the mixin does not infer the
// current user.
type AuditMixin[T any] struct {
	Target *Col[T]
	Cols   AuditCols[T]
}

// Apply implements Mixin.
func (m *AuditMixin[T]) Apply(t *Table) {
	m.Cols = Audit(t, m.Target)
}

// UUIDPrimaryKeyMixin registers a TEXT "id" PRIMARY KEY column defaulting
// to a random v4 UUID.
type UUIDPrimaryKeyMixin struct {
	Cols UUIDPrimaryKeyCols
}

// Apply implements Mixin.
func (m *UUIDPrimaryKeyMixin) Apply(t *Table) {
	m.Cols = UUIDPrimaryKey(t)
}
