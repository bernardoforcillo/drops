package pg

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/bernardoforcillo/drops"
)

// Long jobs that survive the process running them.
//
// [Backfill] drives a chunked loop over a huge table and persists its
// checkpoint, so a crash resumes rather than restarts. What it cannot
// do is exist independently of the process: the loop is a function
// call, so nothing else can start the work, ask how far it has got,
// or stop it, and two deploys running the same backfill run it twice.
//
// A JobQueue is the row that outlives the call. A job is claimed by
// whichever worker gets there first, heartbeats while it runs,
// records its progress where anybody can read it, and is stopped by
// setting a column. The pattern is InstantDB's indexing and backup
// jobs, including the detail that makes it safe: a partial unique
// index allows at most one live job per key, so the "start the
// reindex" button is idempotent no matter how many times it is
// pressed or how many machines are listening.
//
//	q := pg.NewJobQueue(db, "jobs")
//	id, err := q.Enqueue(ctx, pg.NewJob{
//	    Kind: "reindex", Key: "orders", Total: 4_200_000,
//	})
//
//	// On every worker:
//	job, ok, err := q.Claim(ctx, "worker-3", "reindex")
//	if ok {
//	    defer q.Heartbeat(ctx, job.ID)     // called periodically
//	    ...
//	    err = q.Complete(ctx, job.ID)
//	}
//
// # Where the guarantees come from, and stop
//
// Claiming is `FOR UPDATE SKIP LOCKED` in one statement, so two
// workers cannot take one job however they race.
//
// A worker that dies holds nothing — the lock went with its
// connection — but the job stays marked running until [Reap] notices
// its heartbeat has stopped. Until then nobody else takes it, which
// is the right default: a job that is merely slow must not be started
// twice. Choose the reap threshold accordingly, and heartbeat more
// often than it.
//
// [Reap] is at-least-once, not exactly-once. A worker partitioned
// from the database long enough to be reaped may still be running,
// and the reaped job may then be claimed by somebody else. Jobs must
// be safe to run twice — a chunked backfill written against
// [Backfill] already is, because each chunk is idempotent and the
// checkpoint says where to resume.

// JobState is where a job is in its life.
type JobState string

const (
	// JobPending is enqueued and unclaimed.
	JobPending JobState = "pending"
	// JobRunning is claimed by a worker that is still heartbeating.
	JobRunning JobState = "running"
	// JobDone completed successfully.
	JobDone JobState = "done"
	// JobFailed gave up. LastError says why.
	JobFailed JobState = "failed"
	// JobCanceled was stopped by request. A worker notices at its
	// next checkpoint; nothing is killed from outside.
	JobCanceled JobState = "canceled"
)

// Live reports whether the state is one that occupies the "at most
// one per key" slot: pending or running.
func (s JobState) Live() bool { return s == JobPending || s == JobRunning }

// Job is one row of the queue.
type Job struct {
	ID          int64
	Kind        string
	Key         string
	State       JobState
	Payload     json.RawMessage
	Worker      string
	Progress    int64
	Total       int64
	Attempts    int
	LastError   string
	CreatedAt   time.Time
	StartedAt   time.Time
	HeartbeatAt time.Time
	FinishedAt  time.Time
}

// Fraction returns how far along the job is, in [0,1], or 0 when it
// declared no total.
func (j Job) Fraction() float64 {
	if j.Total <= 0 || j.Progress <= 0 {
		return 0
	}
	if j.Progress >= j.Total {
		return 1
	}
	return float64(j.Progress) / float64(j.Total)
}

// NewJob describes a job to enqueue.
type NewJob struct {
	// Kind groups jobs a given worker knows how to run, e.g.
	// "reindex" or "backup".
	Kind string

	// Key identifies what the job is about — the table, the tenant,
	// the account. At most one job per (Kind, Key) may be live at a
	// time; see [ErrJobAlreadyQueued].
	Key string

	// Payload is whatever the worker needs. Encoded as JSON.
	Payload any

	// Total is the unit count the job expects to process, for
	// progress reporting. Zero means unknown.
	Total int64
}

// ErrJobAlreadyQueued is returned by [JobQueue.Enqueue] when a job
// with the same kind and key is already pending or running.
//
// It is an expected outcome rather than a failure: the caller asked
// for the work to happen and it is going to. Treat it as success
// unless the caller specifically needs a new run.
var ErrJobAlreadyQueued = errors.New("drops/pg: a job for this kind and key is already queued")

// ErrJobNotRunning is returned when an update targets a job that is
// no longer claimed — because it was reaped, canceled, or already
// finished. A worker that sees it should stop.
var ErrJobNotRunning = errors.New("drops/pg: job is not running")

