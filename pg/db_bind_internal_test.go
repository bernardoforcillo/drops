package pg

import (
	"context"
	"reflect"
	"testing"

	"github.com/bernardoforcillo/drops"
	"github.com/bernardoforcillo/drops/dropstest"
)

// What a transaction-bound *DB carries.
//
// [DB.bind] builds the DB that every statement inside a transaction
// goes out through. Before it existed the same struct literal was
// written twice — in Begin and in inTxOnce — and both spellings carried
// the hook and dropped the tracer, so a WithTracer produced no span for
// anything inside a transaction, including the SET LOCAL ROLE and
// set_config [DB.InTxAs] sends. Dropping a field is a decision; being
// dropped because nobody typed it is not, and the difference is
// invisible in a struct literal.
//
// So the decision is written down per field and checked in both
// directions: a field added to DB fails the census until somebody says
// what a transaction should do with it, and an entry naming a field DB
// no longer has fails too.

// bindDisposition says, for every field of DB, what bind does with it
// and why. "carried" and "replaced" need no argument beyond the name;
// a field that is deliberately DROPPED has to say why carrying it
// would be wrong, on the same terms as the exemption maps in the
// scoping censuses.
var bindDisposition = map[string]string{
	"drv":    "replaced — putting the transaction handle where the pool was is the whole of what bind is for",
	"hook":   "carried — a hook that stopped seeing statements at the BEGIN would lose exactly the statements a transaction exists to group, and the audit trail with them",
	"tracer": "carried — the field this test was written for: a trace that stopped at the BEGIN went dark across the boundary a latency investigation follows it over, and lost the two statements InTxAs sends to establish the identity the work ran under",
	"retry":  "dropped — a RetryPolicy re-runs a whole transaction and this DB is inside one already, so a nested InTx that retried would re-run its body against a transaction the failed attempt had aborted, which PostgreSQL answers with 25P02 until the outer transaction ends. Retrying belongs to the outermost InTx, which still has the policy",
}

// TestEveryDBFieldHasABindDisposition is the census. It fails when DB
// grows a field bind says nothing about, which is the shape the tracer
// defect had: nobody decided to drop it.
func TestEveryDBFieldHasABindDisposition(t *testing.T) {
	dbType := reflect.TypeOf(DB{})
	for _, f := range reflect.VisibleFields(dbType) {
		if bindDisposition[f.Name] == "" {
			t.Errorf("*DB has a field %q that bind says nothing about — decide whether a transaction-bound DB carries it, and write the reason in bindDisposition", f.Name)
		}
	}
	for name := range bindDisposition {
		if _, ok := dbType.FieldByName(name); !ok {
			t.Errorf("bindDisposition names a field %q that *DB no longer has — drop the entry", name)
		}
	}
}

// TestBindCarriesTheConfigurationIntoTheTransaction is the behaviour
// the dispositions describe, asserted on a DB with every field set to
// something distinguishable.
func TestBindCarriesTheConfigurationIntoTheTransaction(t *testing.T) {
	var hooked []string
	drv := dropstest.New()
	tracer := &countingTracer{}
	db := New(drv).
		WithHook(func(_ context.Context, e drops.QueryEvent) { hooked = append(hooked, e.Kind) }).
		WithTracer(tracer).
		WithRetry(DefaultRetryPolicy())

	tx, err := drv.Begin(context.Background())
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	inner := db.bind(tx)

	if inner.drv != drops.Driver(tx) {
		t.Errorf("bind kept the pool handle: drv = %T, want the transaction", inner.drv)
	}
	if inner.tracer != Tracer(tracer) {
		t.Errorf("tracer = %v, want the one installed on the pool DB — %s", inner.tracer, bindDisposition["tracer"])
	}
	if inner.hook == nil {
		t.Errorf("hook = nil, want the one installed on the pool DB — %s", bindDisposition["hook"])
	} else {
		inner.emit(context.Background(), drops.QueryEvent{Kind: "exec"})
		if len(hooked) != 1 || hooked[0] != "exec" {
			t.Errorf("the bound DB's hook is not the pool DB's: events = %v", hooked)
		}
	}
	if inner.retry != nil {
		t.Errorf("retry = %+v, want none — %s", *inner.retry, bindDisposition["retry"])
	}
}

// TestBoundDBTracesTheStatementsInsideATransaction is the same claim
// from the outside, and the one that fails when the propagation is
// removed: it never mentions bind, only that a statement issued on the
// transactional *DB is traced.
func TestBoundDBTracesTheStatementsInsideATransaction(t *testing.T) {
	tracer := &countingTracer{}
	db := New(dropstest.New()).WithTracer(tracer)
	if err := db.InTx(context.Background(), func(tx *DB) error {
		_, err := tx.Exec(context.Background(), "UPDATE x SET y = 1")
		return err
	}); err != nil {
		t.Fatalf("InTx: %v", err)
	}
	if tracer.names() == nil {
		t.Fatal("a statement inside a transaction produced no span at all")
	}
	if got, want := tracer.names(), []string{"drops.exec"}; !reflect.DeepEqual(got, want) {
		t.Errorf("spans = %v, want %v", got, want)
	}
}

// TestBoundDBTracesTheIdentityInTxAsEstablishes is the case the
// propagation was worth fixing for. The SET LOCAL ROLE and the
// set_config go out through the transactional *DB, so with the tracer
// dropped they were the first two statements to leave the trace — and
// they are the ones that say which identity everything after them ran
// under.
func TestBoundDBTracesTheIdentityInTxAsEstablishes(t *testing.T) {
	tracer := &countingTracer{}
	db := New(dropstest.New()).WithTracer(tracer)
	sess := Session{Role: "app_tenant", Settings: map[string]string{"app.tenantId": "7"}}
	if err := db.InTxAs(context.Background(), sess, func(*DB) error { return nil }); err != nil {
		t.Fatalf("InTxAs: %v", err)
	}
	// One for the role, one for the setting, in the order InTxAs sends
	// them; the body sends nothing.
	if got, want := tracer.names(), []string{"drops.exec", "drops.exec"}; !reflect.DeepEqual(got, want) {
		t.Errorf("spans = %v, want %v — the identity statements are traced like any other", got, want)
	}
}

// countingTracer records the name of every span opened. The package's
// external tests have a recordingTracer of their own; this one is in
// package pg because bind is.
type countingTracer struct{ spans []string }

func (c *countingTracer) Start(ctx context.Context, name string) (context.Context, Span) {
	c.spans = append(c.spans, name)
	return ctx, noopSpan{}
}

func (c *countingTracer) names() []string { return c.spans }
