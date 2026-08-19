package drops_test

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bernardoforcillo/drops"
)

// The scan layer is what every dialect funnels through, and most of
// what it decides — a NULL landing in a non-pointer field, a string
// column reaching an int field — is decided by database/sql's
// convertAssign. So these tests drive a real *sql.Rows over a canned
// driver rather than a hand-written cursor, which would only restate
// the assumptions under test.

type resultSet struct {
	cols []string
	rows [][]driver.Value
}

var canned sync.Map // map[string]resultSet

type cannedDriver struct{}

func (cannedDriver) Open(string) (driver.Conn, error) { return cannedConn{}, nil }

type cannedConn struct{}

func (cannedConn) Prepare(query string) (driver.Stmt, error) { return cannedStmt{query}, nil }
func (cannedConn) Close() error                              { return nil }
func (cannedConn) Begin() (driver.Tx, error) {
	return nil, errors.New("canned: no transactions")
}

type cannedStmt struct{ query string }

func (cannedStmt) Close() error  { return nil }
func (cannedStmt) NumInput() int { return 0 }
func (cannedStmt) Exec([]driver.Value) (driver.Result, error) {
	return nil, errors.New("canned: no exec")
}

func (s cannedStmt) Query([]driver.Value) (driver.Rows, error) {
	rs, ok := canned.Load(s.query)
	if !ok {
		return nil, fmt.Errorf("canned: no result registered for %q", s.query)
	}
	set := rs.(resultSet)
	return &cannedRows{set: set}, nil
}

type cannedRows struct {
	set resultSet
	pos int
}

func (r *cannedRows) Columns() []string { return r.set.cols }
func (r *cannedRows) Close() error      { return nil }

func (r *cannedRows) Next(dest []driver.Value) error {
	if r.pos >= len(r.set.rows) {
		return io.EOF
	}
	copy(dest, r.set.rows[r.pos])
	r.pos++
	return nil
}

var (
	cannedDB   *sql.DB
	cannedOnce sync.Once
	cannedSeq  atomic.Int64
)

// query registers a result set and returns a drops.RowSource that
// replays it.
func query(t *testing.T, cols []string, rows ...[]driver.Value) drops.RowSource {
	t.Helper()
	cannedOnce.Do(func() {
		sql.Register("drops-canned", cannedDriver{})
		db, err := sql.Open("drops-canned", "")
		if err != nil {
			t.Fatalf("open canned db: %v", err)
		}
		cannedDB = db
	})
	key := fmt.Sprintf("q%d", cannedSeq.Add(1))
	canned.Store(key, resultSet{cols: cols, rows: rows})
	return sqlSource{db: cannedDB, query: key}
}

type sqlSource struct {
	db    *sql.DB
	query string
}

func (s sqlSource) Rows(ctx context.Context) (drops.Rows, error) {
	return s.db.QueryContext(ctx, s.query)
}

// --- All / One --------------------------------------------------------

// projection is the shape an ad-hoc query produces: no table has it.
type projection struct {
	Author string
	Posts  int64
}

