package pg

import (
	"errors"
	"fmt"
	"reflect"
	"sort"
	"time"
)

// Go has no lazy loading, and that is a feature — a field access can
// never fire a query behind your back. But the loop only closes if
// forgetting to load a relation is visible, and today it is not:
// forget With("posts") and user.Posts is nil, which reads exactly like
// "this user has no posts". The wrong answer is silent, and it is
// silent in production rather than in a test.
//
// Other ecosystems close the loop by making the *read* fail —
// SQLAlchemy's raiseload, Rails' strict_loading. Go cannot: a struct
// field is a struct field and nothing can interpose on reading one.
// What drops can do is make the *query* fail, before it runs, when the
// struct it is about to fill declares a relation the query never asked
// for:
//
//	db := pg.New(drv)
//	if devMode {
//	    db = db.StrictLoading()
//	}
//
//	var users []User            // User has Posts []Post `dropRel:"posts"`
//	db.Find(Users).All(ctx, &users)
//	// error: relation "posts" on struct main.User is declared but not
//	// loaded — add .Load(Users.Rel("posts")) or .NoLoad(...)
//
// The check is structural and runs before the SELECT, so it costs one
// walk of a struct type and never a round trip. It is off by default:
// turn it on in development and in tests, where a refused query is a
// failing test rather than a failing request.
//
// Two things it deliberately is not. It is not a type change — no
// Rel[T] wrapper, so no churn through the scan layer, dropsgen or the
// drift check, and no rewrite of anybody's structs. And it is not
// [N1Hook]'s job: the N+1 detector counts SQL that already ran, to
// catch a loop issuing the same query over and over, and answers a
// question about queries the caller *did* write. This answers the
// opposite one — about the query the caller did not write — and it
// answers it without executing anything. Run both; they never look at
// the same evidence.
//
// The check runs where a relation could have been loaded: the
// FindBuilder's All/One and the EntityQuery's All/One. [Entity.Get]
// takes a primary key and has no relation-loading vocabulary at all,
// so it is exempt — a struct whose relations matter is fetched with
// Query(db), not Get.

// ErrRelationNotLoaded is the sentinel behind every strict-loading
// refusal, so a caller can errors.Is a failed query into "somebody
// forgot to load a relation" without parsing the message.
var ErrRelationNotLoaded = errors.New("drops/pg: relation not loaded")

// StrictLoading returns a shallow copy of db whose Find and
// Entity.Query builders refuse a query that would leave a declared
// relation field unloaded. See the package notes above; the intended
// home for it is a development / test build, not production, where the
// same mistake should surface as a caught test rather than a refused
// request.
func (db *DB) StrictLoading() *DB {
	cp := *db
	cp.strictLoading = true
	return &cp
}

// IsStrictLoading reports whether db refuses under-specified relation
// queries.
func (db *DB) IsStrictLoading() bool { return db.strictLoading }

// Strict turns the strict-loading check on for this query alone, for
// the case where the DB is not in strict mode but this particular
// result must not carry a silently-empty relation.
func (f *FindBuilder) Strict() *FindBuilder { f.strict = true; return f }

// NoLoad declares that this query deliberately does not load rels —
// the waiver the strict-loading check accepts, and the checked
// counterpart of [FindBuilder.Without]. Its name is SQLAlchemy's
// noload, and it means the same thing: not "do not load", which is
// already the default, but "I know it is not loaded and I will not
// read it".
//
//	db.Find(Users).Load(UserPosts).NoLoad(UserProfile).All(ctx, &users)
//
// A nil relation is ignored. Outside strict mode NoLoad does nothing,
// so it is safe to leave in code that runs both ways.
func (f *FindBuilder) NoLoad(rels ...*Relation) *FindBuilder {
	for _, r := range rels {
		if r == nil {
			continue
		}
		f.Without(r.Name)
	}
	return f
}

// Without is [FindBuilder.NoLoad] taking names rather than handles, and
// it takes the same dot paths With does: Without("posts.comments")
// waives the comments of the posts this query loads.
//
// A waiver is recorded as a path and matched when the check runs, so
// it neither loads anything itself nor depends on where in the chain
// it appears — Without("posts.comments") before or after With("posts")
// means the same thing.
func (f *FindBuilder) Without(names ...string) *FindBuilder {
	f.waived = append(f.waived, names...)
	return f
}

// NoLoad declares that this eager-loaded relation deliberately does not
// load rels beneath it — the nested counterpart of
// [FindBuilder.NoLoad].
func (c *RelConfig) NoLoad(rels ...*Relation) *RelConfig {
	for _, r := range rels {
		if r == nil {
			continue
		}
		c.node.waived = append(c.node.waived, r.Name)
	}
	return c
}

