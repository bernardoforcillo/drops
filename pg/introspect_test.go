package pg_test

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/bernardoforcillo/drops"
	"github.com/bernardoforcillo/drops/pg"
)

// --- IF EXISTS / IF NOT EXISTS in Diff -------------------------------

func TestDiffSafeIfNotExistsCreateTable(t *testing.T) {
	users := pg.NewTable("users")
	pg.Add(users, pg.BigSerial("id").PrimaryKey())
	pg.Add(users, pg.Text("name").NotNull())

	stmts := pg.Diff(pg.EmptySnapshot(), pg.BuildSnapshot(pg.NewSchema(users)), pg.DiffOptions{Safe: true})
	if len(stmts) != 1 || !strings.HasPrefix(stmts[0], `CREATE TABLE IF NOT EXISTS "users"`) {
		t.Errorf("expected CREATE TABLE IF NOT EXISTS, got: %v", stmts)
	}
}

func TestDiffSafeIfExistsDropTable(t *testing.T) {
	users := pg.NewTable("users")
	pg.Add(users, pg.BigSerial("id").PrimaryKey())
	prev := pg.BuildSnapshot(pg.NewSchema(users))

	stmts := pg.Diff(prev, pg.EmptySnapshot(), pg.DiffOptions{Safe: true})
	if len(stmts) != 1 || stmts[0] != `DROP TABLE IF EXISTS "users" CASCADE;` {
		t.Errorf("expected DROP TABLE IF EXISTS, got: %v", stmts)
	}
}

func TestDiffSafeIfNotExistsAddColumn(t *testing.T) {
	before := pg.NewTable("users")
	pg.Add(before, pg.BigSerial("id").PrimaryKey())
	prev := pg.BuildSnapshot(pg.NewSchema(before))

	after := pg.NewTable("users")
	pg.Add(after, pg.BigSerial("id").PrimaryKey())
	pg.Add(after, pg.Integer("age"))
	cur := pg.BuildSnapshot(pg.NewSchema(after))

	stmts := pg.Diff(prev, cur, pg.DiffOptions{Safe: true})
	want := `ALTER TABLE "users" ADD COLUMN IF NOT EXISTS "age" integer;`
	if len(stmts) != 1 || stmts[0] != want {
		t.Errorf("expected %q, got %v", want, stmts)
	}
}

func TestDiffSafeIfExistsDropColumnAndConstraint(t *testing.T) {
	before := pg.NewTable("users")
	pg.Add(before, pg.BigSerial("id").PrimaryKey())
	pg.Add(before, pg.Text("email").NotNull().Unique())
	pg.Add(before, pg.Text("nickname"))
	prev := pg.BuildSnapshot(pg.NewSchema(before))

	after := pg.NewTable("users")
	pg.Add(after, pg.BigSerial("id").PrimaryKey())
	pg.Add(after, pg.Text("email").NotNull()) // unique removed
	cur := pg.BuildSnapshot(pg.NewSchema(after))

	stmts := pg.Diff(prev, cur, pg.DiffOptions{Safe: true})
	joined := strings.Join(stmts, "\n")
	for _, want := range []string{
		`ALTER TABLE "users" DROP COLUMN IF EXISTS "nickname";`,
		`ALTER TABLE "users" DROP CONSTRAINT IF EXISTS "usersEmailUnique";`,
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q in:\n%s", want, joined)
		}
	}
}

// --- Existence helpers ----------------------------------------------

func TestExistsHelpersReturnTrueWhenRowFound(t *testing.T) {
	fd := &fakeDriver{handler: func(sql string, _ []any) (drops.Rows, error) {
		// Any matching SELECT — return a single row to signal "exists".
		return &fakeRows{cols: []string{"?"}, data: [][]any{{int64(1)}}}, nil
	}}
	db := pg.New(fd)
	ctx := context.Background()

	cases := []struct {
		name string
		fn   func() (bool, error)
	}{
		{"SchemaExists", func() (bool, error) { return pg.SchemaExists(ctx, db, "public") }},
		{"TableExists", func() (bool, error) { return pg.TableExists(ctx, db, "", "users") }},
		{"ColumnExists", func() (bool, error) { return pg.ColumnExists(ctx, db, "", "users", "id") }},
		{"ConstraintExists", func() (bool, error) {
			return pg.ConstraintExists(ctx, db, "", "users", "users_pkey")
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.fn()
			if err != nil {
				t.Fatal(err)
			}
			if !got {
				t.Error("expected true")
			}
		})
	}
}

func TestExistsHelpersReturnFalseWhenNoRow(t *testing.T) {
	fd := &fakeDriver{handler: func(string, []any) (drops.Rows, error) {
		return &fakeRows{cols: []string{"?"}}, nil
	}}
	db := pg.New(fd)
	ctx := context.Background()

	if ok, _ := pg.TableExists(ctx, db, "", "missing"); ok {
		t.Error("TableExists: expected false")
	}
	if ok, _ := pg.ColumnExists(ctx, db, "", "users", "ghost"); ok {
		t.Error("ColumnExists: expected false")
	}
}

