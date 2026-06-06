package pg

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// IdempotencyStore stamps a (response, completed) record per key so
// retries of the same logical operation observe the original result
// instead of executing again. The pattern is the canonical
// "idempotency key" used by payment APIs; every POST endpoint that
// mutates money or emits events needs one.
//
//	store := pg.NewIdempotencyStore(db, "idempotency_keys", 24*time.Hour)
//
//	// In the request handler:
//	raw, err := store.Run(ctx, requestKey, func(tx *pg.DB) ([]byte, error) {
//	    // mutate state inside the tx
//	    _, err := PaymentEntity.Create(tx, ctx, &p)
//	    if err != nil { return nil, err }
//	    return json.Marshal(map[string]any{"paymentId": p.ID})
//	})
//
// On the FIRST call with a given key the closure runs and its
// response is stored; subsequent calls with the same key skip the
// closure entirely and return the cached bytes. A failed closure
// rolls back the row, so the next attempt with the same key
// re-executes — exactly what callers expect.
//
// The store leverages SELECT ... FOR UPDATE to serialise concurrent
// calls with the same key: late arrivals wait for the in-flight
// callback to commit, then observe its response. The wait window is
// bounded by the caller's context.
type IdempotencyStore struct {
	db    *DB
	table string
	ttl   time.Duration
}

// NewIdempotencyStore returns a store bound to db. table is the SQL
// identifier of the keys table (create one via NewIdempotencyTable);
// ttl is how long records survive before Cleanup is allowed to
// reclaim them.
func NewIdempotencyStore(db *DB, table string, ttl time.Duration) *IdempotencyStore {
	if table == "" {
		table = "idempotency_keys"
	}
	if ttl <= 0 {
		ttl = 24 * time.Hour
	}
	return &IdempotencyStore{db: db, table: table, ttl: ttl}
}

// NewIdempotencyTable declares the canonical idempotency table. Add
// it to your schema:
//
//	schema := pg.NewSchema(Users, pg.NewIdempotencyTable("idempotency_keys"))
//	pg.Push(ctx, db, schema)
func NewIdempotencyTable(name string) *Table {
	t := NewTable(name)
	Add(t, Text("key").PrimaryKey())
	Add(t, Bytea("response"))
	Add(t, Boolean("completed").NotNull().Default("false"))
	Add(t, Timestamp("createdAt", true).NotNull().Default("now()"))
	Add(t, Timestamp("expiresAt", true).NotNull())
	return t
}

// ErrEmptyKey is returned by Run when key is the empty string. An
// empty key would collapse every operation onto the same row,
// which is almost always a bug.
var ErrEmptyKey = errors.New("drops/pg: idempotency key cannot be empty")

// Run executes fn under key. The first invocation runs fn; later
// invocations with the same key return fn's previously-stored
// response without re-running it. A non-nil error from fn rolls
// back the claim so a subsequent call with the same key retries.
func (s *IdempotencyStore) Run(
	ctx context.Context,
	key string,
	fn func(tx *DB) ([]byte, error),
) ([]byte, error) {
	if key == "" {
		return nil, ErrEmptyKey
	}
	var result []byte
	err := s.db.InTx(ctx, func(tx *DB) error {
		// Reserve the key. ON CONFLICT keeps the SQL idempotent so
		// the FOR UPDATE below always lands on a row whether we
		// just inserted it or it pre-existed.
		expires := time.Now().Add(s.ttl)
		insertSQL := fmt.Sprintf(`
			INSERT INTO %q ("key", "expiresAt") VALUES ($1, $2)
			ON CONFLICT ("key") DO NOTHING`, s.table)
		if _, err := tx.Exec(ctx, insertSQL, key, expires); err != nil {
			return err
		}

		// Lock the row for the duration of fn so concurrent callers
		// queue behind us. Stale-completed entries return their
		// cached response immediately.
		selSQL := fmt.Sprintf(`
			SELECT "response", "completed" FROM %q WHERE "key" = $1
			FOR UPDATE`, s.table)
		rows, err := tx.Query(ctx, selSQL, key)
		if err != nil {
			return err
		}
		var response []byte
		var completed bool
		if rows.Next() {
			if scanErr := rows.Scan(&response, &completed); scanErr != nil {
				rows.Close()
				return scanErr
			}
		}
		rows.Close()
		if cerr := rows.Err(); cerr != nil {
			return cerr
		}

		if completed {
			result = response
			return nil
		}

		// Fresh attempt — run the closure inside the tx.
		out, err := fn(tx)
		if err != nil {
			return err
		}
		updateSQL := fmt.Sprintf(`
			UPDATE %q SET "response" = $1, "completed" = true
			WHERE "key" = $2`, s.table)
		if _, err := tx.Exec(ctx, updateSQL, out, key); err != nil {
			return err
		}
		result = out
		return nil
	})
	return result, err
}

// RunJSON wraps Run with JSON marshalling on both sides. fn returns
// a typed result; Run handles the serialisation. Subsequent calls
// unmarshal the cached bytes back into T.
func RunJSON[T any](
	s *IdempotencyStore,
	ctx context.Context,
	key string,
	fn func(tx *DB) (T, error),
) (T, error) {
	var zero T
	raw, err := s.Run(ctx, key, func(tx *DB) ([]byte, error) {
		v, err := fn(tx)
		if err != nil {
			return nil, err
		}
		return json.Marshal(v)
	})
	if err != nil {
		return zero, err
	}
	if len(raw) == 0 {
		return zero, nil
	}
	var out T
	if err := json.Unmarshal(raw, &out); err != nil {
		return zero, err
	}
	return out, nil
}

// Cleanup removes records whose expiresAt has passed. Returns the
// number of rows reclaimed.
func (s *IdempotencyStore) Cleanup(ctx context.Context) (int64, error) {
	sql := fmt.Sprintf(`DELETE FROM %q WHERE "expiresAt" < now()`, s.table)
	res, err := s.db.Exec(ctx, sql)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// SweepEvery launches a background goroutine that calls Cleanup
// every interval until ctx is cancelled. Errors are forwarded to
// onError when supplied.
func (s *IdempotencyStore) SweepEvery(ctx context.Context, interval time.Duration, onError func(error)) {
	if interval <= 0 {
		interval = time.Hour
	}
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if _, err := s.Cleanup(ctx); err != nil && onError != nil {
					onError(err)
				}
			}
		}
	}()
}
