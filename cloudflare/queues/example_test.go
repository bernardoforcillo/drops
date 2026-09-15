package queues_test

import (
	"context"
	"errors"
	"log"
	"os/signal"
	"syscall"
	"time"

	"github.com/bernardoforcillo/drops/cloudflare"
	"github.com/bernardoforcillo/drops/cloudflare/queues"
)

func ExampleNew() {
	cf, err := cloudflare.New("your-account-id", cloudflare.WithAPIToken("your-api-token"))
	if err != nil {
		log.Fatal(err)
	}
	q := queues.New(cf).Queue("your-queue-id")
	_ = q
}

// The other half of an outbox: the transaction wrote the row and the
// intent to publish it, and this is where the intent goes.
func ExampleQueue_Publish() {
	var q *queues.Queue
	ctx := context.Background()

	type invalidate struct {
		Table string `json:"table"`
		Key   string `json:"key"`
	}
	// The key rather than the row: a message is capped at 128 KB, and
	// a consumer that can read the database does not need a copy of
	// it.
	if err := q.Publish(ctx, queues.JSON(invalidate{Table: "users", Key: "42"})); err != nil {
		log.Fatal(err)
	}
}

// The loop the two-step batch exists for: one pull, a decision per
// message, one settlement.
func ExampleQueue_Pull() {
	var q *queues.Queue
	ctx := context.Background()

	batch, err := q.Pull(ctx, queues.PullOptions{
		BatchSize: 50,
		// Comfortably more than the work takes: a timeout that
		// expires mid-job hands the same message to a second
		// consumer while the first is still running it.
		VisibilityTimeout: 2 * time.Minute,
	})
	if err != nil {
		log.Fatal(err)
	}
	for _, m := range batch.Messages {
		if err := doWork(m); err != nil {
			// A message that has failed many times is not going to
			// succeed on the next one; let it reach the dead-letter
			// queue rather than spending the retry budget quickly.
			_ = batch.Retry(m, queues.DefaultBackoff(m.Attempts))
			continue
		}
		_ = batch.Ack(m)
	}
	settled, err := batch.Settle(ctx)
	if err != nil {
		log.Fatal(err)
	}
	// A warning here is an expired lease, which means the visibility
	// timeout is too short for the work.
	for lease, warning := range settled.Warnings {
		log.Printf("lease %s: %s", lease, warning)
	}
}

// Consume is that loop written once, for the case where the handler's
// error is the only decision.
func ExampleQueue_Consume() {
	var q *queues.Queue

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)

	err := q.Consume(ctx, func(ctx context.Context, m queues.PulledMessage) error {
		var payload struct {
			Table string `json:"table"`
			Key   string `json:"key"`
		}
		if err := m.Decode(&payload); err != nil {
			// A message that will never parse must not be retried a
			// hundred times: acknowledge it and record it somewhere
			// a human will look.
			log.Printf("dropping unparseable message %s: %v", m.ID, err)
			return nil
		}
		return invalidateCache(ctx, payload.Table, payload.Key)
	}, queues.ConsumeOptions{
		Pull: queues.PullOptions{BatchSize: 100, VisibilityTimeout: time.Minute},
		Idle: 5 * time.Second,
	})
	stop()
	// A cancelled context is the shutdown working, not a failure.
	if err != nil && !errors.Is(err, context.Canceled) {
		log.Fatal(err)
	}
}

func doWork(queues.PulledMessage) error                     { return nil }
func invalidateCache(context.Context, string, string) error { return nil }