func TestTableExistsDefaultsToPublic(t *testing.T) {
	var capturedSchema string
	fd := &fakeDriver{handler: func(_ string, args []any) (drops.Rows, error) {
		if len(args) > 0 {
			capturedSchema = args[0].(string)
		}
		return &fakeRows{cols: []string{"?"}}, nil
	}}
	db := pg.New(fd)
	if _, err := pg.TableExists(context.Background(), db, "", "users"); err != nil {
		t.Fatal(err)
	}
	if capturedSchema != "public" {
		t.Errorf("expected schema arg 'public', got %q", capturedSchema)
	}
}

// --- Introspection --------------------------------------------------

// introspectFake routes information_schema queries to canned responses
// for an imagined two-table database.
func introspectFake() *fakeDriver {
	return &fakeDriver{handler: func(q string, _ []any) (drops.Rows, error) {
		switch {
		case strings.Contains(q, "information_schema.tables"):
			return &fakeRows{
				cols: []string{"table_schema", "table_name"},
				data: [][]any{
					{"public", "users"},
					{"public", "posts"},
				},
			}, nil
		case strings.Contains(q, "information_schema.columns"):
			return &fakeRows{
				cols: []string{
					"table_schema", "table_name", "column_name",
					"udt_name", "character_maximum_length",
					"numeric_precision", "numeric_scale",
					"is_nullable", "column_default",
				},
				data: [][]any{
					{"public", "users", "id", "int8",
						sql.NullInt64{}, sql.NullInt64{Int64: 64, Valid: true}, sql.NullInt64{Int64: 0, Valid: true},
						"NO", sql.NullString{String: "nextval('users_id_seq'::regclass)", Valid: true}},
					{"public", "users", "name", "text",
						sql.NullInt64{}, sql.NullInt64{}, sql.NullInt64{},
						"NO", sql.NullString{}},
					{"public", "users", "email", "text",
						sql.NullInt64{}, sql.NullInt64{}, sql.NullInt64{},
						"NO", sql.NullString{}},
					{"public", "posts", "id", "int8",
						sql.NullInt64{}, sql.NullInt64{Int64: 64, Valid: true}, sql.NullInt64{Int64: 0, Valid: true},
						"NO", sql.NullString{String: "nextval('posts_id_seq'::regclass)", Valid: true}},
					{"public", "posts", "userId", "int8",
						sql.NullInt64{}, sql.NullInt64{Int64: 64, Valid: true}, sql.NullInt64{Int64: 0, Valid: true},
						"NO", sql.NullString{}},
				},
			}, nil
		case strings.Contains(q, "PRIMARY KEY"):
			return &fakeRows{
				cols: []string{"table_schema", "table_name", "column_name"},
				data: [][]any{
					{"public", "users", "id"},
					{"public", "posts", "id"},
				},
			}, nil
		case strings.Contains(q, "constraint_type = 'UNIQUE'"):
			return &fakeRows{
				cols: []string{"table_schema", "table_name", "constraint_name", "column_name"},
				data: [][]any{
					{"public", "users", "usersEmailUnique", "email"},
				},
			}, nil
		case strings.Contains(q, "constraint_type = 'FOREIGN KEY'"):
			return &fakeRows{
				cols: []string{
					"table_schema", "table_name", "constraint_name", "column_name",
					"target_schema", "target_table", "target_column",
					"delete_rule", "update_rule",
				},
				data: [][]any{
					{"public", "posts", "postsUserIdUsersIdFk", "userId",
						"public", "users", "id", "CASCADE", "NO ACTION"},
				},
			}, nil
		}
		return &fakeRows{}, nil
	}}
}

func TestIntrospectBuildsSnapshot(t *testing.T) {
	fd := introspectFake()
	db := pg.New(fd)
	snap, err := pg.Introspect(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Tables) != 2 {
		t.Fatalf("got %d tables, want 2", len(snap.Tables))
	}
	usersT := snap.Tables["public.users"]
	if usersT == nil {
		t.Fatal("missing public.users")
	}
	idCol := usersT.Columns["id"]
	if idCol == nil || idCol.Type != "bigserial" {
		t.Errorf("users.id: %+v (want bigserial)", idCol)
	}
	if !idCol.PrimaryKey || !idCol.NotNull {
		t.Errorf("users.id flags: pk=%v notNull=%v", idCol.PrimaryKey, idCol.NotNull)
	}
	if idCol.Default != nil {
		t.Errorf("users.id default should be hidden for serial, got %q", *idCol.Default)
	}

	if _, ok := usersT.UniqueConstraints["usersEmailUnique"]; !ok {
		t.Errorf("missing unique constraint")
	}

	postsT := snap.Tables["public.posts"]
	fk := postsT.ForeignKeys["postsUserIdUsersIdFk"]
	if fk == nil {
		t.Fatal("missing FK")
	}
	if fk.TableTo != "users" || fk.ColumnsTo[0] != "id" {
		t.Errorf("FK target: %+v", fk)
	}
	if fk.OnDelete != "cascade" {
		t.Errorf("onDelete = %q, want cascade", fk.OnDelete)
	}
	if fk.OnUpdate != "no action" {
		t.Errorf("onUpdate = %q, want 'no action'", fk.OnUpdate)
	}
}

