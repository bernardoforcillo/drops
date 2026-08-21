package integration_test

import (
	"context"
	"strings"
	"testing"

	"github.com/bernardoforcillo/drops/integration"
	"github.com/bernardoforcillo/drops/mysql"
)

// What MySQL does to the objects that name a column when the column
// goes, established against the live server rather than borrowed from
// PostgreSQL.
//
// PostgreSQL has one rule — the dependent goes whole, whichever of its
// columns went — and drops/pg orders its statements around it. MySQL
// has five, and they disagree with each other:
//
//   - a secondary index over the dropped column alone is removed with
//     the column, so a DROP INDEX afterwards names nothing (1091);
//   - a secondary index over several columns is NARROWED to the ones
//     that remain, and stays. Nothing is removed, so a fix that
//     suppressed the DROP INDEX on the grounds that the server had
//     already taken it would leave the index standing and the next
//     push would ask for the same drop again;
//   - a UNIQUE key over the dropped column alone is removed with it,
//     but one spanning several columns is not narrowed: MariaDB
//     refuses the column drop outright (1072);
//   - a CHECK naming only the dropped column is removed with it on
//     MariaDB (1091 afterwards) and refused on MySQL 8.0.16+ (3959);
//     one naming another column too is refused on both (1054);
//   - a column on either side of a foreign key cannot be dropped at
//     all while the key stands (1553), and neither can the index the
//     key needs;
//   - a PRIMARY KEY over the dropped column alone goes with it (1091
//     afterwards); a composite one is refused (1072).
//
// Every one of them points the same way — the dependent goes before
// the column — which is what these tests pin. They assert the
// catalogue after the push rather than the statement list, because the
// statement list was never the thing that failed.

// dropIndexes is the push mode in which the ordering question about an
// index arises at all. Push withholds the drop of an index the Go
// schema does not declare unless it is told otherwise, and a withheld
// statement cannot be misordered; these tests are about the statement
// Push does emit.
var dropIndexes = mysql.PushOptions{DropUnmanagedIndexes: true}

// mysqlPushDeps applies desired and fails with the plan that broke.
//
// The plan is computed first, in a dry run, purely so the failure names
// the statements in the order they were about to run — the subject
// here, and not something the server's error carries. MySQL has no
// transactional DDL, so a push that fails half way leaves the first
// half applied; the plan is also the only way to see which half.
func mysqlPushDeps(t *testing.T, db *mysql.DB, desired *mysql.Schema, opts ...mysql.PushOptions) {
	t.Helper()
	ctx := context.Background()
	dry := mysql.PushOptions{DryRun: true}
	if len(opts) > 0 {
		dry = opts[0]
		dry.DryRun = true
	}
	plan, err := mysql.Push(ctx, db, desired, dry)
	if err != nil {
		t.Fatalf("planning the push: %v", err)
	}
	if _, err := mysql.Push(ctx, db, desired, opts...); err != nil {
		t.Fatalf("push: %v\nthe plan was:\n  %s", err, strings.Join(plan.Statements, "\n  "))
	}
}

// mysqlSeedBefore builds the "before" database with a push of its own,
// so every index and constraint wears the name drops gives it rather
// than one the server invented for an inline declaration.
func mysqlSeedBefore(t *testing.T, db *mysql.DB, before *mysql.Schema) {
	t.Helper()
	if _, err := mysql.Push(context.Background(), db, before); err != nil {
		t.Fatalf("seeding the before-schema: %v", err)
	}
}

// mysqlIndexesOf reads a table's indexes as name -> comma-joined key
// columns, in key order. PRIMARY is among them: MySQL keeps the primary
// key in the same catalogue as everything else.
func mysqlIndexesOf(t *testing.T, db *mysql.DB, table string) map[string]string {
	t.Helper()
	return mysqlNamedRows(t, db,
		`SELECT INDEX_NAME, GROUP_CONCAT(COLUMN_NAME ORDER BY SEQ_IN_INDEX)
		   FROM information_schema.STATISTICS
		  WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ?
		  GROUP BY INDEX_NAME`, table)
}

// mysqlColumnsOf reads a table's columns as name -> data type.
func mysqlColumnsOf(t *testing.T, db *mysql.DB, table string) map[string]string {
	t.Helper()
	return mysqlNamedRows(t, db,
		`SELECT COLUMN_NAME, DATA_TYPE FROM information_schema.COLUMNS
		  WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ?`, table)
}