func TestAllReturnsTypedProjection(t *testing.T) {
	src := query(t, []string{"author", "posts"},
		[]driver.Value{"ada", int64(3)},
		[]driver.Value{"grace", int64(7)},
	)
	got, err := drops.All[projection](context.Background(), src)
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	want := []projection{{"ada", 3}, {"grace", 7}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

func TestAllScalar(t *testing.T) {
	src := query(t, []string{"id"},
		[]driver.Value{int64(1)},
		[]driver.Value{int64(2)},
	)
	got, err := drops.All[int64](context.Background(), src)
	if err != nil {
		t.Fatalf("All[int64]: %v", err)
	}
	if !reflect.DeepEqual(got, []int64{1, 2}) {
		t.Errorf("got %v, want [1 2]", got)
	}
}

func TestAllScalarRejectsMultipleColumns(t *testing.T) {
	src := query(t, []string{"id", "name"}, []driver.Value{int64(1), "ada"})
	_, err := drops.All[int64](context.Background(), src)
	if err == nil || !strings.Contains(err.Error(), "single-column") {
		t.Fatalf("want a single-column error, got %v", err)
	}
}

func TestAllPointerElements(t *testing.T) {
	src := query(t, []string{"author", "posts"}, []driver.Value{"ada", int64(3)})
	got, err := drops.All[*projection](context.Background(), src)
	if err != nil {
		t.Fatalf("All[*projection]: %v", err)
	}
	if len(got) != 1 || got[0].Author != "ada" {
		t.Fatalf("got %+v", got)
	}
}

func TestAllPointerScalarTakesNull(t *testing.T) {
	src := query(t, []string{"nickname"},
		[]driver.Value{"ada"},
		[]driver.Value{nil},
	)
	got, err := drops.All[*string](context.Background(), src)
	if err != nil {
		t.Fatalf("All[*string]: %v", err)
	}
	if len(got) != 2 || got[0] == nil || *got[0] != "ada" || got[1] != nil {
		t.Fatalf("got %v", got)
	}
}

func TestAllEmptyResultIsEmptySlice(t *testing.T) {
	got, err := drops.All[projection](context.Background(), query(t, []string{"author", "posts"}))
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %+v, want empty", got)
	}
}

func TestOneReturnsErrNoRowsOnEmpty(t *testing.T) {
	got, err := drops.One[projection](context.Background(), query(t, []string{"author", "posts"}))
	if !errors.Is(err, drops.ErrNoRows) {
		t.Fatalf("want ErrNoRows, got %v", err)
	}
	if got != (projection{}) {
		t.Errorf("want the zero value alongside ErrNoRows, got %+v", got)
	}
}

func TestOneScalar(t *testing.T) {
	got, err := drops.One[int64](context.Background(), query(t, []string{"count"}, []driver.Value{int64(42)}))
	if err != nil {
		t.Fatalf("One[int64]: %v", err)
	}
	if got != 42 {
		t.Errorf("got %d, want 42", got)
	}
}

func TestOnePointerScalarTakesNull(t *testing.T) {
	got, err := drops.One[*string](context.Background(), query(t, []string{"nickname"}, []driver.Value{nil}))
	if err != nil {
		t.Fatalf("One[*string]: %v", err)
	}
	if got != nil {
		t.Errorf("got %q, want nil", *got)
	}
}

func TestOnePointerStructIsRejected(t *testing.T) {
	src := query(t, []string{"author"}, []driver.Value{"ada"})
	_, err := drops.One[*projection](context.Background(), src)
	if err == nil || !strings.Contains(err.Error(), "requires a pointer to struct or to a scalar") {
		t.Fatalf("want a pointer-to-struct rejection, got %v", err)
	}
	if err != nil && !strings.Contains(err.Error(), "**drops_test.projection") {
		t.Errorf("error should name the destination type, got %v", err)
	}
}

func TestAllPropagatesQueryError(t *testing.T) {
	_, err := drops.All[projection](context.Background(), sqlSource{db: cannedDBOrOpen(t), query: "unregistered"})
	if err == nil {
		t.Fatal("want the query error, got nil")
	}
}

func cannedDBOrOpen(t *testing.T) *sql.DB {
	t.Helper()
	query(t, nil)
	return cannedDB
}

// --- Mapping rules ----------------------------------------------------

type audit struct {
	CreatedAt time.Time
	Note      string
}

// article embeds an unexported struct, the way a codebase factors
// bookkeeping columns out of every row type.
type article struct {
	ID int64
	audit
}

func TestScanWalksUnexportedEmbeddedStruct(t *testing.T) {
	when := time.Date(2024, 3, 1, 12, 0, 0, 0, time.UTC)
	src := query(t, []string{"id", "createdAt", "note"},
		[]driver.Value{int64(1), when, "draft"})
	got, err := drops.One[article](context.Background(), src)
	if err != nil {
		t.Fatalf("One: %v", err)
	}
	if !got.CreatedAt.Equal(when) || got.Note != "draft" {
		t.Errorf("promoted fields not scanned: %+v", got)
	}
}

type EmbeddedKey struct{ ID int64 }

// shadowedFirst declares its own ID before embedding one, which is the
// order that used to hand the column to the embedded field.
type shadowedFirst struct {
	ID string
	EmbeddedKey
}

type shadowedLast struct {
	EmbeddedKey
	ID string
}

func TestOuterFieldShadowsEmbeddedOne(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  func(drops.RowSource) (string, int64, error)
	}{
		{"outer declared first", func(src drops.RowSource) (string, int64, error) {
			v, err := drops.One[shadowedFirst](context.Background(), src)
			return v.ID, v.EmbeddedKey.ID, err
		}},
		{"outer declared last", func(src drops.RowSource) (string, int64, error) {
			v, err := drops.One[shadowedLast](context.Background(), src)
			return v.ID, v.EmbeddedKey.ID, err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src := query(t, []string{"id"}, []driver.Value{"a1"})
			outer, embedded, err := tc.run(src)
			if err != nil {
				t.Fatalf("One: %v", err)
			}
			if outer != "a1" || embedded != 0 {
				t.Errorf("column went to the embedded field: outer=%q embedded=%d", outer, embedded)
			}
		})
	}
}

