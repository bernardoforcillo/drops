package mysql

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
// "idempotency key" used by payment APIs; every endpoint that mutates
// money or emits events needs one.
//
//	store := mysql.NewIdempotencyStore(db, "idempotency_keys", 24*time.Hour)
//
//	// In the request handler:
//	raw, err := store.Run(ctx, requestKey, func(tx *mysql.DB) ([]byte, error) {
//	    if err := PaymentEntity.Create(tx, ctx, &p); err != nil {
//	        return nil, err
//	    }
//	    return json.Marshal(map[string]any{"paymentId": p.ID})
//	})
//
// On the FIRST call with a given key the closure runs and its response
// is stored; subsequent calls with the same key skip the closure
// entirely and return the cached bytes. A failed closure rolls back
// the row, so the next attempt with the same key re-executes — exactly
// what callers expect.
//
// Concurrent calls with the same key are serialised by SELECT … FOR
// UPDATE: late arrivals wait for the in-flight callback to commit,
// then observe its response. That works under MySQL's REPEATABLE READ
// default because a locking read is a *current* read — it sees the
// winner's committed row rather than the snapshot the transaction
// started from, which a plain SELECT could not promise. The wait
// window is bounded by the caller's context and by the server's
// innodb_lock_wait_timeout, whichever is shorter.
type IdempotencyStore struct {
	db    *DB
	table string
	ttl   time.Duration
	now   func() time.Time
}

// NewIdempotencyStore returns a store bound to db. table is the SQL
// identifier of the keys table (create one via NewIdempotencyTable);
// ttl is how long records survive before Cleanup is allowed to reclaim
// them.
func NewIdempotencyStore(db *DB, table string, ttl time.Duration) *IdempotencyStore {
	if table == "" {
		table = "idempotency_keys"
	}
	if ttl <= 0 {
		ttl = 24 * time.Hour
	}
	return &IdempotencyStore{db: db, table: table, ttl: ttl, now: time.Now}
}

// Table returns the keys table's name.
func (s *IdempotencyStore) Table() string { return s.table }

// NewIdempotencyTable declares the canonical idempotency table.
//
// The key column is a VARCHAR under the binary collation for a reason
// that is closer to a security property than a preference: MySQL's
// default collation is case-insensitive, so under it a request keyed
// "A1b2" would be served the response stored for "a1b2" — one client's
// answer handed to another. See [NewOutboxTable] on the collation, and
// note the one thing it does not fix: every VARCHAR collation on these
// servers is PAD SPACE, so a key ending in a space is the same key
// without it. Do not let one.
//
// The response is a LONGBLOB rather than PostgreSQL's unbounded bytea.
// A plain BLOB caps at 64KB, and a response that outgrows it is
// rejected outright under a strict sql_mode and *silently truncated*
// without one — an idempotent replay quietly returning a corrupted
// body is the worst failure this table can have. The remaining limit
// is the server's max_allowed_packet, which no column type can widen.
func NewIdempotencyTable(name string) *Table {
	t := NewTable(name)
	// "key" is a reserved word in MySQL; every reference drops emits
	// is backticked, which is what makes the plain spelling usable.
	Add(t, streamKeyColumn("key").PrimaryKey())
	Add(t, LongBlob("response"))
	Add(t, Boolean("completed").NotNull().Default("0"))
	Add(t, Timestamp("createdAt", false).NotNull())
	Add(t, Timestamp("expiresAt", false).NotNull())
	return t
}

// ErrEmptyKey is returned by Run when key is the empty string. An
// empty key would collapse every operation onto the same row, which is
// almost always a bug.
var ErrEmptyKey = errors.New("drops/mysql: idempotency key cannot be empty")

// Run executes fn under key. The first invocation runs fn; later
// invocations with the same key return fn's previously-stored response
// without re-running it. A non-nil error from fn rolls back the claim
// so a subsequent call with the same key retries.
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
		// Reserve the key. The self-assignment is MySQL's "insert or
		// do nothing" that still takes the row lock, and it is
		// deliberately not INSERT IGNORE: IGNORE downgrades *every*
		// error on the statement to a warning — a truncated key, a
		// missing table's column — and swallowing those here would
		// turn a broken deployment into a store that quietly never
		// caches anything.
		expires := s.now().UTC().Add(s.ttl)
		insertSQL := fmt.Sprintf(
			"INSERT INTO %s (`key`, `createdAt`, `expiresAt`) VALUES (?, ?, ?)\n"+
				"ON DUPLICATE KEY UPDATE `key` = `key`", Dialect.QuoteIdent(s.table))
		if _, err := tx.Exec(ctx, insertSQL, key, s.now().UTC(), expires); err != nil {
			return err
		}

		// Lock the row for the duration of fn so concurrent callers
		// queue behind us. A completed entry returns its cached
		// response immediately.
		selSQL := fmt.Sprintf("SELECT `response`, `completed` FROM %s WHERE `key` = ? FOR UPDATE", Dialect.QuoteIdent(s.table))
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

		// Fresh attempt — run the closure inside the tx.
		out, err := fn(tx)
		if err != nil {
			return err
		}
		updateSQL := fmt.Sprintf("UPDATE %s SET `response` = ?, `completed` = 1 WHERE `key` = ?", Dialect.QuoteIdent(s.table))
		if _, err := tx.Exec(ctx, updateSQL, out, key); err != nil {
			return err
		}
		result = out
		return nil
	})
	return result, err
}

// RunJSON wraps Run with JSON marshalling on both sides. fn returns a
// typed result; Run handles the serialisation. Subsequent calls
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

// Cleanup removes records whose expiresAt has passed, returning the
// number of rows reclaimed. The cutoff is bound as a parameter rather
// than written as NOW(): the column is a DATETIME, which carries no
// zone, so comparing it against the server's clock would be wrong by
// the server's time_zone offset rather than merely imprecise.
func (s *IdempotencyStore) Cleanup(ctx context.Context) (int64, error) {
	sql := fmt.Sprintf("DELETE FROM %s WHERE `expiresAt` < ?", Dialect.QuoteIdent(s.table))
	res, err := s.db.Exec(ctx, sql, s.now().UTC())
	if err != nil {
		return 0, err
	}
	if res == nil {
		return 0, nil
	}
	return res.RowsAffected()
}

// SweepEvery launches a background goroutine that calls Cleanup every
// interval until ctx is cancelled. Errors are forwarded to onError
// when supplied.
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