// mysqlChecksOf reads a table's CHECK constraints as name -> clause.
func mysqlChecksOf(t *testing.T, db *mysql.DB, table string) map[string]string {
	t.Helper()
	return mysqlNamedRows(t, db,
		`SELECT c.CONSTRAINT_NAME, c.CHECK_CLAUSE
		   FROM information_schema.CHECK_CONSTRAINTS c
		   JOIN information_schema.TABLE_CONSTRAINTS tc
		     ON tc.CONSTRAINT_SCHEMA = c.CONSTRAINT_SCHEMA
		    AND tc.CONSTRAINT_NAME = c.CONSTRAINT_NAME
		    AND tc.CONSTRAINT_TYPE = 'CHECK'
		  WHERE c.CONSTRAINT_SCHEMA = DATABASE() AND tc.TABLE_NAME = ?`, table)
}

// mysqlForeignKeysOf reads a table's foreign keys as name -> referenced
// table.
func mysqlForeignKeysOf(t *testing.T, db *mysql.DB, table string) map[string]string {
	t.Helper()
	return mysqlNamedRows(t, db,
		`SELECT CONSTRAINT_NAME, REFERENCED_TABLE_NAME
		   FROM information_schema.KEY_COLUMN_USAGE
		  WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ?
		    AND REFERENCED_TABLE_NAME IS NOT NULL`, table)
}

func mysqlNamedRows(t *testing.T, db *mysql.DB, query string, args ...any) map[string]string {
	t.Helper()
	rows, err := db.Query(context.Background(), query, args...)
	if err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var name, value string
		if err := rows.Scan(&name, &value); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
		out[name] = value
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return out
}

// mysqlSettled asserts the schema has converged: pushing it a second
// time asks for nothing. A drop the server had already made for us
// would pass the first push and reappear here; so would an index the
// server kept because it was only narrowed.
func mysqlSettled(t *testing.T, db *mysql.DB, desired *mysql.Schema, opts ...mysql.PushOptions) {
	t.Helper()
	dry := mysql.PushOptions{DryRun: true}
	if len(opts) > 0 {
		dry = opts[0]
		dry.DryRun = true
	}
	res, err := mysql.Push(context.Background(), db, desired, dry)
	if err != nil {
		t.Fatalf("re-planning the push: %v", err)
	}
	if len(res.Statements) != 0 {
		t.Fatalf("the schema has not settled; a second push still wants:\n  %s",
			strings.Join(res.Statements, "\n  "))
	}
}

// The reported shape: a UNIQUE key on the column that is going. The
// server takes the key with the column, so the DROP INDEX that came
// after it named nothing (1091).
func TestMySQLPushDropsAColumnCarryingAUniqueKey(t *testing.T) {
	db := openMySQL(t)
	name := integration.UniqueName(t, "dd")

	before := mysql.NewTable(name)
	mysql.Add(before, mysql.BigSerial("id").PrimaryKey())
	mysql.Add(before, mysql.Varchar("email", 190).NotNull().Unique())
	dropMySQL(t, db, before)
	mysqlSeedBefore(t, db, mysql.NewSchema(before))

	after := mysql.NewTable(name)
	mysql.Add(after, mysql.BigSerial("id").PrimaryKey())
	mysqlPushDeps(t, db, mysql.NewSchema(after), dropIndexes)

	if cols := mysqlColumnsOf(t, db, name); cols["email"] != "" {
		t.Fatalf("%s after the push = %v", name, cols)
	}
	if idx := mysqlIndexesOf(t, db, name); idx["uq_"+name+"_email"] != "" {
		t.Fatalf("the unique key outlived its column: %v", idx)
	}
	mysqlSettled(t, db, mysql.NewSchema(after), dropIndexes)
}

// A secondary index over the column that is going, which the server
// also takes with the column.
func TestMySQLPushDropsAColumnCarryingAnIndex(t *testing.T) {
	db := openMySQL(t)
	name := integration.UniqueName(t, "dd")

	before := mysql.NewTable(name)
	mysql.Add(before, mysql.BigSerial("id").PrimaryKey())
	age := mysql.Add(before, mysql.Integer("age"))
	before.AddIndex(mysql.NewIndex("idx_age", before, age))
	dropMySQL(t, db, before)
	mysqlSeedBefore(t, db, mysql.NewSchema(before))

	after := mysql.NewTable(name)
	mysql.Add(after, mysql.BigSerial("id").PrimaryKey())
	mysqlPushDeps(t, db, mysql.NewSchema(after), dropIndexes)

	if idx := mysqlIndexesOf(t, db, name); idx["idx_age"] != "" {
		t.Fatalf("the index outlived its column: %v", idx)
	}
	mysqlSettled(t, db, mysql.NewSchema(after), dropIndexes)
}

