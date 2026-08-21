package pg_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/bernardoforcillo/drops"
	"github.com/bernardoforcillo/drops/pg"
)

// Invoice schema fixture used across guard tests:
//
//	invoices    (id, organizationId, createdBy, total)
//	org_members (userId, organizationId)
//
// A user can read / mutate an invoice if EITHER they created it
// OR they're a member of its organization.
type guardInvoice struct {
	ID             int64 `drop:"id,primaryKey,autoIncrement"`
	OrganizationID int64 `drop:"organizationId,notNull"`
	CreatedBy      int64 `drop:"createdBy,notNull"`
	Total          int64 `drop:"total,notNull"`
}

func guardSchema(t *testing.T) (entity *pg.Entity[guardInvoice], invoiceTbl, memberTbl *pg.Table) {
	t.Helper()
	invoices := pg.AutoTable[guardInvoice]("invoices")

	members := pg.NewTable("org_members")
	pg.Add(members, pg.BigInt("userId").NotNull())
	pg.Add(members, pg.BigInt("organizationId").NotNull())

	ent := pg.NewEntity[guardInvoice](invoices)
	return ent, invoices, members
}

func TestOwnerGuardInjectsPredicate(t *testing.T) {
	ent, invoices, _ := guardSchema(t)
	ent.AuthorizeWith(pg.OwnerGuard{Owner: invoices.Col("createdBy")})

	fd := &fakeDriver{handler: func(string, []any) (drops.Rows, error) {
		return &fakeRows{
			cols: []string{"id", "organizationId", "createdBy", "total"},
			data: [][]any{{int64(1), int64(7), int64(42), int64(100)}},
		}, nil
	}}
	db := pg.New(fd)
	ctx := pg.WithSubject(context.Background(), int64(42))
	if _, err := ent.Get(db, ctx, int64(1)); err != nil {
		t.Fatalf("Get: %v", err)
	}
	sql := fd.queries[0]
	if !strings.Contains(sql, `"invoices"."createdBy" = $`) {
		t.Errorf("OwnerGuard predicate missing: %s", sql)
	}
}

func TestMembershipGuardInjectsSubquery(t *testing.T) {
	ent, invoices, members := guardSchema(t)
	ent.AuthorizeWith(pg.MembershipGuard{
		Junction:         members,
		JunctionSubject:  members.Col("userId"),
		JunctionResource: members.Col("organizationId"),
		ResourceOwner:    invoices.Col("organizationId"),
	})

	fd := &fakeDriver{handler: func(string, []any) (drops.Rows, error) {
		return &fakeRows{
			cols: []string{"id", "organizationId", "createdBy", "total"},
		}, nil
	}}
	db := pg.New(fd)
	ctx := pg.WithSubject(context.Background(), int64(42))
	if _, err := ent.Query(db).All(ctx); err != nil {
		t.Fatalf("All: %v", err)
	}
	sql := fd.queries[0]
	// The junction subquery is composed as a *SelectBuilder rather than
	// written out as SQL text, so its columns are qualified and its
	// predicate is parenthesised the way every other predicate in the
	// package is. That is what these two fragments say now, and it is
	// what makes the junction table's own DefaultFilters and
	// ContextFilters apply to the membership check — see
	// MembershipGuard.Predicate, and authzscope_test.go for a junction
	// table that has some.
	wantFragments := []string{
		`"invoices"."organizationId" IN`,
		`SELECT "org_members"."organizationId" FROM "org_members"`,
		`WHERE ("org_members"."userId" = $`,
	}
	for _, w := range wantFragments {
		if !strings.Contains(sql, w) {
			t.Errorf("missing fragment %q in:\n%s", w, sql)
		}
	}
	// The whole statement and the argument bound with it. The fragments
	// say the membership check is present; this says nothing else is,
	// and that what was bound is the subject rather than something that
	// merely renders like one.
	want := `SELECT * FROM "invoices" WHERE ("invoices"."organizationId" IN (` +
		`SELECT "org_members"."organizationId" FROM "org_members" ` +
		`WHERE ("org_members"."userId" = $1)))`
	if sql != want {
		t.Errorf("got = %v, want %v", sql, want)
	}
	if !sameArgs(fd.args[0], []any{int64(42)}) {
		t.Errorf("args = %v, want %v", fd.args[0], []any{int64(42)})
	}
}

