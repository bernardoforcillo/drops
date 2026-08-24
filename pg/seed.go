package pg

import (
	"context"
	"fmt"
)

// Seeder accumulates seed data declarations and applies them in
// order. Use it to populate dev / test databases and fixture
// scenarios without hand-wiring transactions:
//
//	seeder := pg.NewSeeder(db)
//	pg.SeedAdd(seeder, UserEntity, alice, bob)
//	pg.SeedAdd(seeder, PostEntity, post1, post2)
//	if err := seeder.Apply(ctx); err != nil { ... }
//
// Apply runs every op inside a single transaction by default, so a
// failure rolls back every prior insert. Pass WithoutTransaction()
// to opt out — useful when the seeded data spans tables that the
// surrounding migration tool can't tx-wrap together.
type Seeder struct {
	db   *DB
	ops  []seedOp
	noTx bool
	gen  *Gen
}

// seedOp is the unit of work the Seeder applies. Generic over T at
// the call site via SeedAdd; type-erased here so the Seeder can hold
// a mixed list of entity ops in one slice.
type seedOp interface {
	run(db *DB, ctx context.Context) error
}

// seedResettable is implemented by the ops that carry state across a
// run — the generated-row plans, which accumulate what they created.
type seedResettable interface {
	reset()
}

// DefaultSeed is the PRNG seed a Seeder starts with. It is a constant
// rather than something drawn from the clock so that a seed run is
// reproducible whether or not anybody remembered to pin it.
const DefaultSeed uint64 = 1

// NewSeeder returns a Seeder bound to db.
func NewSeeder(db *DB) *Seeder { return &Seeder{db: db, gen: NewGen(DefaultSeed)} }

// WithSeed sets the integer the generated-row plans draw from — see
// [SeedMany]. The same seed against a clean database produces the same
// rows, which is the whole point: a test that fails on generated data
// is re-runnable only if the number that produced it is written down.
// Returns the seeder for chaining.
func (s *Seeder) WithSeed(seed uint64) *Seeder {
	s.gen = NewGen(seed)
	return s
}

// Gen returns the Seeder's generator. Useful for a [SeedDo] step that
// wants to draw from the same reproducible stream as the rest of the
// run rather than start an unrelated one.
func (s *Seeder) Gen() *Gen { return s.gen }

// WithoutTransaction disables the transactional wrapper Apply uses
// by default. Returns the seeder for chaining.
func (s *Seeder) WithoutTransaction() *Seeder {
	s.noTx = true
	return s
}

// SeedAdd registers rows for entity ent. It is a free function
// because Go does not allow generic methods. Returns s so calls
// chain.
func SeedAdd[T any](s *Seeder, ent *Entity[T], rows ...T) *Seeder {
	s.ops = append(s.ops, &seedAddOp[T]{ent: ent, rows: rows})
	return s
}

// SeedAddCreate registers rows for ent and uses Create per row,
// which populates RETURNING fields back into the pointed-to values.
// Use this when later seeds depend on generated PKs from earlier
// seeds.
func SeedAddCreate[T any](s *Seeder, ent *Entity[T], rows ...*T) *Seeder {
	s.ops = append(s.ops, &seedCreateOp[T]{ent: ent, rows: rows})
	return s
}

// SeedDo registers an arbitrary function. Use it for cross-entity
// fixups or seed steps that don't map cleanly to Create / CreateMany.
func SeedDo(s *Seeder, fn func(db *DB, ctx context.Context) error) *Seeder {
	s.ops = append(s.ops, seedFnOp(fn))
	return s
}

// Apply runs every registered op in declaration order. By default
// wraps the run in a transaction; on first error the transaction
// rolls back and the failure is returned.
//
// The generator is rewound to its seed first, and the generated-row
// plans forget what a previous Apply created, so applying the same
// Seeder twice against a clean database inserts the same rows twice
// rather than continuing where the last run left off.
func (s *Seeder) Apply(ctx context.Context) error {
	s.gen.Reset()
	for _, op := range s.ops {
		if r, ok := op.(seedResettable); ok {
			r.reset()
		}
	}
	run := func(db *DB) error {
		for i, op := range s.ops {
			if err := op.run(db, ctx); err != nil {
				return fmt.Errorf("drops/pg: Seeder op #%d: %w", i, err)
			}
		}
		return nil
	}
	if s.noTx {
		return run(s.db)
	}
	return s.db.InTx(ctx, run)
}

// ----------------------------------------------------------------------
// op implementations
// ----------------------------------------------------------------------

type seedAddOp[T any] struct {
	ent  *Entity[T]
	rows []T
}

func (o *seedAddOp[T]) run(db *DB, ctx context.Context) error {
	if len(o.rows) == 0 {
		return nil
	}
	_, err := o.ent.CreateMany(db, ctx, o.rows)
	return err
}

type seedCreateOp[T any] struct {
	ent  *Entity[T]
	rows []*T
}

func (o *seedCreateOp[T]) run(db *DB, ctx context.Context) error {
	for _, r := range o.rows {
		if err := o.ent.Create(db, ctx, r); err != nil {
			return err
		}
	}
	return nil
}

type seedFnOp func(db *DB, ctx context.Context) error

func (f seedFnOp) run(db *DB, ctx context.Context) error { return f(db, ctx) }