// One column of a two-column index. This is where MySQL parts company
// with PostgreSQL: the server narrows the index to the column that
// remains rather than dropping it, so the drop has to be emitted —
// suppressing it on the grounds that the column drop took the index
// would leave a narrowed index behind and a push that never settles.
func TestMySQLPushDropsOneColumnOfATwoColumnIndex(t *testing.T) {
	db := openMySQL(t)
	name := integration.UniqueName(t, "dd")

	before := mysql.NewTable(name)
	mysql.Add(before, mysql.BigSerial("id").PrimaryKey())
	a := mysql.Add(before, mysql.Integer("a"))
	b := mysql.Add(before, mysql.Integer("b"))
	before.AddIndex(mysql.NewIndex("idx_ab", before, a, b))
	dropMySQL(t, db, before)
	mysqlSeedBefore(t, db, mysql.NewSchema(before))

	after := mysql.NewTable(name)
	mysql.Add(after, mysql.BigSerial("id").PrimaryKey())
	mysql.Add(after, mysql.Integer("b"))
	mysqlPushDeps(t, db, mysql.NewSchema(after), dropIndexes)

	if idx := mysqlIndexesOf(t, db, name); idx["idx_ab"] != "" {
		t.Fatalf("the index was narrowed rather than dropped, and the push let it stand: %v", idx)
	}
	mysqlSettled(t, db, mysql.NewSchema(after), dropIndexes)
}

// One column of a two-column UNIQUE key. MariaDB will not narrow a
// unique key the way it narrows a plain index — it refuses the column
// drop with 1072 — so the key has to go first.
func TestMySQLPushDropsOneColumnOfATwoColumnUniqueKey(t *testing.T) {
	db := openMySQL(t)
	name := integration.UniqueName(t, "dd")

	before := mysql.NewTable(name)
	mysql.Add(before, mysql.BigSerial("id").PrimaryKey())
	a := mysql.Add(before, mysql.Integer("a"))
	b := mysql.Add(before, mysql.Integer("b"))
	before.AddIndex(mysql.NewIndex("uq_ab", before, a, b).Unique())
	dropMySQL(t, db, before)
	mysqlSeedBefore(t, db, mysql.NewSchema(before))

	after := mysql.NewTable(name)
	mysql.Add(after, mysql.BigSerial("id").PrimaryKey())
	mysql.Add(after, mysql.Integer("b"))
	mysqlPushDeps(t, db, mysql.NewSchema(after), dropIndexes)

	if idx := mysqlIndexesOf(t, db, name); idx["uq_ab"] != "" {
		t.Fatalf("the unique key outlived the column it spanned: %v", idx)
	}
	mysqlSettled(t, db, mysql.NewSchema(after), dropIndexes)
}

// A CHECK over the column that is going. The two servers disagree on
// what happens if the column goes first — MariaDB drops the constraint
// silently, MySQL refuses with 3959 — and agree that the push must not
// find out: the constraint goes first either way.
func TestMySQLPushDropsAColumnCarryingACheck(t *testing.T) {
	db := openMySQL(t)
	name := integration.UniqueName(t, "dd")

	before := mysql.NewTable(name)
	mysql.Add(before, mysql.BigSerial("id").PrimaryKey())
	mysql.Add(before, mysql.Integer("age"))
	mysql.Add(before, mysql.Integer("score"))
	before.AddCheck("ck_age", "`age` >= 0")
	before.AddCheck("ck_age_score", "`age` < `score`")
	dropMySQL(t, db, before)
	mysqlSeedBefore(t, db, mysql.NewSchema(before))

	after := mysql.NewTable(name)
	mysql.Add(after, mysql.BigSerial("id").PrimaryKey())
	mysql.Add(after, mysql.Integer("score"))
	mysqlPushDeps(t, db, mysql.NewSchema(after))

	if ck := mysqlChecksOf(t, db, name); ck["ck_age"] != "" || ck["ck_age_score"] != "" {
		t.Fatalf("a CHECK outlived the column it names: %v", ck)
	}
	mysqlSettled(t, db, mysql.NewSchema(after))
}

