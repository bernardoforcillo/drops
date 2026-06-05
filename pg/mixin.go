package pg

import "github.com/bernardoforcillo/drops"

// Mixin is the richer companion of the plain template functions in
// template.go. While a template function (Timestamps, SoftDelete, ...)
// only contributes columns, a Mixin can also register indexes,
// foreign keys, lifecycle hooks, and default filters in a single
// Apply call.
//
// The two styles compose freely:
//
//	pg.ApplyMixins(Users,
//	    pg.UUIDPrimaryKeyMixin{},
//	    pg.TimestampsMixin{},   // adds timestamps + bumps updatedAt on UPDATE
//	    pg.SoftDeleteMixin{},   // adds deletedAt, default-scopes queries,
//	                            // and rewrites DELETE into UPDATE
//	)
//
// External libraries follow the same recipe: define a struct that
// holds the typed handles it will expose, implement Apply, and
// register whatever side effects the template needs.
type Mixin interface {
	Apply(*Table)
}

// MixinFunc adapts a plain function to the Mixin interface — useful
// when a template doesn't need its own state.
type MixinFunc func(*Table)

// Apply implements Mixin.
func (f MixinFunc) Apply(t *Table) { f(t) }

// ApplyMixins runs each mixin against t in order and returns t.
// Mixins applied later observe the columns / hooks installed by
// earlier mixins.
func ApplyMixins(t *Table, mixins ...Mixin) *Table {
	for _, m := range mixins {
		m.Apply(t)
	}
	return t
}

// ----------------------------------------------------------------------
// Built-in rich mixins
// ----------------------------------------------------------------------

// TimestampsMixin registers "createdAt" and "updatedAt" columns and
// an UpdateHook that bumps updatedAt to now() on every UPDATE the
// caller hasn't already touched. INSERT is left to the column's
// DEFAULT now() — no hook needed.
//
// After Apply, the embedded Cols field exposes typed handles to the
// columns so the rest of the application can reference them.
type TimestampsMixin struct {
	Cols TimestampsCols
}

// Apply implements Mixin.
func (m *TimestampsMixin) Apply(t *Table) {
	m.Cols = Timestamps(t)
	updatedAt := m.Cols.UpdatedAt.Column
	t.OnUpdate(UpdateHookFunc(func(ctx *UpdateHookCtx) {
		ctx.SetExpr(updatedAt, drops.Raw("now()"))
	}))
}

// SoftDeleteMixin registers a "deletedAt" column, a DefaultFilter
// (deletedAt IS NULL), and a DeleteHook that rewrites DELETE
// statements as UPDATE deletedAt = now() — i.e. the row stays in
// the table but is hidden by default. Use Unscoped() on any builder
// to bypass the guard and operate on every row (including the
// already-deleted ones).
type SoftDeleteMixin struct {
	Cols SoftDeleteCols
}

// Apply implements Mixin.
func (m *SoftDeleteMixin) Apply(t *Table) {
	m.Cols = SoftDelete(t)
	deletedAt := m.Cols.DeletedAt.Column
	t.DefaultFilter(IsNull(deletedAt))
	t.OnDelete(DeleteHookFunc(func(d *DeleteBuilder) drops.Expression {
		upd := d.DB().Update(d.Table()).
			Set(&exprBinding{col: deletedAt, expr: drops.Raw("now()")})
		for _, w := range d.Wheres() {
			upd = upd.Where(w)
		}
		for _, r := range d.ReturningClauses() {
			upd = upd.Returning(r)
		}
		return upd
	}))
}

// AuditMixin registers nullable "createdBy" / "updatedBy" foreign
// keys to target — typically a users.id PK — and exposes typed
// handles to the rest of the application. Audit information is
// populated by the caller (e.g. via context-aware middleware); the
// mixin itself does not infer the current user.
type AuditMixin[T any] struct {
	Target *Col[T]
	Cols   AuditCols[T]
}

// Apply implements Mixin.
func (m *AuditMixin[T]) Apply(t *Table) {
	m.Cols = Audit(t, m.Target)
}

// UUIDPrimaryKeyMixin registers an "id" UUID PRIMARY KEY column
// defaulting to gen_random_uuid().
type UUIDPrimaryKeyMixin struct {
	Cols UUIDPrimaryKeyCols
}

// Apply implements Mixin.
func (m *UUIDPrimaryKeyMixin) Apply(t *Table) {
	m.Cols = UUIDPrimaryKey(t)
}
