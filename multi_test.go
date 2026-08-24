package drops_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/bernardoforcillo/drops"
)

// A driver that records the transaction lifecycle. Nothing here talks
// to a database: what a Multi does to a transaction is the root
// package's own logic, and the questions that are really the server's
// (does the rollback undo the rows?) are asked in the integration
// suite instead.
type recordingDriver struct {
	begins int
	tx     *recordingTx
}

type recordingTx struct {
	committed  int
	rolledBack int
	execs      []string
	// commitErr, when set, is what Commit returns — the failure that
	// belongs to no step.
	commitErr error
}

func (d *recordingDriver) Exec(context.Context, string, ...any) (drops.Result, error) {
	return nil, errors.New("recordingDriver: Exec outside a transaction")
}

func (d *recordingDriver) Query(context.Context, string, ...any) (drops.Rows, error) {
	return nil, errors.New("recordingDriver: Query outside a transaction")
}

func (d *recordingDriver) Begin(context.Context) (drops.Tx, error) {
	d.begins++
	d.tx = &recordingTx{}
	return d.tx, nil
}

func (t *recordingTx) Exec(_ context.Context, sql string, _ ...any) (drops.Result, error) {
	t.execs = append(t.execs, sql)
	return nil, nil
}

func (t *recordingTx) Query(context.Context, string, ...any) (drops.Rows, error) {
	return nil, errors.New("recordingTx: not implemented")
}

func (t *recordingTx) Begin(context.Context) (drops.Tx, error) { return t, nil }
func (t *recordingTx) Commit(context.Context) error            { t.committed++; return t.commitErr }
func (t *recordingTx) Rollback(context.Context) error          { t.rolledBack++; return nil }

type team struct{ ID int64 }

func TestMultiRunsStepsInOrderAndCommits(t *testing.T) {
	var order []string
	m := drops.NewMulti().
		Run("team", func(_ context.Context, tx drops.Tx, _ drops.Results) (any, error) {
			order = append(order, "team")
			_, _ = tx.Exec(context.Background(), "INSERT INTO teams")
			return &team{ID: 7}, nil
		}).
		Run("audit", func(_ context.Context, tx drops.Tx, prior drops.Results) (any, error) {
			order = append(order, "audit")
			tm, err := drops.StepResult[*team](prior, "team")
			if err != nil {
				return nil, err
			}
			_, _ = tx.Exec(context.Background(), "INSERT INTO audit")
			return tm.ID, nil
		})

	d := &recordingDriver{}
	res, err := m.Exec(context.Background(), d)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if strings.Join(order, ",") != "team,audit" {
		t.Errorf("steps ran %v, want [team audit]", order)
	}
	if d.begins != 1 || d.tx.committed != 1 || d.tx.rolledBack != 0 {
		t.Errorf("begins=%d commits=%d rollbacks=%d, want 1/1/0",
			d.begins, d.tx.committed, d.tx.rolledBack)
	}
	if len(d.tx.execs) != 2 {
		t.Errorf("both steps should have used the one transaction, saw %v", d.tx.execs)
	}
	tm, err := drops.StepResult[*team](res, "team")
	if err != nil || tm.ID != 7 {
		t.Errorf("StepResult[*team] = %v, %v", tm, err)
	}
	id, err := drops.StepResult[int64](res, "audit")
	if err != nil || id != 7 {
		t.Errorf("StepResult[int64] = %v, %v", id, err)
	}
	if got := strings.Join(res.Names(), ","); got != "team,audit" {
		t.Errorf("Results.Names() = %q", got)
	}
	res.Names()[0] = "mutated"
	if res.Names()[0] != "team" {
		t.Error("Results.Names() handed out the internal slice")
	}
}

// The whole point of the type: the failure says which step, and what
// had already run.
func TestMultiErrorNamesTheStepAndCarriesTheCompletedOnes(t *testing.T) {
	boom := errors.New("constraint violated")
	ran := 0
	m := drops.NewMulti().
		Run("team", func(context.Context, drops.Tx, drops.Results) (any, error) {
			ran++
			return &team{ID: 7}, nil
		}).
		Run("seats", func(context.Context, drops.Tx, drops.Results) (any, error) {
			ran++
			return 3, nil
		}).
		Run("audit", func(context.Context, drops.Tx, drops.Results) (any, error) {
			ran++
			return nil, boom
		}).
		Run("email", func(context.Context, drops.Tx, drops.Results) (any, error) {
			ran++
			return nil, nil
		})

	d := &recordingDriver{}
	res, err := m.Exec(context.Background(), d)
	if err == nil {
		t.Fatal("expected an error")
	}
	if ran != 3 {
		t.Errorf("%d steps ran; the one after the failure must not", ran)
	}
	if d.tx.rolledBack != 1 || d.tx.committed != 0 {
		t.Errorf("rollbacks=%d commits=%d, want 1/0", d.tx.rolledBack, d.tx.committed)
	}

	var me *drops.MultiError
	if !errors.As(err, &me) {
		t.Fatalf("error is %T, want *drops.MultiError", err)
	}
	if me.FailedStep != "audit" || me.FailedStepIdx != 2 {
		t.Errorf("failed step = %q at %d, want audit at 2", me.FailedStep, me.FailedStepIdx)
	}
	if !errors.Is(err, boom) {
		t.Error("errors.Is should see through to the step's own error")
	}
	if !drops.IsMultiError(err) {
		t.Error("IsMultiError said no")
	}
	if got := strings.Join(me.Completed.Names(), ","); got != "team,seats" {
		t.Errorf("Completed = %q, want the two steps that had succeeded", got)
	}
	if _, err := drops.StepResult[*team](me.Completed, "team"); err != nil {
		t.Errorf("the completed team should still be readable: %v", err)
	}
	if me.Completed.Has("audit") {
		t.Error("the failing step must not appear in Completed")
	}
	// The same values are handed back positionally, so a caller who
	// only wanted "how far did it get" need not type-assert the error.
	if got := strings.Join(res.Names(), ","); got != "team,seats" {
		t.Errorf("returned Results = %q", got)
	}
	if !strings.Contains(me.Error(), "audit") || !strings.Contains(me.Error(), boom.Error()) {
		t.Errorf("message hides the useful part: %s", me.Error())
	}
}