// JobQueue is a durable queue of long jobs in one table.
type JobQueue struct {
	db    *DB
	table string
}

// NewJobQueue returns a queue over the named table. Create the table
// from [NewJobTable].
func NewJobQueue(db *DB, table string) *JobQueue {
	return &JobQueue{db: db, table: table}
}

// Table returns the queue's table name.
func (q *JobQueue) Table() string { return q.table }

// NewJobTable declares the schema a [JobQueue] runs on.
//
// The partial unique index is the load-bearing part. It covers
// (kind, key) for the pending and running rows only, so the database
// itself enforces "at most one live job per key" — the check cannot
// be raced, unlike the SELECT-then-INSERT every application writes
// first. Finished rows fall out of the index, so history accumulates
// without ever blocking a new run.
func NewJobTable(name string) *Table {
	t := NewTable(name)
	Add(t, BigSerial("id").PrimaryKey())
	kind := Add(t, Text("kind").NotNull())
	key := Add(t, Text("key").NotNull().Default("''"))
	Add(t, Text("state").NotNull().Default("'pending'"))
	Add(t, JSONB("payload"))
	Add(t, Text("worker").NotNull().Default("''"))
	Add(t, BigInt("progress").NotNull().Default("0"))
	Add(t, BigInt("total").NotNull().Default("0"))
	Add(t, Integer("attempts").NotNull().Default("0"))
	Add(t, Text("lastError").NotNull().Default("''"))
	Add(t, Timestamp("createdAt", true).NotNull().Default("now()"))
	Add(t, Timestamp("startedAt", true))
	heartbeat := Add(t, Timestamp("heartbeatAt", true))
	Add(t, Timestamp("finishedAt", true))

	// At most one live job per (kind, key). The predicate is what
	// keeps finished rows out of it.
	t.AddIndex(NewIndex(name+"LiveIdx", t, kind.Column, key.Column).
		Unique().
		Where(drops.SQL("state IN ('pending','running')")))

	// The claim path: pending rows of one kind, oldest first.
	t.AddIndex(NewIndex(name+"ClaimIdx", t, kind.Column, t.Col("id")).
		Where(drops.SQL("state = 'pending'")))

	// The reap path: running rows by how stale their heartbeat is.
	t.AddIndex(NewIndex(name+"ReapIdx", t, heartbeat.Column).
		Where(drops.SQL("state = 'running'")))

	return t
}

// Enqueue adds a job and returns its id.
//
// It returns [ErrJobAlreadyQueued] when one for the same kind and key
// is already live. The answer comes from the unique index rather than
// from a lookup, so two callers pressing the button at once get one
// job and one ErrJobAlreadyQueued, rather than two jobs.
func (q *JobQueue) Enqueue(ctx context.Context, j NewJob) (int64, error) {
	if j.Kind == "" {
		return 0, errors.New("drops/pg: a job needs a kind")
	}
	payload := json.RawMessage("null")
	if j.Payload != nil {
		b, err := json.Marshal(j.Payload)
		if err != nil {
			return 0, fmt.Errorf("drops/pg: encoding job payload: %w", err)
		}
		payload = b
	}
	rows, err := q.db.Query(ctx, fmt.Sprintf(`
		INSERT INTO %s ("kind", "key", "payload", "total")
		VALUES ($1, $2, $3, $4)
		RETURNING "id"`, quoteIdent(q.table)),
		j.Kind, j.Key, []byte(payload), j.Total)
	if err != nil {
		if errors.Is(err, ErrUniqueViolation) {
			return 0, fmt.Errorf("%w: %s/%s", ErrJobAlreadyQueued, j.Kind, j.Key)
		}
		return 0, err
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return 0, err
		}
		return 0, errors.New("drops/pg: enqueue returned no id")
	}
	var id int64
	if err := rows.Scan(&id); err != nil {
		return 0, err
	}
	return id, rows.Err()
}

