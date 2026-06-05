package pg

import (
	"context"
	"errors"
	"reflect"
)

// COPY is the only sane way to move > 100k rows into PostgreSQL —
// the wire protocol is column-oriented, parameter limits don't
// apply, and the server skips most of the per-row overhead INSERT
// pays. For analytics ingestion, ETL backfills and seed loading,
// COPY is typically 10–50× faster than INSERT.
//
// drops imports no concrete driver, so the COPY path is opt-in via
// duck typing on drops.Driver. The pgx stdlib bridge exposes
// CopyFrom on its Conn; adapt it in ~10 lines:
//
//	type pgxCopier struct { conn *pgx.Conn }
//	func (c pgxCopier) Copy(ctx context.Context, table string,
//	    cols []string, rows [][]any) (int64, error) {
//	    return c.conn.CopyFrom(ctx, pgx.Identifier{table}, cols,
//	        pgx.CopyFromRows(rows))
//	}
//
// Compose pgxCopier with the regular drops.Driver implementation
// (embedding works) and drops' CopyFrom helper picks it up
// automatically:
//
//	n, err := pg.CopyFrom(db, ctx, UserEntity, rows)
//
// For drivers that don't expose CopyFrom, drops returns
// ErrCopyNotSupported and the caller falls back to CreateMany.

// Copier is the contract drops uses to dispatch a bulk COPY. Any
// driver that satisfies it (typically by adding a Copy method to
// the existing drops.Driver implementation) gets the fast path
// for free.
type Copier interface {
	// Copy ingests rows into table. cols carries the destination
	// columns in order; each entry in rows must align with cols.
	// Returns the number of rows accepted by the server.
	Copy(ctx context.Context, table string, cols []string, rows [][]any) (int64, error)
}

// ErrCopyNotSupported is returned by CopyFrom when the underlying
// driver does not satisfy Copier. Callers should fall back to
// Entity.CreateMany / UpsertMany.
var ErrCopyNotSupported = errors.New("drops/pg: driver does not implement Copier — fall back to CreateMany")

// CopyFrom bulk-loads rs into the entity's table via the driver's
// COPY path. Returns the number of rows accepted. Validators run
// per row before any bytes hit the wire — bad input aborts the
// whole batch before the server is touched.
//
// Bypasses the cache (COPY-loaded rows never populate it), the
// audit log (audit per row would defeat the bandwidth advantage),
// and the per-row INSERT hooks. Use Entity.CreateMany /
// UpsertMany when those guarantees matter; use CopyFrom when
// raw throughput is the point.
func CopyFrom[T any](db *DB, ctx context.Context, ent *Entity[T], rs []T) (int64, error) {
	if len(rs) == 0 {
		return 0, ErrNoRowsToInsert
	}
	c, ok := db.Driver().(Copier)
	if !ok {
		return 0, ErrCopyNotSupported
	}
	for i := range rs {
		if err := ent.runValidators(&rs[i]); err != nil {
			return 0, err
		}
	}
	cols := make([]string, 0, len(ent.colFields))
	for _, cf := range ent.colFields {
		cols = append(cols, cf.col.Name())
	}
	data := make([][]any, len(rs))
	for i := range rs {
		v := reflect.ValueOf(&rs[i]).Elem()
		row := make([]any, len(ent.colFields))
		for j, cf := range ent.colFields {
			row[j] = v.FieldByIndex(cf.field).Interface()
		}
		data[i] = row
	}
	return c.Copy(ctx, ent.table.Name(), cols, data)
}

// SupportsCopy reports whether the driver behind db exposes the
// Copier interface. Useful for code paths that want to choose
// between CopyFrom and CreateMany at runtime.
func SupportsCopy(db *DB) bool {
	_, ok := db.Driver().(Copier)
	return ok
}
