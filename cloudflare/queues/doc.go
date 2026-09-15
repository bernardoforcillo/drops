// Package queues is Cloudflare Queues over its HTTP API — the
// durable, at-least-once hop between a database write and whatever
// has to happen next.
//
// drops already has an outbox
// ([github.com/bernardoforcillo/drops/pg.Outbox]): the pattern where
// a change and the intent to publish it are written in one
// transaction, so the two cannot disagree. What the outbox needs on
// the other side is somewhere durable to publish *to*, and on
// Cloudflare that is a Queue.
//
//	q := queues.New(cf).Queue(queueID)
//	err := q.Publish(ctx, queues.JSON(change))
//
// # Two kinds of consumer, one of them usable from Go
//
// A Queue is consumed either by a Worker — Cloudflare pushes batches
// into it — or by a pull consumer, which asks over HTTP. Only the
// second is reachable from a Go process outside Cloudflare, so it is
// the one this package implements, and a queue has to be configured
// for it before [Queue.Pull] returns anything: the pull consumer is a
// property of the queue, set with wrangler or in the dashboard, not
// something a client turns on per request.
//
// # Leases, not deletes
//
// A pulled message is not removed, it is leased. Each carries a lease
// ID and stays invisible to other consumers for the visibility
// timeout, and then comes back if nobody acknowledged it. So a
// consumer that crashes mid-batch loses nothing, and one that
// succeeds must say so:
//
//	batch, err := q.Pull(ctx, queues.PullOptions{BatchSize: 50})
//	for _, m := range batch.Messages {
//	    if err := handle(m); err != nil {
//	        batch.Retry(m, 30*time.Second)   // back to the queue, later
//	        continue
//	    }
//	    batch.Ack(m)
//	}
//	_, err = batch.Settle(ctx)
//
// [Queue.Consume] is that loop written once, for the common case
// where the handler's error is the only decision.
//
// At-least-once is the guarantee, which means a handler will
// eventually see the same message twice — a lease that expired while
// the work was in flight, an ack that never arrived. Making the
// handler idempotent is not optional, and it is the same requirement
// [github.com/bernardoforcillo/drops/mirror.Sink] states for the same
// reason.
//
// # Limits
//
// [Published] carries them as values. The two that change a design
// are the 128 KB ceiling on a message — a row with a large text
// column will not fit, so send the key and let the consumer read the
// row — and the hundred-message ceiling on a batch, which is the size
// [Queue.Consume] asks for.
package queues
