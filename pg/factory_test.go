package pg_test

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/bernardoforcillo/drops"
	"github.com/bernardoforcillo/drops/pg"
)

type factoryUser struct {
	ID    int64  `drop:"id"`
	Name  string `drop:"name"`
	Email string `drop:"email"`
	Role  string `drop:"role"`
}

func factoryEntity() *pg.Entity[factoryUser] {
	t := pg.NewTable("factoryUsers")
	pg.Add(t, pg.BigSerial("id").PrimaryKey())
	pg.Add(t, pg.Text("name").NotNull())
	pg.Add(t, pg.Text("email").NotNull())
	pg.Add(t, pg.Text("role").NotNull())
	return pg.NewEntity[factoryUser](t)
}

func TestFactoryBuildProducesDistinctSequenceValues(t *testing.T) {
	f := pg.NewFactory(factoryEntity(), func(seq int) factoryUser {
		return factoryUser{Name: fmt.Sprintf("u-%d", seq), Email: fmt.Sprintf("u-%d@x", seq), Role: "member"}
	})
	a := f.Build()
	b := f.Build()
	if a.Name == b.Name {
		t.Errorf("expected distinct names, got %q == %q", a.Name, b.Name)
	}
	if a.Name != "u-1" || b.Name != "u-2" {
		t.Errorf("seq names: %q %q", a.Name, b.Name)
	}
}

func TestFactoryBuildNFillsBatch(t *testing.T) {
	f := pg.NewFactory(factoryEntity(), func(seq int) factoryUser {
		return factoryUser{Name: fmt.Sprintf("u-%d", seq), Email: "x", Role: "member"}
	})
	rows := f.BuildN(5)
	if len(rows) != 5 {
		t.Fatalf("len: %d", len(rows))
	}
	seen := map[string]bool{}
	for _, r := range rows {
		if seen[r.Name] {
			t.Errorf("duplicate name in batch: %q", r.Name)
		}
		seen[r.Name] = true
	}
}

func TestFactoryWithMutatesEachBuild(t *testing.T) {
	base := pg.NewFactory(factoryEntity(), func(seq int) factoryUser {
		return factoryUser{Name: fmt.Sprintf("u-%d", seq), Role: "member"}
	})
	admins := base.With(func(u *factoryUser) { u.Role = "admin" })

	a := admins.Build()
	if a.Role != "admin" {
		t.Errorf("admin role: %q", a.Role)
	}
}

func TestFactoryWithSharesSequenceCounter(t *testing.T) {
	base := pg.NewFactory(factoryEntity(), func(seq int) factoryUser {
		return factoryUser{Name: fmt.Sprintf("u-%d", seq)}
	})
	child := base.With(func(*factoryUser) {})

	u1 := base.Build()
	u2 := child.Build()
	if u1.Name == u2.Name {
		t.Errorf("parent and child must share seq counter so names differ; got %q == %q", u1.Name, u2.Name)
	}
}

func TestFactoryResetRewindsCounter(t *testing.T) {
	f := pg.NewFactory(factoryEntity(), func(seq int) factoryUser {
		return factoryUser{Name: fmt.Sprintf("u-%d", seq)}
	})
	_ = f.BuildN(10)
	if f.Sequence() != 10 {
		t.Errorf("sequence after BuildN(10): %d", f.Sequence())
	}
	f.Reset()
	if f.Sequence() != 0 {
		t.Errorf("sequence after reset: %d", f.Sequence())
	}
	v := f.Build()
	if v.Name != "u-1" {
		t.Errorf("post-reset name: %q", v.Name)
	}
}

func TestFactoryCreateNUsesBatchInsert(t *testing.T) {
	drv := &captureDriver{}
	db := pg.New(drv)
	f := pg.NewFactory(factoryEntity(), func(seq int) factoryUser {
		return factoryUser{Name: fmt.Sprintf("u-%d", seq), Email: "x@x", Role: "member"}
	})
	if _, err := f.CreateN(context.Background(), db, 5); err != nil {
		t.Fatalf("CreateN: %v", err)
	}
	if drv.execs.Load() != 1 {
		t.Errorf("CreateN should issue 1 Exec (batch), got %d", drv.execs.Load())
	}
	if !strings.Contains(drv.lastSQL.Load().(string), "INSERT INTO") {
		t.Errorf("expected INSERT, got: %v", drv.lastSQL.Load())
	}
}

func TestFactoryConcurrentBuildIsSafe(t *testing.T) {
	f := pg.NewFactory(factoryEntity(), func(seq int) factoryUser {
		return factoryUser{Name: fmt.Sprintf("u-%d", seq)}
	})
	const n = 100
	var wg sync.WaitGroup
	results := make(chan string, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results <- f.Build().Name
		}()
	}
	wg.Wait()
	close(results)
	seen := map[string]bool{}
	for name := range results {
		if seen[name] {
			t.Errorf("duplicate concurrent name: %q", name)
		}
		seen[name] = true
	}
}

// captureDriver counts Exec calls and stores the last SQL — enough
// to assert factory plumbing without a real database.
type captureDriver struct {
	execs   atomic.Int64
	lastSQL atomic.Value
}

func (d *captureDriver) Exec(_ context.Context, sql string, _ ...any) (drops.Result, error) {
	d.execs.Add(1)
	d.lastSQL.Store(sql)
	return captureResult{}, nil
}
func (d *captureDriver) Query(context.Context, string, ...any) (drops.Rows, error) {
	return nil, nil
}
func (d *captureDriver) Begin(context.Context) (drops.Tx, error) { return nil, nil }

type captureResult struct{}

func (captureResult) RowsAffected() (int64, error) { return 1, nil }