// The column the foreign key is on. InnoDB refuses to drop it while
// the key stands (1553).
func TestMySQLPushDropsAColumnCarryingAForeignKey(t *testing.T) {
	db := openMySQL(t)
	base := integration.UniqueName(t, "dd")
	parentName, childName := base+"_p", base+"_c"

	parent := mysql.NewTable(parentName)
	parentID := mysql.Add(parent, mysql.BigSerial("id").PrimaryKey())
	child := mysql.NewTable(childName)
	mysql.Add(child, mysql.BigSerial("id").PrimaryKey())
	mysql.Add(child, mysql.BigInt("parentId").References(parentID))
	dropMySQL(t, db, parent)
	dropMySQL(t, db, child)
	mysqlSeedBefore(t, db, mysql.NewSchema(parent, child))

	parentAfter := mysql.NewTable(parentName)
	mysql.Add(parentAfter, mysql.BigSerial("id").PrimaryKey())
	childAfter := mysql.NewTable(childName)
	mysql.Add(childAfter, mysql.BigSerial("id").PrimaryKey())
	mysqlPushDeps(t, db, mysql.NewSchema(parentAfter, childAfter))

	if cols := mysqlColumnsOf(t, db, childName); cols["parentId"] != "" {
		t.Fatalf("%s after the push = %v", childName, cols)
	}
	if fk := mysqlForeignKeysOf(t, db, childName); len(fk) != 0 {
		t.Fatalf("the foreign key outlived its column: %v", fk)
	}
	mysqlSettled(t, db, mysql.NewSchema(parentAfter, childAfter))
}

// The other side of the same key: the column another table's foreign
// key points at. The review found this one emitted worst of all — the
// DROP COLUMN on the referenced table came out ahead of the DROP
// FOREIGN KEY on the table pointing at it, so 1553 fired before any of
// the stale-name errors could.
func TestMySQLPushDropsAColumnAnotherTablesForeignKeyPointsAt(t *testing.T) {
	db := openMySQL(t)
	base := integration.UniqueName(t, "dd")
	parentName, childName := base+"_p", base+"_c"

	parent := mysql.NewTable(parentName)
	mysql.Add(parent, mysql.BigSerial("id").PrimaryKey())
	code := mysql.Add(parent, mysql.Varchar("code", 64).NotNull().Unique())
	child := mysql.NewTable(childName)
	mysql.Add(child, mysql.BigSerial("id").PrimaryKey())
	mysql.Add(child, mysql.Varchar("parentCode", 64).References(code))
	dropMySQL(t, db, parent)
	dropMySQL(t, db, child)
	mysqlSeedBefore(t, db, mysql.NewSchema(parent, child))

	parentAfter := mysql.NewTable(parentName)
	mysql.Add(parentAfter, mysql.BigSerial("id").PrimaryKey())
	childAfter := mysql.NewTable(childName)
	mysql.Add(childAfter, mysql.BigSerial("id").PrimaryKey())
	mysqlPushDeps(t, db, mysql.NewSchema(parentAfter, childAfter))

	if cols := mysqlColumnsOf(t, db, parentName); cols["code"] != "" {
		t.Fatalf("%s after the push = %v", parentName, cols)
	}
	if fk := mysqlForeignKeysOf(t, db, childName); len(fk) != 0 {
		t.Fatalf("the foreign key outlived the column it pointed at: %v", fk)
	}
	mysqlSettled(t, db, mysql.NewSchema(parentAfter, childAfter))
}

// One column of a composite PRIMARY KEY. MariaDB refuses to narrow it
// (1072), so the key goes first and the narrower one is added back
// after the column has gone.
func TestMySQLPushDropsOneColumnOfACompositePrimaryKey(t *testing.T) {
	db := openMySQL(t)
	name := integration.UniqueName(t, "dd")

	before := mysql.NewTable(name)
	mysql.Add(before, mysql.BigInt("orgId").PrimaryKey())
	mysql.Add(before, mysql.BigInt("userId").PrimaryKey())
	mysql.Add(before, mysql.Varchar("role", 32).NotNull())
	dropMySQL(t, db, before)
	mysqlSeedBefore(t, db, mysql.NewSchema(before))

	after := mysql.NewTable(name)
	mysql.Add(after, mysql.BigInt("userId").PrimaryKey())
	mysql.Add(after, mysql.Varchar("role", 32).NotNull())
	mysqlPushDeps(t, db, mysql.NewSchema(after))

	if idx := mysqlIndexesOf(t, db, name); idx["PRIMARY"] != "userId" {
		t.Fatalf("the primary key after the push = %v", idx)
	}
	mysqlSettled(t, db, mysql.NewSchema(after))
}