type tagged struct {
	Label string `drop:"b"`
	B     string
}

func TestTagBeatsAnotherFieldsCamelCaseForm(t *testing.T) {
	src := query(t, []string{"b"}, []driver.Value{"tagged"})
	got, err := drops.One[tagged](context.Background(), src)
	if err != nil {
		t.Fatalf("One: %v", err)
	}
	if got.Label != "tagged" || got.B != "" {
		t.Errorf("tag lost to a camelCase form: %+v", got)
	}
}

// colliding has two fields that camelCase to the same "sku".
type colliding struct {
	SKU int64
	Sku int64
}

func TestCamelCaseCollisionPrefersFirstField(t *testing.T) {
	src := query(t, []string{"sku"}, []driver.Value{int64(9)})
	got, err := drops.One[colliding](context.Background(), src)
	if err != nil {
		t.Fatalf("One: %v", err)
	}
	if got.SKU != 9 || got.Sku != 0 {
		t.Errorf("want the first-declared field to win, got %+v", got)
	}
}

// reading embeds time.Time, which the scanner must hand a column to
// rather than walk into.
type reading struct {
	ID int64
	time.Time
}

func TestEmbeddedTimeReceivesItsOwnColumn(t *testing.T) {
	when := time.Date(2024, 3, 1, 12, 0, 0, 0, time.UTC)
	src := query(t, []string{"id", "time"}, []driver.Value{int64(1), when})
	got, err := drops.One[reading](context.Background(), src)
	if err != nil {
		t.Fatalf("One: %v", err)
	}
	if !got.Time.Equal(when) {
		t.Errorf("embedded time.Time was walked into instead of scanned: %+v", got)
	}
}

type sparse struct {
	ID   int64
	Name string
}

func TestUnmatchedColumnAndUnmatchedFieldAreBothTolerated(t *testing.T) {
	src := query(t, []string{"id", "unrelated"}, []driver.Value{int64(4), "ignored"})
	got, err := drops.One[sparse](context.Background(), src)
	if err != nil {
		t.Fatalf("One: %v", err)
	}
	if got.ID != 4 || got.Name != "" {
		t.Errorf("got %+v", got)
	}
}

type withUnexported struct {
	ID     int64
	secret string
}

func TestUnexportedFieldIsIgnored(t *testing.T) {
	src := query(t, []string{"id", "secret"}, []driver.Value{int64(1), "shh"})
	got, err := drops.One[withUnexported](context.Background(), src)
	if err != nil {
		t.Fatalf("One: %v", err)
	}
	if got.secret != "" {
		t.Errorf("unexported field was written: %+v", got)
	}
}

type nullable struct {
	ID   int64
	Name string
	Nick *string
}

func TestNullIntoNonPointerFieldIsAnError(t *testing.T) {
	src := query(t, []string{"id", "name"}, []driver.Value{int64(1), nil})
	_, err := drops.One[nullable](context.Background(), src)
	if err == nil {
		t.Fatal("want a conversion error for NULL into string, got nil")
	}
}

func TestNullIntoPointerFieldIsNil(t *testing.T) {
	src := query(t, []string{"id", "nick"}, []driver.Value{int64(1), nil})
	got, err := drops.One[nullable](context.Background(), src)
	if err != nil {
		t.Fatalf("One: %v", err)
	}
	if got.Nick != nil {
		t.Errorf("want nil Nick, got %q", *got.Nick)
	}
}

// --- Cursor lifetime --------------------------------------------------

// panicRows panics from Scan, standing in for any failure inside the
// scan loop that unwinds past the normal return paths.
type panicRows struct {
	closed bool
	done   bool
}

func (r *panicRows) Next() bool {
	if r.done {
		return false
	}
	r.done = true
	return true
}
func (r *panicRows) Scan(...any) error          { panic("driver blew up") }
func (r *panicRows) Columns() ([]string, error) { return []string{"id"}, nil }
func (r *panicRows) Close() error               { r.closed = true; return nil }
func (r *panicRows) Err() error                 { return nil }

func TestScanClosesRowsOnPanic(t *testing.T) {
	rows := &panicRows{}
	func() {
		defer func() { _ = recover() }()
		var out []sparse
		_ = drops.ScanAll(rows, &out)
	}()
	if !rows.closed {
		t.Error("ScanAll leaked the cursor on the panic path")
	}
}

func TestScanClosesRowsOnEarlyError(t *testing.T) {
	rows := &panicRows{}
	if err := drops.ScanAll(rows, sparse{}); err == nil {
		t.Fatal("want an error for a non-pointer destination")
	}
	if !rows.closed {
		t.Error("ScanAll leaked the cursor on the early-return path")
	}
}
