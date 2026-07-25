package sqlite

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// IdempotencyStore stamps a (response, completed) record per key so
// retries of the same logical operation observe the original result
// instead of executing again — the canonical "idempotency key" pattern
// for mutating API endpoints.
//
//	store := sqlite.NewIdempotencyStore(db, "idempotency_keys", 24*time.Hour)
//	raw, err := store.Run(ctx, requestKey, func(tx *sqlite.DB) ([]byte, error) {
//	    if err := PaymentEntity.Create(tx, ctx, &p); err != nil {
//	        return nil, err
//	    }
//	    return json.Marshal(map[string]any{"paymentId": p.ID})
//	})
//
// The first call with a given key runs the closure and stores its
// response; later calls with the same key skip the closure and return
// the cached bytes. A failed closure rolls back the claim so the next
// attempt re-executes.
//
// SQLite has no SELECT ... FOR UPDATE, but its write transactions
// serialise: the INSERT below takes the database write lock, so
// concurrent Run calls with the same key queue at the transaction
// boundary and the late arrivals observe the committed response.
type IdempotencyStore struct {
	db    *DB
	table string
	ttl   time.Duration
	now   func() time.Time
}

// NewIdempotencyStore returns a store bound to db. table is the keys
// table (create one via NewIdempotencyTable); ttl is how long records
// survive before Cleanup may reclaim them.
func NewIdempotencyStore(db *DB, table string, ttl time.Duration) *IdempotencyStore {
	if table == "" {
		table = "idempotency_keys"
	}
	if ttl <= 0 {
		ttl = 24 * time.Hour
	}
	return &IdempotencyStore{db: db, table: table, ttl: ttl, now: time.Now}
}

// NewIdempotencyTable declares the canonical idempotency table.
func NewIdempotencyTable(name string) *Table {
	t := NewTable(name)
	Add(t, Text("key").PrimaryKey())
	Add(t, Blob("response"))
	Add(t, Boolean("completed").NotNull().Default("0"))
	Add(t, Timestamp("createdAt", false).NotNull().Default("CURRENT_TIMESTAMP"))
	Add(t, Timestamp("expiresAt", false).NotNull())
	return t
}

// ErrEmptyKey is returned by Run for an empty key — it would collapse
// every operation onto one row.
var ErrEmptyKey = errors.New("drops/sqlite: idempotency key cannot be empty")

// Run executes fn under key. The first invocation runs fn and stores
// its response; later invocations with the same key return the stored
// bytes without re-running. A non-nil error rolls the claim back.
func (s *IdempotencyStore) Run(ctx context.Context, key string, fn func(tx *DB) ([]byte, error)) ([]byte, error) {
	if key == "" {
		return nil, ErrEmptyKey
	}
	var result []byte
	err := s.db.InTx(ctx, func(tx *DB) error {
		expires := s.now().Add(s.ttl)
		insertSQL := fmt.Sprintf(
			`INSERT OR IGNORE INTO %s ("key", "expiresAt") VALUES (?, ?)`,
			quoteIdent(s.table))
		if _, err := tx.Exec(ctx, insertSQL, key, expires); err != nil {
			return err
		}

		selSQL := fmt.Sprintf(`SELECT "response", "completed" FROM %s WHERE "key" = ?`, quoteIdent(s.table))
		rows, err := tx.Query(ctx, selSQL, key)
		if err != nil {
			return err
		}
		var response []byte
		var completed int64
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

		if completed != 0 {
			result = response
			return nil
		}

		out, err := fn(tx)
		if err != nil {
			return err
		}
		updateSQL := fmt.Sprintf(
			`UPDATE %s SET "response" = ?, "completed" = 1 WHERE "key" = ?`,
			quoteIdent(s.table))
		if _, err := tx.Exec(ctx, updateSQL, out, key); err != nil {
			return err
		}
		result = out
		return nil
	})
	return result, err
}

// RunJSON wraps Run with JSON marshalling on both sides.
func RunJSON[T any](s *IdempotencyStore, ctx context.Context, key string, fn func(tx *DB) (T, error)) (T, error) {
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

// Cleanup removes records whose expiresAt has passed. The cutoff is
// bound as a parameter (not CURRENT_TIMESTAMP) so both sides of the
// comparison share the driver's time encoding.
func (s *IdempotencyStore) Cleanup(ctx context.Context) (int64, error) {
	sql := fmt.Sprintf(`DELETE FROM %s WHERE "expiresAt" < ?`, quoteIdent(s.table))
	res, err := s.db.Exec(ctx, sql, s.now())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// SweepEvery launches a goroutine calling Cleanup every interval until
// ctx is cancelled. Errors go to onError when supplied.
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