// A step gets the results of the steps before it and nothing else —
// and the view it was handed stays that way, so a MultiError's
// Completed still describes the moment the step ran and not whatever
// the accumulator became afterwards.
func TestMultiPriorIsASnapshot(t *testing.T) {
	var midView drops.Results
	m := drops.NewMulti().
		Run("a", func(_ context.Context, _ drops.Tx, prior drops.Results) (any, error) {
			return 1, nil
		}).
		Run("b", func(_ context.Context, _ drops.Tx, prior drops.Results) (any, error) {
			if !prior.Has("a") || prior.Has("b") {
				t.Errorf("step b saw %v", prior.Names())
			}
			midView = prior
			return 2, nil
		}).
		Run("c", func(context.Context, drops.Tx, drops.Results) (any, error) {
			return 3, nil
		})
	if _, err := m.Exec(context.Background(), &recordingDriver{}); err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if got := strings.Join(midView.Names(), ","); got != "a" {
		t.Errorf("the view step b was handed grew to %q after b returned", got)
	}
	if midView.Has("b") || midView.Has("c") {
		t.Error("later steps wrote into an earlier step's snapshot")
	}
}

// A Multi is a value, so running it twice runs the same declared list
// again — the property that makes it safe to hand to a retry loop.
func TestMultiIsReRunnable(t *testing.T) {
	n := 0
	m := drops.NewMulti().Run("bump", func(context.Context, drops.Tx, drops.Results) (any, error) {
		n++
		return n, nil
	})
	for i := 1; i <= 2; i++ {
		res, err := m.Exec(context.Background(), &recordingDriver{})
		if err != nil {
			t.Fatalf("attempt %d: %v", i, err)
		}
		got, err := drops.StepResult[int](res, "bump")
		if err != nil || got != i {
			t.Errorf("attempt %d returned %v (%v)", i, got, err)
		}
	}
}

// Inspection without a database: the assertion CI can make about a
// transaction script.
func TestMultiNamesAreInspectableBeforeRunning(t *testing.T) {
	m := drops.NewMulti().
		Run("charge", func(context.Context, drops.Tx, drops.Results) (any, error) { return nil, nil }).
		Run("ship", func(context.Context, drops.Tx, drops.Results) (any, error) { return nil, nil })
	if got := strings.Join(m.Names(), ","); got != "charge,ship" {
		t.Errorf("Names() = %q", got)
	}
	if m.Len() != 2 {
		t.Errorf("Len() = %d", m.Len())
	}
}

func TestMultiWithNoStepsNeverOpensATransaction(t *testing.T) {
	d := &recordingDriver{}
	res, err := drops.NewMulti().Exec(context.Background(), d)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if d.begins != 0 {
		t.Errorf("opened %d transactions for an empty Multi", d.begins)
	}
	if res.Len() != 0 {
		t.Errorf("empty Multi produced %v", res.Names())
	}
}

func TestMultiAppendComposesTwoScripts(t *testing.T) {
	a := drops.NewMulti().Run("a", func(context.Context, drops.Tx, drops.Results) (any, error) { return 1, nil })
	b := drops.NewMulti().Run("b", func(context.Context, drops.Tx, drops.Results) (any, error) { return 2, nil })
	both := drops.NewMulti().Append(a).Append(b)
	if got := strings.Join(both.Names(), ","); got != "a,b" {
		t.Errorf("Names() = %q", got)
	}
	if a.Len() != 1 {
		t.Error("Append modified the source Multi")
	}
	res, err := both.Exec(context.Background(), &recordingDriver{})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if res.Len() != 2 {
		t.Errorf("ran %v", res.Names())
	}
}