func TestIntrospectThenDiffAgainstGoSchemaProducesAddColumn(t *testing.T) {
	// Introspect the live DB (two tables).
	fd := introspectFake()
	db := pg.New(fd)
	current, err := pg.Introspect(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}

	// Build a Go schema where users has an extra column.
	users := pg.NewTable("users")
	uid := pg.Add(users, pg.BigSerial("id").PrimaryKey())
	pg.Add(users, pg.Text("name").NotNull())
	pg.Add(users, pg.Text("email").NotNull().Unique())
	pg.Add(users, pg.Integer("age")) // new
	posts := pg.NewTable("posts")
	pg.Add(posts, pg.BigSerial("id").PrimaryKey())
	pg.Add(posts, pg.BigInt("userId").NotNull().References(uid, pg.OnDelete("CASCADE")))

	desired := pg.BuildSnapshot(pg.NewSchema(users, posts))
	stmts := pg.Diff(current, desired)

	joined := strings.Join(stmts, "\n")
	if !strings.Contains(joined, `ADD COLUMN "age"`) {
		t.Errorf("expected ADD COLUMN age, got:\n%s", joined)
	}
}

// --- Push -----------------------------------------------------------

func TestPushDryRunReturnsStatementsWithoutExec(t *testing.T) {
	fd := introspectFake()
	db := pg.New(fd)

	users := pg.NewTable("users")
	uid := pg.Add(users, pg.BigSerial("id").PrimaryKey())
	pg.Add(users, pg.Text("name").NotNull())
	pg.Add(users, pg.Text("email").NotNull().Unique())
	pg.Add(users, pg.Integer("age"))
	posts := pg.NewTable("posts")
	pg.Add(posts, pg.BigSerial("id").PrimaryKey())
	pg.Add(posts, pg.BigInt("userId").NotNull().References(uid, pg.OnDelete("CASCADE")))

	res, err := pg.Push(context.Background(), db, pg.NewSchema(users, posts),
		pg.PushOptions{DryRun: true, Safe: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.Applied {
		t.Error("DryRun should not have Applied=true")
	}
	if len(res.Statements) == 0 {
		t.Fatal("expected at least one statement")
	}
	if !strings.Contains(res.Statements[0], "ADD COLUMN IF NOT EXISTS") {
		t.Errorf("expected Safe-mode statement, got %q", res.Statements[0])
	}

	// Verify no Exec was invoked beyond introspection queries.
	for _, q := range fd.queries {
		if strings.HasPrefix(strings.TrimSpace(q), "ALTER TABLE") {
			t.Errorf("DryRun executed an ALTER: %q", q)
		}
	}
}

func TestPushAppliesStatementsInTransaction(t *testing.T) {
	executed := []string{}
	fd := introspectFake()
	fd.exec = func(sqlStr string, _ []any) (drops.Result, error) {
		executed = append(executed, sqlStr)
		return fakeResult{1}, nil
	}
	db := pg.New(fd)

	users := pg.NewTable("users")
	pg.Add(users, pg.BigSerial("id").PrimaryKey())
	pg.Add(users, pg.Text("name").NotNull())
	pg.Add(users, pg.Text("email").NotNull().Unique())
	pg.Add(users, pg.Integer("age"))
	posts := pg.NewTable("posts")
	pg.Add(posts, pg.BigSerial("id").PrimaryKey())
	pg.Add(posts, pg.BigInt("userId").NotNull())

	res, err := pg.Push(context.Background(), db, pg.NewSchema(users, posts))
	if err != nil {
		t.Fatal(err)
	}
	if !res.Applied {
		t.Error("expected Applied=true")
	}
	if len(executed) == 0 {
		t.Fatal("expected at least one Exec")
	}
	found := false
	for _, s := range executed {
		if strings.Contains(s, `ADD COLUMN "age"`) {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected an ADD COLUMN age in Exec calls, got: %v", executed)
	}
}

func TestPushNoOpWhenSchemaMatches(t *testing.T) {
	fd := introspectFake()
	db := pg.New(fd)

	// Build a Go schema identical to what introspectFake represents.
	users := pg.NewTable("users")
	uid := pg.Add(users, pg.BigSerial("id").PrimaryKey())
	pg.Add(users, pg.Text("name").NotNull())
	pg.Add(users, pg.Text("email").NotNull().Unique())
	posts := pg.NewTable("posts")
	pg.Add(posts, pg.BigSerial("id").PrimaryKey())
	pg.Add(posts, pg.BigInt("userId").NotNull().References(uid, pg.OnDelete("CASCADE")))

	res, err := pg.Push(context.Background(), db, pg.NewSchema(users, posts))
	if err != nil {
		t.Fatal(err)
	}
	if res.Applied || len(res.Statements) != 0 {
		t.Errorf("expected no-op, got %+v", res)
	}
}
