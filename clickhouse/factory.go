package clickhouse

import (
	"context"
	"sync/atomic"
)

// Factory builds and inserts test rows for an Entity. The template
// callback returns a fresh value each call; the supplied seq is a
// monotonically increasing counter the template can interpolate into
// fields that need to stay unique across a batch.
//
//	var EventFactory = clickhouse.NewFactory(EventEnt, func(seq int) Event {
//	    return Event{
//	        ID:     fmt.Sprintf("evt-%d", seq),
//	        UserID: uint64(seq),
//	        Kind:   "pageview",
//	        Ts:     time.Now(),
//	    }
//	})
//
//	// In a test
//	e, _  := EventFactory.Create(ctx, db)           // one row, inserted
//	es, _ := EventFactory.CreateN(ctx, db, 100)     // batch insert
//	pv := EventFactory.With(func(e *Event) { e.Kind = "purchase" })
//	p, _ := pv.Create(ctx, db)
//
// Factories are safe to share across goroutines — the sequence
// counter is atomic. Call Reset between subtests if you need
// stable identifiers.
type Factory[T any] struct {
	entity   *Entity[T]
	template func(seq int) T
	seq      *atomic.Int64
}

// NewFactory binds template to entity. The template fires once per
// Build / Create call; the seq it receives is the post-increment
// counter so the first invocation sees seq=1.
func NewFactory[T any](e *Entity[T], template func(seq int) T) *Factory[T] {
	return &Factory[T]{entity: e, template: template, seq: new(atomic.Int64)}
}

// Build returns a fresh value without touching the database.
func (f *Factory[T]) Build() T {
	n := int(f.seq.Add(1))
	return f.template(n)
}

// BuildN returns n freshly templated values without touching the
// database. Each value receives a distinct seq.
func (f *Factory[T]) BuildN(n int) []T {
	out := make([]T, n)
	for i := 0; i < n; i++ {
		out[i] = f.Build()
	}
	return out
}

// Create builds one value and inserts it. ClickHouse has no
// RETURNING so the returned T is the value as built — server-side
// defaults (e.g. DEFAULT expressions) are not reflected back.
func (f *Factory[T]) Create(ctx context.Context, db *DB) (T, error) {
	v := f.Build()
	_, err := f.entity.Create(db, ctx, &v)
	return v, err
}

// CreateN builds n values and inserts them in a single batch
// statement via Entity.CreateMany. Returns the templated values;
// server-side defaults are not read back (matches Entity.CreateMany's
// semantics).
func (f *Factory[T]) CreateN(ctx context.Context, db *DB, n int) ([]T, error) {
	rows := f.BuildN(n)
	if _, err := f.entity.CreateMany(db, ctx, rows); err != nil {
		return nil, err
	}
	return rows, nil
}

// With returns a child factory whose Build / Create calls run the
// parent template, then apply mutate to the result. Use it to spin
// off variant factories — different event kinds, user tiers, etc.
// — without redeclaring the whole template.
//
// Child factories share the parent's sequence counter so identifiers
// remain unique across both.
func (f *Factory[T]) With(mutate func(*T)) *Factory[T] {
	return &Factory[T]{
		entity: f.entity,
		template: func(seq int) T {
			v := f.template(seq)
			mutate(&v)
			return v
		},
		seq: f.seq,
	}
}

// Reset rewinds the sequence counter to zero. Typically called in
// test setup between subtests so identifiers stay stable from one
// run to the next.
func (f *Factory[T]) Reset() { f.seq.Store(0) }

// Sequence returns the current sequence value without advancing.
// Useful for assertions on factory state in test helpers.
func (f *Factory[T]) Sequence() int { return int(f.seq.Load()) }

// Entity returns the underlying Entity[T] handle.
func (f *Factory[T]) Entity() *Entity[T] { return f.entity }