// Every shape in one migration, across four tables.
//
// Each test above has a single table with a single dependent on it, so
// a fix that grouped the drops per table rather than per kind would
// pass all of them. This is the one that separates them: a foreign key
// pointing from a table that survives at a column that does not, and a
// table dropped out from under another key. The order has to hold
// across the whole statement list at once.
func TestMySQLPushDropsEveryDependentShapeInOneMigration(t *testing.T) {
	db := openMySQL(t)
	base := integration.UniqueName(t, "dd")
	orgsName, usersName := base+"_o", base+"_u"
	membersName, legacyName, notesName := base+"_m", base+"_l", base+"_n"

	orgs := mysql.NewTable(orgsName)
	mysql.Add(orgs, mysql.BigSerial("id").PrimaryKey())
	orgCode := mysql.Add(orgs, mysql.Varchar("code", 64).NotNull().Unique())
	mysql.Add(orgs, mysql.Varchar("name", 64).NotNull())

	users := mysql.NewTable(usersName)
	mysql.Add(users, mysql.BigSerial("id").PrimaryKey())
	mysql.Add(users, mysql.Varchar("email", 190).NotNull().Unique())
	age := mysql.Add(users, mysql.Integer("age"))
	a := mysql.Add(users, mysql.Integer("a"))
	b := mysql.Add(users, mysql.Integer("b"))
	mysql.Add(users, mysql.Varchar("orgCode", 64).References(orgCode))
	users.AddIndex(mysql.NewIndex("idx_age", users, age))
	users.AddIndex(mysql.NewIndex("idx_ab", users, a, b))
	users.AddCheck("ck_age", "`age` >= 0")

	members := mysql.NewTable(membersName)
	mysql.Add(members, mysql.BigInt("orgId").PrimaryKey())
	mysql.Add(members, mysql.BigInt("userId").PrimaryKey())
	mysql.Add(members, mysql.Varchar("role", 32).NotNull())

	legacy := mysql.NewTable(legacyName)
	legacyID := mysql.Add(legacy, mysql.BigSerial("id").PrimaryKey())
	notes := mysql.NewTable(notesName)
	mysql.Add(notes, mysql.BigSerial("id").PrimaryKey())
	mysql.Add(notes, mysql.BigInt("legacyId").References(legacyID))

	for _, tbl := range []*mysql.Table{orgs, legacy} {
		dropMySQL(t, db, tbl)
	}
	for _, tbl := range []*mysql.Table{users, members, notes} {
		dropMySQL(t, db, tbl)
	}
	mysqlSeedBefore(t, db, mysql.NewSchema(orgs, users, members, legacy, notes))

	// orgs loses the column a surviving table's key points at; users
	// loses a column carrying a single-column UNIQUE, one carrying an
	// index and a CHECK, one column of a two-column index and the
	// column its own key is on; members loses one column of its
	// composite PRIMARY KEY; notes loses the column its key on legacy
	// was on, and the key with it.
	//
	// legacy itself stays: Push narrows the live database to the
	// tables the Go schema declares, so a table the schema stops
	// naming is not its business. That is the same reason the schema
	// below still has to name notes.
	orgsAfter := mysql.NewTable(orgsName)
	mysql.Add(orgsAfter, mysql.BigSerial("id").PrimaryKey())
	mysql.Add(orgsAfter, mysql.Varchar("name", 64).NotNull())

	usersAfter := mysql.NewTable(usersName)
	mysql.Add(usersAfter, mysql.BigSerial("id").PrimaryKey())
	mysql.Add(usersAfter, mysql.Integer("b"))

	membersAfter := mysql.NewTable(membersName)
	mysql.Add(membersAfter, mysql.BigInt("userId").PrimaryKey())
	mysql.Add(membersAfter, mysql.Varchar("role", 32).NotNull())

	notesAfter := mysql.NewTable(notesName)
	mysql.Add(notesAfter, mysql.BigSerial("id").PrimaryKey())

	after := mysql.NewSchema(orgsAfter, usersAfter, membersAfter, notesAfter)
	mysqlPushDeps(t, db, after, dropIndexes)

	if cols := mysqlColumnsOf(t, db, usersName); cols["email"] != "" || cols["age"] != "" ||
		cols["a"] != "" || cols["orgCode"] != "" || cols["b"] == "" {
		t.Fatalf("%s after the push = %v", usersName, cols)
	}
	if cols := mysqlColumnsOf(t, db, orgsName); cols["code"] != "" {
		t.Fatalf("%s after the push = %v", orgsName, cols)
	}
	if idx := mysqlIndexesOf(t, db, usersName); len(idx) != 1 || idx["PRIMARY"] != "id" {
		t.Fatalf("%s kept an index it stopped declaring: %v", usersName, idx)
	}
	if ck := mysqlChecksOf(t, db, usersName); len(ck) != 0 {
		t.Fatalf("a CHECK outlived its column: %v", ck)
	}
	if fk := mysqlForeignKeysOf(t, db, usersName); len(fk) != 0 {
		t.Fatalf("the foreign key outlived its column: %v", fk)
	}
	if idx := mysqlIndexesOf(t, db, membersName); idx["PRIMARY"] != "userId" {
		t.Fatalf("%s primary key after the push = %v", membersName, idx)
	}
	if fk := mysqlForeignKeysOf(t, db, notesName); len(fk) != 0 {
		t.Fatalf("the foreign key the schema stopped declaring is still there: %v", fk)
	}
	mysqlSettled(t, db, after, dropIndexes)
}

