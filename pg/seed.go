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
}

// seedOp is the unit of work the Seeder applies. Generic over T at
// the call site via SeedAdd; type-erased here so the Seeder can hold
// a mixed list of entity ops in one slice.
type seedOp interface {
	run(db *DB, ctx context.Context) error
}

// NewSeeder returns a Seeder bound to db.
func NewSeeder(db *DB) *Seeder { return &Seeder{db: db} }

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
func (s *Seeder) Apply(ctx context.Context) error {
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