func TestAnyOfCombinesGuardsWithOR(t *testing.T) {
	ent, invoices, members := guardSchema(t)
	ent.AuthorizeWith(pg.AnyOf(
		pg.OwnerGuard{Owner: invoices.Col("createdBy")},
		pg.MembershipGuard{
			Junction:         members,
			JunctionSubject:  members.Col("userId"),
			JunctionResource: members.Col("organizationId"),
			ResourceOwner:    invoices.Col("organizationId"),
		},
	))

	fd := &fakeDriver{handler: func(string, []any) (drops.Rows, error) {
		return &fakeRows{
			cols: []string{"id", "organizationId", "createdBy", "total"},
			data: [][]any{{int64(1), int64(7), int64(42), int64(100)}},
		}, nil
	}}
	db := pg.New(fd)
	ctx := pg.WithSubject(context.Background(), int64(42))
	if _, err := ent.Get(db, ctx, int64(1)); err != nil {
		t.Fatalf("Get: %v", err)
	}
	sql := fd.queries[0]
	if !strings.Contains(sql, " OR ") {
		t.Errorf("AnyOf must OR predicates: %s", sql)
	}
	if !strings.Contains(sql, "createdBy") || !strings.Contains(sql, `organizationId" IN`) {
		t.Errorf("both guard predicates must appear: %s", sql)
	}
}

func TestAllOfCombinesGuardsWithAND(t *testing.T) {
	ent, invoices, _ := guardSchema(t)
	hasRole := pg.CustomGuard(func(ctx context.Context) (drops.Expression, error) {
		return drops.Raw(`("invoices"."total" >= 0)`), nil
	})
	ent.AuthorizeWith(pg.AllOf(
		pg.OwnerGuard{Owner: invoices.Col("createdBy")},
		hasRole,
	))

	fd := &fakeDriver{handler: func(string, []any) (drops.Rows, error) {
		return &fakeRows{
			cols: []string{"id", "organizationId", "createdBy", "total"},
		}, nil
	}}
	db := pg.New(fd)
	ctx := pg.WithSubject(context.Background(), int64(42))
	if _, err := ent.Query(db).All(ctx); err != nil {
		t.Fatalf("All: %v", err)
	}
	sql := fd.queries[0]
	if !strings.Contains(sql, " AND ") {
		t.Errorf("AllOf must AND predicates: %s", sql)
	}
}

func TestGuardWithoutSubjectErrors(t *testing.T) {
	ent, invoices, _ := guardSchema(t)
	ent.AuthorizeWith(pg.OwnerGuard{Owner: invoices.Col("createdBy")})
	db := pg.New(&fakeDriver{})
	_, err := ent.Get(db, context.Background(), int64(1))
	if !errors.Is(err, pg.ErrSubjectMissing) {
		t.Errorf("expected ErrSubjectMissing, got %v", err)
	}
}

func TestGuardOnUpdate(t *testing.T) {
	ent, invoices, _ := guardSchema(t)
	ent.AuthorizeWith(pg.OwnerGuard{Owner: invoices.Col("createdBy")})
	fd := &fakeDriver{handler: func(string, []any) (drops.Rows, error) {
		return &fakeRows{
			cols: []string{"id", "organizationId", "createdBy", "total"},
			data: [][]any{{int64(1), int64(7), int64(42), int64(100)}},
		}, nil
	}}
	db := pg.New(fd)
	ctx := pg.WithSubject(context.Background(), int64(42))
	inv := guardInvoice{ID: 1, OrganizationID: 7, CreatedBy: 42, Total: 100}
	if err := ent.Update(db, ctx, &inv); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if !strings.Contains(fd.queries[0], `"createdBy" = $`) {
		t.Errorf("Update must include guard predicate: %s", fd.queries[0])
	}
}

func TestGuardOnDelete(t *testing.T) {
	ent, invoices, _ := guardSchema(t)
	ent.AuthorizeWith(pg.OwnerGuard{Owner: invoices.Col("createdBy")})
	fd := &fakeDriver{}
	db := pg.New(fd)
	ctx := pg.WithSubject(context.Background(), int64(42))
	if _, err := ent.Delete(db, ctx, int64(1)); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if !strings.Contains(fd.queries[0], `"createdBy" = $`) {
		t.Errorf("Delete must include guard predicate: %s", fd.queries[0])
	}
}

func TestSubjectContextRoundTrip(t *testing.T) {
	ctx := pg.WithSubject(context.Background(), "user-7")
	v, ok := pg.SubjectFrom(ctx)
	if !ok || v != "user-7" {
		t.Errorf("subject from ctx: %v, %v", v, ok)
	}
}

func TestCustomGuardError(t *testing.T) {
	ent, _, _ := guardSchema(t)
	boom := errors.New("denied")
	ent.AuthorizeWith(pg.CustomGuard(func(ctx context.Context) (drops.Expression, error) {
		return nil, boom
	}))
	db := pg.New(&fakeDriver{})
	_, err := ent.Get(db, pg.WithSubject(context.Background(), 1), int64(1))
	if !errors.Is(err, boom) {
		t.Errorf("custom guard error must surface, got %v", err)
	}
}

func TestUnguardedEntityIsUnaffected(t *testing.T) {
	_, ent := entUsersSchema()
	fd := &fakeDriver{handler: func(string, []any) (drops.Rows, error) {
		return &fakeRows{cols: []string{"id", "name", "email"}, data: [][]any{{int64(1), "x", "x@x"}}}, nil
	}}
	db := pg.New(fd)
	if _, err := ent.Get(db, context.Background(), int64(1)); err != nil {
		t.Fatalf("Get: %v", err)
	}
}