// The server rule the emitted drop rests on, read off the server
// rather than off drops.
//
// A fix ported from drops/pg would carry PostgreSQL's rule with it —
// the dependent goes whole when any of its columns goes — and could
// then reasonably suppress the DROP INDEX as naming something the
// server had already taken. Here it has not taken it: the two-column
// index is still there, narrowed to the column that remains. The
// single-column one really is gone, and the same DROP INDEX against it
// is 1091. Both facts are what Diff orders around.
//
// MariaDB only. MySQL 8.4 answers the UNIQUE case differently — it
// narrows a multi-column unique key where MariaDB refuses — and there
// is no MySQL here to establish the rest against, so the test skips
// rather than pinning one server's answers as if they were both's.
func TestMariaDBNarrowsSomeIndexesAndTakesOthers(t *testing.T) {
	db := openMySQL(t)
	ctx := context.Background()
	if _, _, mariadb := mysqlServerVersion(t, db); !mariadb {
		t.Skip("the answers below were established against MariaDB; MySQL differs and is not here to be asked")
	}
	name := integration.UniqueName(t, "dd")

	tbl := mysql.NewTable(name)
	mysql.Add(tbl, mysql.BigSerial("id").PrimaryKey())
	dropMySQL(t, db, tbl)
	if _, err := db.Exec(ctx, "CREATE TABLE `"+name+"` ("+
		"`id` BIGINT NOT NULL, `a` INT, `b` INT, `c` INT,"+
		" PRIMARY KEY (`id`), INDEX `idx_a` (`a`), INDEX `idx_bc` (`b`, `c`))"); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := db.Exec(ctx, "ALTER TABLE `"+name+"` DROP COLUMN `a`, DROP COLUMN `b`"); err != nil {
		t.Fatalf("drop the columns: %v", err)
	}
	idx := mysqlIndexesOf(t, db, name)
	if idx["idx_a"] != "" {
		t.Errorf("the single-column index survived its column: %v", idx)
	}
	if idx["idx_bc"] != "c" {
		t.Errorf("the two-column index was expected to narrow to `c`, got %v", idx)
	}
	// And the drop that a suppression rule would have skipped is the
	// one that gets rid of it.
	if _, err := db.Exec(ctx, "DROP INDEX `idx_bc` ON `"+name+"`"); err != nil {
		t.Fatalf("dropping the narrowed index: %v", err)
	}
	if _, err := db.Exec(ctx, "DROP INDEX `idx_a` ON `"+name+"`"); err == nil {
		t.Error("dropping an index the column drop had already taken was accepted; 1091 was expected")
	}
}