// Without is [RelConfig.NoLoad] taking names. Unlike the FindBuilder's
// it takes a single relation name, not a path: it is already scoped to
// this edge.
func (c *RelConfig) Without(names ...string) *RelConfig {
	c.node.waived = append(c.node.waived, names...)
	return c
}

// checkStrictLoading walks structT against the table's declared
// relations and refuses the query if one of them has a field on the
// struct that nothing in this query fills. Cheap and pure: no
// allocation beyond the error it may return, and no round trip.
func (f *FindBuilder) checkStrictLoading(structT reflect.Type) error {
	if !f.strict || structT == nil || structT.Kind() != reflect.Struct {
		return nil
	}
	return checkStrictLevel(structT, f.table, f.roots, nil, "", f.waived)
}

// checkStrictLevel walks one level of the load tree. edgeWaived holds
// the bare names [RelConfig.NoLoad] declared on the edge this level
// hangs off; paths holds the FindBuilder's dot-path waivers, matched
// against each relation's full path. Two lists because the two
// waivers are written in different places and neither can see the
// other's spelling.
func checkStrictLevel(
	structT reflect.Type,
	t *Table,
	nodes []*relNode,
	edgeWaived []string,
	prefix string,
	paths []string,
) error {
	if t == nil {
		return nil
	}
	names := make([]string, 0, len(t.relations))
	for name := range t.relations {
		names = append(names, name)
	}
	// Deterministic order: a query missing two relations must always
	// report the same one first.
	sort.Strings(names)

	for _, name := range names {
		field, ok := relationTargetField(structT, name)
		if !ok {
			// The destination struct does not carry this relation at
			// all — a projection DTO, say. Nothing can read a field
			// that is not there, so nothing to guard.
			continue
		}
		fieldType := structT.FieldByIndex(field).Type
		childT, shaped := relationStructType(fieldType)
		if !shaped {
			// A column field that happens to share a name with a
			// relation. relationTargetField matches by name as a
			// fallback, and a plain scalar is not a relation target.
			continue
		}
		path := name
		if prefix != "" {
			path = prefix + "." + name
		}
		if containsName(edgeWaived, name) || containsName(paths, path) {
			continue
		}
		node := findNode(nodes, name)
		if node == nil {
			return notLoadedError(structT, t, name, path)
		}
		if childT == nil {
			// MorphTo: the field is `any` and the loaded type varies
			// row by row, so there is no child struct to descend into.
			continue
		}
		rel := t.relations[name]
		if err := checkStrictLevel(childT, rel.To, node.children, node.waived, path, paths); err != nil {
			return err
		}
	}
	return nil
}

func notLoadedError(structT reflect.Type, t *Table, name, path string) error {
	return fmt.Errorf(
		"%w: %q on struct %s — this query never loaded it, and an unloaded relation field is indistinguishable from an empty one. "+
			"Load it with .Load(%s.Rel(%q)) or .With(%q), or say the query does not need it with .NoLoad(%s.Rel(%q)) or .Without(%q)",
		ErrRelationNotLoaded, path, structName(structT),
		t.Name(), name, path,
		t.Name(), name, path)
}

// structName renders a struct type the way a caller would recognise
// it: package-qualified, which is how it reads at the declaration.
func structName(t reflect.Type) string { return t.String() }

var timeType = reflect.TypeOf(time.Time{})

// relationStructType reports whether ft could hold a loaded relation
// and, when it could, the struct type behind it. The second return is
// false for a scalar column field; the first is nil for an interface
// field (a MorphTo target), which is relation-shaped but has no single
// struct type to descend into.
func relationStructType(ft reflect.Type) (reflect.Type, bool) {
	if ft.Kind() == reflect.Interface {
		return nil, true
	}
	for ft.Kind() == reflect.Slice || ft.Kind() == reflect.Ptr {
		ft = ft.Elem()
	}
	if ft.Kind() != reflect.Struct || ft == timeType {
		return nil, false
	}
	return ft, true
}

func findNode(nodes []*relNode, name string) *relNode {
	for _, n := range nodes {
		if n.name == name {
			return n
		}
	}
	return nil
}

func containsName(names []string, name string) bool {
	for _, n := range names {
		if n == name {
			return true
		}
	}
	return false
}

// destStructType peels a *[]T / *[]*T / *T destination down to the
// struct type the rows land in. Returns nil when dest is not one of
// those shapes — the strict check has nothing to say about a
// destination the scanner is going to reject anyway.
func destStructType(dest any) reflect.Type {
	t := reflect.TypeOf(dest)
	if t == nil || t.Kind() != reflect.Ptr {
		return nil
	}
	t = t.Elem()
	if t.Kind() == reflect.Slice {
		t = t.Elem()
	}
	for t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct {
		return nil
	}
	return t
}
