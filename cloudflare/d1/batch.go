package d1

import (
	"context"
	"sync"

	"github.com/bernardoforcillo/drops"
)

// Batch is several statements sent to D1 as one request.
//
// It is what D1 offers in place of BEGIN/COMMIT: the statements run
// in order, in one implicit transaction, and foreign-key constraints
// are deferred to the end of it so a parent and its children can be
// written in either order.
//
// The atomicity is D1's guarantee, not something this client can
// enforce — all the client controls is that the statements travel in
// one request. What it does enforce is that they are sent as one, and
// that the results come back one per statement.
//
//	b := d1.NewBatch(drv)
//	b.Add("INSERT INTO orders (id, total) VALUES (?, ?)", id, total)
//	for _, l := range lines {
//	    b.Add("INSERT INTO order_lines (order_id, sku) VALUES (?, ?)", id, l.SKU)
//	}
//	res, err := b.Run(ctx)
//
// [Driver.Begin] is the same mechanism wearing a [drops.Tx], for the
// code that is already written against one.
type Batch struct {
	drv   *Driver
	stmts []Statement
	err   error
}

// NewBatch starts a batch against drv.
func NewBatch(drv *Driver) *Batch { return &Batch{drv: drv} }

// Add appends a statement. Binding errors are held until [Batch.Run]
// so a batch can be built up without an error check per line.
func (b *Batch) Add(sql string, args ...any) *Batch {
	if b.err != nil {
		return b
	}
	params, err := BindParams(args)
	if err != nil {
		b.err = err
		return b
	}
	b.stmts = append(b.stmts, Statement{SQL: sql, Params: params})
	return b
}

// Len returns the number of statements added so far.
func (b *Batch) Len() int { return len(b.stmts) }

// Statements returns the SQL text of each statement added, in order.
// For tests and for logging a batch before it runs.
func (b *Batch) Statements() []string {
	out := make([]string, len(b.stmts))
	for i, s := range b.stmts {
		out[i] = s.SQL
	}
	return out
}

// Err returns the first binding error, if any.
func (b *Batch) Err() error { return b.err }

// Run sends the batch and returns one [Result] per statement, in
// order.
//
// Rows are not returned: a batch is for writes. A SELECT inside one
// runs, and its result is reported with the rows discarded — use
// [Driver.Query] for a read.
func (b *Batch) Run(ctx context.Context) ([]*Result, error) {
	if b.err != nil {
		return nil, b.err
	}
	if len(b.stmts) == 0 {
		return nil, ErrNoStatements
	}
	out, err := b.drv.send(ctx, b.stmts)
	if err != nil {
		return nil, err
	}
	results := make([]*Result, len(out))
	for i, r := range out {
		results[i] = &Result{meta: r.Meta}
	}
	return results, nil
}

// Tx is a D1 transaction: a buffer of statements sent as one batch at
// commit.
//
// It implements [github.com/bernardoforcillo/drops.Tx] so
// [github.com/bernardoforcillo/drops.InTx] and
// [github.com/bernardoforcillo/drops/sqlite.DB.InTx] work — with the
// two restrictions D1's HTTP API forces, both spelled out in the
// package comment: [Tx.Query] returns [ErrTxQuery], and a result's
// row count is [ErrPending] until the commit.
//
// Safe for concurrent use, in the sense that concurrent Execs are
// serialised onto the buffer in whatever order they arrive. Whether
// that order is the one you meant is your problem, not the mutex's —
// a transaction whose statements must run in a fixed order should be
// built from one goroutine.
type Tx struct {
	drv *Driver

	mu      sync.Mutex
	stmts   []Statement
	pending []*pendingResult
	done    bool
}

var _ drops.Tx = (*Tx)(nil)

// Exec buffers a statement. Nothing is sent until [Tx.Commit].
//
// The returned Result is a promise: [Result.RowsAffected] answers
// [ErrPending] until the commit, and the real number afterwards.
func (t *Tx) Exec(ctx context.Context, sql string, args ...any) (drops.Result, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	params, err := BindParams(args)
	if err != nil {
		return nil, err
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.done {
		return nil, ErrTxDone
	}
	p := &pendingResult{}
	t.stmts = append(t.stmts, Statement{SQL: sql, Params: params})
	t.pending = append(t.pending, p)
	return &Result{pending: p}, nil
}

// Query always returns [ErrTxQuery]. See the package comment: the
// buffered statements have not run, so there is nothing to read, and
// running the SELECT outside the buffer instead would quietly break
// read-your-writes.
func (t *Tx) Query(context.Context, string, ...any) (drops.Rows, error) {
	return nil, ErrTxQuery
}

// Begin returns [ErrNestedTx]. D1 has no SAVEPOINT over HTTP.
func (t *Tx) Begin(context.Context) (drops.Tx, error) { return nil, ErrNestedTx }

// Commit sends the buffered statements as one batch and fills in the
// results handed out by [Tx.Exec].
//
// An empty transaction commits successfully without a request: a unit
// of work that turned out to have nothing to do is not an error, and
// sending an empty batch to D1 would be.
func (t *Tx) Commit(ctx context.Context) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.done {
		return ErrTxDone
	}
	t.done = true
	if len(t.stmts) == 0 {
		t.settle(nil, nil)
		return nil
	}
	out, err := t.drv.send(ctx, t.stmts)
	if err != nil {
		t.settle(nil, err)
		return err
	}
	t.settle(out, nil)
	return nil
}

// Rollback discards the buffer. It costs nothing and cannot fail:
// nothing was sent.
//
// Every result handed out is settled with [ErrTxDone] rather than
// left pending, so a caller that kept one and asks it for a row count
// after the rollback gets an answer instead of a lie.
func (t *Tx) Rollback(context.Context) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.done {
		return ErrTxDone
	}
	t.done = true
	t.settle(nil, ErrTxDone)
	return nil
}

// settle resolves every pending result. Results beyond what D1
// answered for — which should not happen, but would leave a caller
// blocked on ErrPending forever if it did — are settled with the
// batch's own error, or with a nil-safe zero.
func (t *Tx) settle(out []StatementResult, err error) {
	for i, p := range t.pending {
		p.done = true
		p.err = err
		if err == nil && i < len(out) {
			p.meta = out[i].Meta
		}
	}
}