// An index the Go schema never declared, over a column the push drops.
//
// Push withholds the drop of an unmanaged index by default, because it
// cannot tell one the schema stopped declaring from one somebody built
// by hand. That reasoning runs out when the index spans a column that
// is going: MySQL will not leave such an index as it is, so withholding
// the drop does not preserve it. It leaves a narrowed index nobody
// asked for under a notice saying the index was left alone, or — for a
// multi-column UNIQUE — it stops the push on 1072 with no explanation.
//
// Both were reachable under the default options, which is where most
// pushes run, and neither is fixed by ordering: the statement that
// wanted ordering had been withheld before the order was decided.
func TestMySQLPushDropsAnUnmanagedIndexOverADepartingColumn(t *testing.T) {
	ctx := context.Background()
	db := openMySQL(t)

	for _, tc := range []struct{ label, keyword string }{
		{"plain", "INDEX"},
		{"unique", "UNIQUE INDEX"},
	} {
		t.Run(tc.label, func(t *testing.T) {
			name := integration.UniqueName(t, "du")
			before := mysql.NewTable(name)
			mysql.Add(before, mysql.BigSerial("id").PrimaryKey())
			mysql.Add(before, mysql.Integer("a"))
			mysql.Add(before, mysql.Integer("b"))
			dropMySQL(t, db, before)
			mysqlSeedBefore(t, db, mysql.NewSchema(before))

			// Built by hand, so the Go schema never names it.
			if _, err := db.Exec(ctx, "CREATE "+tc.keyword+" `by_hand` ON "+
				"`"+name+"` (`a`, `b`)"); err != nil {
				t.Fatalf("creating the hand-made index: %v", err)
			}

			after := mysql.NewTable(name)
			mysql.Add(after, mysql.BigSerial("id").PrimaryKey())
			mysql.Add(after, mysql.Integer("b"))
			desired := mysql.NewSchema(after)

			// Default options: the withholding is what is under test.
			res, err := mysql.Push(ctx, db, desired)
			if err != nil {
				t.Fatalf("push: %v", err)
			}
			if idx := mysqlIndexesOf(t, db, name); idx["by_hand"] != "" {
				t.Errorf("the index over the departing column is still there as %q; "+
					"withholding its drop left it narrowed rather than leaving it alone", idx["by_hand"])
			}
			// And the caller is told, rather than the index vanishing
			// under a notice claiming it was untouched.
			var told bool
			for _, n := range res.Notices {
				if n.Rule == "unmanaged-index" && n.Object == "by_hand" {
					told = true
					if strings.Contains(n.Message, "left it alone") {
						t.Errorf("the notice says the index was left alone, and it was not:\n%s", n.Message)
					}
					if !strings.Contains(n.Message, "`a`") {
						t.Errorf("the notice does not name the departing column:\n%s", n.Message)
					}
				}
			}
			if !told {
				t.Errorf("no unmanaged-index notice for the index Push dropped: %v", res.Notices)
			}
			mysqlSettled(t, db, desired)
		})
	}
}

// The other half, and the one that keeps the option meaning something:
// an unmanaged index over columns this push does not touch is still
// left alone.
func TestMySQLPushLeavesAnUnmanagedIndexItDoesNotDisturb(t *testing.T) {
	ctx := context.Background()
	db := openMySQL(t)
	name := integration.UniqueName(t, "du")

	before := mysql.NewTable(name)
	mysql.Add(before, mysql.BigSerial("id").PrimaryKey())
	mysql.Add(before, mysql.Integer("a"))
	mysql.Add(before, mysql.Integer("b"))
	dropMySQL(t, db, before)
	mysqlSeedBefore(t, db, mysql.NewSchema(before))
	if _, err := db.Exec(ctx, "CREATE INDEX `by_hand` ON `"+name+"` (`b`)"); err != nil {
		t.Fatalf("creating the hand-made index: %v", err)
	}

	// `a` goes; the hand-made index does not span it.
	after := mysql.NewTable(name)
	mysql.Add(after, mysql.BigSerial("id").PrimaryKey())
	mysql.Add(after, mysql.Integer("b"))
	res, err := mysql.Push(ctx, db, mysql.NewSchema(after))
	if err != nil {
		t.Fatalf("push: %v", err)
	}
	if idx := mysqlIndexesOf(t, db, name); idx["by_hand"] != "b" {
		t.Fatalf("the hand-made index the push did not disturb = %q", idx["by_hand"])
	}
	for _, n := range res.Notices {
		if n.Rule == "unmanaged-index" && n.Object == "by_hand" &&
			!strings.Contains(n.Message, "left it alone") {
			t.Errorf("the notice no longer says the index was left alone, and it was:\n%s", n.Message)
		}
	}
}
