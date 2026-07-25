package sqlite_test

import (
	"context"
	"strings"
	"testing"

	"github.com/bernardoforcillo/drops/cache/memory"
	"github.com/bernardoforcillo/drops/sqlite"
)

// --- authz ------------------------------------------------------------

func TestAuthzGuard(t *testing.T) {
	tbl := sqlite.NewTable("posts")
	sqlite.Add(tbl, sqlite.BigInt("id").PrimaryKey())
	owner := sqlite.Add(tbl, sqlite.BigInt("authorId").NotNull())
	sqlite.Add(tbl, sqlite.Text("body"))
	ent := sqlite.NewEntity[postRow](tbl).
		AuthorizeWith(sqlite.OwnerGuard{Owner: owner.Column})

	db := sqlite.New(&entDriver{})
	// No subject → fail closed.
	if _, err := ent.Get(db, context.Background(), int64(1)); err != sqlite.ErrSubjectMissing {
		t.Errorf("want ErrSubjectMissing, got %v", err)
	}
	// With subject → guard predicate is AND-ed in.
	drv := &entDriver{}
	db2 := sqlite.New(drv)
	ctx := sqlite.WithSubject(context.Background(), int64(7))
	_, _ = ent.Get(db2, ctx, int64(1))
	q := drv.queries[0]
	if !strings.Contains(q, `("posts"."authorId" = ?)`) {
		t.Errorf("guard predicate absent:\n%s", q)
	}
}

func TestAuthzCompose(t *testing.T) {
	tbl := sqlite.NewTable("posts")
	sqlite.Add(tbl, sqlite.BigInt("id").PrimaryKey())
	a := sqlite.Add(tbl, sqlite.BigInt("authorId"))
	b := sqlite.Add(tbl, sqlite.BigInt("editorId"))
	g := sqlite.AnyOf(
		sqlite.OwnerGuard{Owner: a.Column},
		sqlite.OwnerGuard{Owner: b.Column},
	)
	pred, err := g.Predicate(sqlite.WithSubject(context.Background(), int64(1)))
	if err != nil {
		t.Fatal(err)
	}
	sql, _ := sqlite.ToSQL(pred)
	if !strings.Contains(sql, "OR") || !strings.Contains(sql, "authorId") || !strings.Contains(sql, "editorId") {
		t.Errorf("AnyOf composition:\n%s", sql)
	}
}

type postRow struct {
	ID       int64  `drop:"id"`
	AuthorID int64  `drop:"authorId"`
	Body     string `drop:"body"`
}

// --- audit ------------------------------------------------------------

func TestAuditRecordsInTx(t *testing.T) {
	tbl := sqlite.NewTable("users")
	sqlite.Add(tbl, sqlite.BigInt("id").PrimaryKey())
	sqlite.Add(tbl, sqlite.Text("name").NotNull())
	ent := sqlite.NewEntity[entUser](tbl)
	audit := sqlite.NewAuditLog(sqlite.New(&entDriver{}), "audit_events")
	sqlite.WithAudit(ent, audit)

	drv := &entDriver{}
	db := sqlite.New(drv)
	ctx := sqlite.WithActor(context.Background(), "admin-9")
	if err := ent.Create(db, ctx, &entUser{ID: 1, Name: "a"}); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(drv.queries, "\n")
	if !strings.Contains(joined, `INSERT INTO "users"`) || !strings.Contains(joined, `INSERT INTO "audit_events"`) {
		t.Errorf("audit not recorded alongside insert:\n%s", joined)
	}
	// The audit row carries the actor.
	var sawActor bool
	for _, a := range drv.args {
		for _, v := range a {
			if v == "admin-9" {
				sawActor = true
			}
		}
	}
	if !sawActor {
		t.Error("actor not bound on audit row")
	}
}

func TestActorFrom(t *testing.T) {
	if got := sqlite.ActorFrom(context.Background()); got != "" {
		t.Errorf("empty ctx actor: %q", got)
	}
	ctx := sqlite.WithActor(context.Background(), 42)
	if got := sqlite.ActorFrom(ctx); got != "42" {
		t.Errorf("actor: %q", got)
	}
}

// --- cache ------------------------------------------------------------

func TestEntityCache(t *testing.T) {
	tbl := sqlite.NewTable("users")
	sqlite.Add(tbl, sqlite.BigInt("id").PrimaryKey())
	sqlite.Add(tbl, sqlite.Text("name").NotNull())
	ent := sqlite.NewEntity[entUser](tbl).WithCache(memory.New(), 0)

	drv := &entDriver{rows: &entRows{
		cols: []string{"id", "name"},
		data: [][]any{{int64(1), "alice"}},
	}}
	db := sqlite.New(drv)
	ctx := context.Background()

	u1, err := ent.Get(db, ctx, int64(1))
	if err != nil {
		t.Fatal(err)
	}
	if u1.Name != "alice" {
		t.Fatalf("got %+v", u1)
	}
	queriesAfterFirst := len(drv.queries)

	// Reset the cursor so a second DB read (if it happened) would work,
	// then Get again — it must be served from cache with no new query.
	drv.rows.pos = 0
	u2, err := ent.Get(db, ctx, int64(1))
	if err != nil {
		t.Fatal(err)
	}
	if u2.Name != "alice" {
		t.Fatalf("cached got %+v", u2)
	}
	if len(drv.queries) != queriesAfterFirst {
		t.Errorf("cache miss on second Get: %d → %d queries", queriesAfterFirst, len(drv.queries))
	}
}
