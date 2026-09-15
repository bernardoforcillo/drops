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
// the one this package implements.
//
// A queue has to have one before [Queue.Pull] returns anything, and
// it is a property of the queue rather than of a request.
// [Queue.EnsurePullConsumer] is the call that belongs at boot: the
// queue is consumable afterwards, or the error says why. Without it
// the failure is silent, because a queue with no pull consumer
// answers a pull exactly as an empty queue does —
// [Queue.PullConsumer] is how that question gets an answer.
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
// # The rest of the queue
//
// [Client.Create] and [Client.Delete] make and remove queues;
// [Client.UpdateSettings] changes the delivery delay and the
// retention period; [Client.Pause] and [Client.Resume] stop and start
// delivery without losing anything, which is the lever to pull when a
// consumer is doing damage and the messages are worth keeping.
// [Queue.Purge] is the opposite of that and says so: it takes
// [DeleteMessagesPermanently] as an argument so the sentence is at
// the call site.
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
