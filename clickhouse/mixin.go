package clickhouse

import "github.com/bernardoforcillo/drops"

// Mixin is the richer companion of the plain template functions in
// template.go. ClickHouse exposes only InsertHook and SelectBuilder
// default filters — UPDATE/DELETE happen via async ALTERs and are
// not modelled by builders — so the rich mixins here are more
// limited than in drops/pg.
//
//	clickhouse.ApplyMixins(Events,
//	    clickhouse.UUIDPrimaryKeyMixin{},
//	    clickhouse.TimestampsMixin{},  // adds createdAt, updatedAt
//	    clickhouse.SoftDeleteMixin{},  // adds deletedAt + default scope
//	)
type Mixin interface {
	Apply(*Table)
}

// MixinFunc adapts a plain function to the Mixin interface.
type MixinFunc func(*Table)

// Apply implements Mixin.
func (f MixinFunc) Apply(t *Table) { f(t) }

// ApplyMixins runs each mixin against t in order and returns t.
func ApplyMixins(t *Table, mixins ...Mixin) *Table {
	for _, m := range mixins {
		m.Apply(t)
	}
	return t
}

// ----------------------------------------------------------------------
// Built-in rich mixins
// ----------------------------------------------------------------------

// TimestampsMixin registers "createdAt" and "updatedAt" DateTime
// columns. ClickHouse has no UPDATE builder so there is no UpdateHook
// equivalent; both columns get DEFAULT now() so INSERT without an
// explicit value picks up the server clock.
type TimestampsMixin struct {
	Cols TimestampsCols
}

// Apply implements Mixin.
func (m *TimestampsMixin) Apply(t *Table) {
	m.Cols = Timestamps(t)
}

// SoftDeleteMixin registers a Nullable(DateTime) "deletedAt" column
// and a default filter that excludes already-deleted rows from
// SELECTs. ClickHouse has no UPDATE/DELETE builder, so the "soft
// delete the row" operation is left to the caller as a raw ALTER
// TABLE … UPDATE statement.
type SoftDeleteMixin struct {
	Cols SoftDeleteCols
}

// Apply implements Mixin.
func (m *SoftDeleteMixin) Apply(t *Table) {
	m.Cols = SoftDelete(t)
	t.DefaultFilter(drops.ExprFunc(func(b *drops.Builder) {
		m.Cols.DeletedAt.Column.WriteSQL(b)
		b.WriteString(" IS NULL")
	}))
}

// AuditMixin registers "createdBy" and "updatedBy" columns of the
// same SQL type as Target. ClickHouse has no foreign-key enforcement,
// so the columns are plain scalars.
type AuditMixin[T any] struct {
	Target *Col[T]
	Cols   AuditCols[T]
}

// Apply implements Mixin.
func (m *AuditMixin[T]) Apply(t *Table) {
	m.Cols = Audit(t, m.Target)
}

// UUIDPrimaryKeyMixin registers an "id" UUID column defaulting to
// generateUUIDv4(). Pair the column with the table's ORDER BY
// (or PRIMARY KEY clause on MergeTree-family engines) to make it
// the row identifier.
type UUIDPrimaryKeyMixin struct {
	Cols UUIDPrimaryKeyCols
}

// Apply implements Mixin.
func (m *UUIDPrimaryKeyMixin) Apply(t *Table) {
	m.Cols = UUIDPrimaryKey(t)
}