// Claim takes the oldest pending job of the given kinds for worker,
// or reports ok=false when there is none.
//
// The claim is one statement: the SELECT that finds the row and the
// UPDATE that marks it are the same command, so two workers racing
// cannot both win. SKIP LOCKED means the loser takes the next job
// instead of waiting for the winner.
//
// Pass no kinds to claim any.
func (q *JobQueue) Claim(ctx context.Context, worker string, kinds ...string) (Job, bool, error) {
	if worker == "" {
		return Job{}, false, errors.New("drops/pg: Claim needs a worker name")
	}
	where := `"state" = 'pending'`
	args := []any{worker}
	if len(kinds) > 0 {
		where += ` AND "kind" = ANY($2)`
		args = append(args, kinds)
	}
	rows, err := q.db.Query(ctx, fmt.Sprintf(`
		UPDATE %[1]s SET
			"state"       = 'running',
			"worker"      = $1,
			"startedAt"   = now(),
			"heartbeatAt" = now(),
			"attempts"    = "attempts" + 1
		WHERE "id" = (
			SELECT "id" FROM %[1]s
			WHERE %[2]s
			ORDER BY "id"
			FOR UPDATE SKIP LOCKED
			LIMIT 1
		)
		RETURNING %[3]s`, quoteIdent(q.table), where, jobColumns), args...)
	if err != nil {
		return Job{}, false, err
	}
	defer rows.Close()
	if !rows.Next() {
		return Job{}, false, rows.Err()
	}
	job, err := scanJob(rows)
	if err != nil {
		return Job{}, false, err
	}
	return job, true, rows.Err()
}

// Heartbeat says the worker is still alive and, when progress is
// non-negative, records how far it has got.
//
// It returns [ErrJobNotRunning] when the job has been canceled or
// reaped out from under the worker, which is how a worker learns to
// stop: there is no signal from outside, and a heartbeat is the one
// call a long job is already making regularly.
//
//	if err := q.Heartbeat(ctx, job.ID, done); errors.Is(err, pg.ErrJobNotRunning) {
//	    return nil  // somebody canceled us
//	}
func (q *JobQueue) Heartbeat(ctx context.Context, id int64, progress ...int64) error {
	set := `"heartbeatAt" = now()`
	args := []any{id}
	if len(progress) > 0 && progress[0] >= 0 {
		set += `, "progress" = $2`
		args = append(args, progress[0])
	}
	res, err := q.db.Exec(ctx, fmt.Sprintf(
		`UPDATE %s SET %s WHERE "id" = $1 AND "state" = 'running'`,
		quoteIdent(q.table), set), args...)
	if err != nil {
		return err
	}
	return jobUpdated(res, id)
}

// Complete marks the job done.
func (q *JobQueue) Complete(ctx context.Context, id int64) error {
	res, err := q.db.Exec(ctx, fmt.Sprintf(`
		UPDATE %s SET "state" = 'done', "finishedAt" = now(), "lastError" = ''
		WHERE "id" = $1 AND "state" = 'running'`, quoteIdent(q.table)), id)
	if err != nil {
		return err
	}
	return jobUpdated(res, id)
}

// Fail marks the job failed and records why.
//
// It does not retry. Whether a failed job should run again is a
// decision about the job — a backfill that hit a bad row will hit it
// again — so re-enqueueing is left to the caller, who can now do it
// because the key slot is free.
func (q *JobQueue) Fail(ctx context.Context, id int64, cause error) error {
	msg := ""
	if cause != nil {
		msg = cause.Error()
	}
	res, err := q.db.Exec(ctx, fmt.Sprintf(`
		UPDATE %s SET "state" = 'failed', "finishedAt" = now(), "lastError" = $2
		WHERE "id" = $1 AND "state" = 'running'`, quoteIdent(q.table)), id, msg)
	if err != nil {
		return err
	}
	return jobUpdated(res, id)
}

// Cancel asks a job to stop, whether it is pending or running.
//
// A pending job is canceled outright. A running one is marked, and
// its worker finds out at its next [Heartbeat] — nothing is killed
// from outside, because a job halfway through a chunk should finish
// the chunk. A worker that never heartbeats never notices, which is
// the reason to heartbeat on a schedule rather than only when
// convenient.
func (q *JobQueue) Cancel(ctx context.Context, id int64) error {
	res, err := q.db.Exec(ctx, fmt.Sprintf(`
		UPDATE %s SET "state" = 'canceled', "finishedAt" = now()
		WHERE "id" = $1 AND "state" IN ('pending','running')`, quoteIdent(q.table)), id)
	if err != nil {
		return err
	}
	return jobUpdated(res, id)
}