// Construction mistakes are program text, not run-time conditions.
func TestMultiRefusesBadSteps(t *testing.T) {
	cases := map[string]func(){
		"duplicate name": func() {
			drops.NewMulti().
				Run("x", func(context.Context, drops.Tx, drops.Results) (any, error) { return nil, nil }).
				Run("x", func(context.Context, drops.Tx, drops.Results) (any, error) { return nil, nil })
		},
		"nil function": func() { drops.NewMulti().Run("x", nil) },
		"empty name": func() {
			drops.NewMulti().Run("", func(context.Context, drops.Tx, drops.Results) (any, error) { return nil, nil })
		},
		"append collides": func() {
			a := drops.NewMulti().Run("x", func(context.Context, drops.Tx, drops.Results) (any, error) { return nil, nil })
			drops.NewMulti().
				Run("x", func(context.Context, drops.Tx, drops.Results) (any, error) { return nil, nil }).
				Append(a)
		},
	}
	for name, fn := range cases {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Error("expected a panic")
				}
			}()
			fn()
		})
	}
}

// Typed steps: the compiler agrees the writer and the reader mean the
// same type, so the run-time check cannot fail.
func TestStepIsTypedEndToEnd(t *testing.T) {
	m := drops.NewMulti()
	drops.Step(m, "team", func(context.Context, drops.Tx, drops.Results) (*team, error) {
		return &team{ID: 42}, nil
	})
	drops.Step(m, "seats", func(_ context.Context, _ drops.Tx, prior drops.Results) (int, error) {
		tm, err := drops.StepResult[*team](prior, "team")
		if err != nil {
			return 0, err
		}
		return int(tm.ID), nil
	})
	res, err := m.Exec(context.Background(), &recordingDriver{})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	seats, err := drops.StepResult[int](res, "seats")
	if err != nil || seats != 42 {
		t.Errorf("seats = %d (%v)", seats, err)
	}
}

// "It never ran" and "it returned something else" are different bugs.
func TestStepResultDistinguishesMissingFromMistyped(t *testing.T) {
	m := drops.NewMulti().Run("n", func(context.Context, drops.Tx, drops.Results) (any, error) {
		return 3, nil
	}).Run("nothing", func(context.Context, drops.Tx, drops.Results) (any, error) {
		return nil, nil
	})
	res, err := m.Exec(context.Background(), &recordingDriver{})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}

	if _, err := drops.StepResult[int](res, "absent"); !errors.Is(err, drops.ErrNoSuchStep) {
		t.Errorf("missing step gave %v, want ErrNoSuchStep", err)
	}
	got, err := drops.StepResult[string](res, "n")
	if !errors.Is(err, drops.ErrStepResultType) {
		t.Errorf("mistyped read gave %v, want ErrStepResultType", err)
	}
	if got != "" {
		t.Errorf("a failed read should yield the zero value, got %q", got)
	}
	if !strings.Contains(err.Error(), "int") {
		t.Errorf("the type error should say what was actually there: %v", err)
	}
	// A step that returned nothing is present and yields the zero value.
	p, err := drops.StepResult[*team](res, "nothing")
	if err != nil || p != nil {
		t.Errorf("nil result read back as %v (%v)", p, err)
	}
}

// ExecIn leaves the transaction alone: nesting and retry loops decide
// for themselves.
func TestExecInDoesNotEndTheTransaction(t *testing.T) {
	tx := &recordingTx{}
	m := drops.NewMulti().Run("a", func(context.Context, drops.Tx, drops.Results) (any, error) {
		return 1, nil
	})
	res, err := m.ExecIn(context.Background(), tx)
	if err != nil {
		t.Fatalf("ExecIn: %v", err)
	}
	if res.Len() != 1 {
		t.Errorf("ran %v", res.Names())
	}
	if tx.committed != 0 || tx.rolledBack != 0 {
		t.Errorf("ExecIn touched the transaction: commits=%d rollbacks=%d", tx.committed, tx.rolledBack)
	}
}

// A failed commit is not a MultiError: no step failed, so there is no
// step to name. It is the driver's error as it came, and the Results
// are complete rather than partial — which is how a caller tells "the
// script stopped early" from "the script finished and the database
// refused it".
func TestMultiCommitFailureIsNotAStepFailure(t *testing.T) {
	boom := errors.New("could not serialize access")
	d := &commitFailDriver{err: boom}
	m := drops.NewMulti().
		Run("a", func(context.Context, drops.Tx, drops.Results) (any, error) { return 1, nil }).
		Run("b", func(context.Context, drops.Tx, drops.Results) (any, error) { return 2, nil })

	res, err := m.Exec(context.Background(), d)
	if !errors.Is(err, boom) {
		t.Fatalf("Exec returned %v, want the commit error", err)
	}
	if drops.IsMultiError(err) {
		t.Error("a commit failure was dressed up as a step failure")
	}
	if got := strings.Join(res.Names(), ","); got != "a,b" {
		t.Errorf("Results = %q, want every step — none of them failed", got)
	}
}

type commitFailDriver struct {
	recordingDriver
	err error
}

func (d *commitFailDriver) Begin(ctx context.Context) (drops.Tx, error) {
	tx, err := d.recordingDriver.Begin(ctx)
	if err != nil {
		return nil, err
	}
	d.recordingDriver.tx.commitErr = d.err
	return tx, nil
}