// Reap returns jobs whose heartbeat is older than stale to the
// pending state, and reports how many it moved.
//
// Run it on a schedule from anywhere; it is the only thing that
// recovers a job whose worker died, since a dead worker cannot mark
// its own job.
//
// stale must be comfortably longer than the heartbeat interval — a
// job reaped while its worker is merely slow gets a second worker,
// and both keep running. That is why jobs have to be safe to run
// twice, and why this is at-least-once rather than exactly-once. Ten
// heartbeat intervals is a reasonable starting point.
func (q *JobQueue) Reap(ctx context.Context, stale time.Duration) (int64, error) {
	if stale <= 0 {
		return 0, errors.New("drops/pg: Reap needs a positive staleness threshold")
	}
	res, err := q.db.Exec(ctx, fmt.Sprintf(`
		UPDATE %s SET
			"state"     = 'pending',
			"worker"    = '',
			"lastError" = 'reaped: heartbeat stopped'
		WHERE "state" = 'running'
		  AND "heartbeatAt" < now() - $1::interval`, quoteIdent(q.table)),
		fmt.Sprintf("%d milliseconds", stale.Milliseconds()))
	if err != nil {
		return 0, err
	}
	if res == nil {
		return 0, nil
	}
	return res.RowsAffected()
}

// Get returns one job by id.
func (q *JobQueue) Get(ctx context.Context, id int64) (Job, error) {
	rows, err := q.db.Query(ctx, fmt.Sprintf(
		`SELECT %s FROM %s WHERE "id" = $1`, jobColumns, quoteIdent(q.table)), id)
	if err != nil {
		return Job{}, err
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return Job{}, err
		}
		return Job{}, fmt.Errorf("drops/pg: no job %d", id)
	}
	job, err := scanJob(rows)
	if err != nil {
		return Job{}, err
	}
	return job, rows.Err()
}

// Live returns the pending and running jobs, oldest first.
//
// It is what a status endpoint renders: "this is what the cluster is
// working on", answerable from any process because the answer is in
// the database rather than in the one that happens to be running the
// loop.
func (q *JobQueue) Live(ctx context.Context, kinds ...string) ([]Job, error) {
	where := `"state" IN ('pending','running')`
	var args []any
	if len(kinds) > 0 {
		where += ` AND "kind" = ANY($1)`
		args = append(args, kinds)
	}
	rows, err := q.db.Query(ctx, fmt.Sprintf(
		`SELECT %s FROM %s WHERE %s ORDER BY "id"`,
		jobColumns, quoteIdent(q.table), where), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// Cleanup deletes finished jobs older than retain, and reports how
// many it removed. Live jobs are never touched however old they are.
func (q *JobQueue) Cleanup(ctx context.Context, retain time.Duration) (int64, error) {
	if retain < 0 {
		return 0, errors.New("drops/pg: Cleanup needs a non-negative retention")
	}
	res, err := q.db.Exec(ctx, fmt.Sprintf(`
		DELETE FROM %s
		WHERE "state" NOT IN ('pending','running')
		  AND "finishedAt" < now() - $1::interval`, quoteIdent(q.table)),
		fmt.Sprintf("%d milliseconds", retain.Milliseconds()))
	if err != nil {
		return 0, err
	}
	if res == nil {
		return 0, nil
	}
	return res.RowsAffected()
}

// jobColumns is the projection every job read shares, in the order
// scanJob expects.
//
// Every identifier is quoted, including the ones that would not need
// it. [NewJobTable] declares camelCase columns and CREATE TABLE quotes
// them, so the columns really are named "lastError" and "heartbeatAt"
// — while an unquoted lastError in a statement folds to lasterror and
// finds nothing. Quoting the lowercase ones too keeps the two lists
// from drifting into that trap one column at a time.
const jobColumns = `"id", "kind", "key", "state", coalesce("payload", 'null'::jsonb), "worker",
	"progress", "total", "attempts", "lastError", "createdAt",
	coalesce("startedAt", to_timestamp(0)), coalesce("heartbeatAt", to_timestamp(0)),
	coalesce("finishedAt", to_timestamp(0))`

func scanJob(rows drops.Rows) (Job, error) {
	var j Job
	var payload []byte
	var state string
	err := rows.Scan(&j.ID, &j.Kind, &j.Key, &state, &payload, &j.Worker,
		&j.Progress, &j.Total, &j.Attempts, &j.LastError, &j.CreatedAt,
		&j.StartedAt, &j.HeartbeatAt, &j.FinishedAt)
	if err != nil {
		return Job{}, err
	}
	j.State = JobState(state)
	j.Payload = json.RawMessage(payload)
	return j, nil
}

// jobUpdated turns "no rows matched" into [ErrJobNotRunning].
//
// A driver that cannot report the affected count leaves the caller
// with success, which is the safe direction here: the alternative is
// telling a worker its job was canceled when it was not, and stopping
// work that should continue.
func jobUpdated(res drops.Result, id int64) error {
	if res == nil {
		return nil
	}
	n, err := res.RowsAffected()
	if err != nil {
		return nil
	}
	if n == 0 {
		return fmt.Errorf("%w: %d", ErrJobNotRunning, id)
	}
	return nil
}
