package pg

import (
	"context"
	"fmt"
	"reflect"
	"sort"
	"strings"

	"github.com/bernardoforcillo/drops"
)

// FindBuilder composes a SELECT and (optionally) eager-loads relations
// declared via NewRelations. It mirrors a subset of SelectBuilder's API
// — Where, OrderBy, Limit, Offset — and adds With/WithRel for relations.
type FindBuilder struct {
	db     *DB
	table  *Table
	sel    *SelectBuilder
	roots  []*relNode
	relErr error // first deferred path-parse error, surfaced from All
}

// relNode is one node in the parsed eager-load tree. A path such as
// "posts.comments" becomes a posts node with a comments child. Optional
// per-relation constraints (set via WithRel) are AND-ed into / appended
// onto that edge's batched query.
type relNode struct {
	name     string
	wheres   []drops.Expression
	orderBys []drops.Expression
	children []*relNode
}

// mergeRelPath inserts a dot-separated path into a relNode forest,
// reusing nodes that already exist so shared prefixes are visited once.
// It returns the leaf node so callers can attach constraints to it.
func mergeRelPath(level *[]*relNode, path string) (*relNode, error) {
	var leaf *relNode
	for _, seg := range strings.Split(path, ".") {
		if seg == "" {
			return nil, fmt.Errorf("drops/pg: invalid relation path %q", path)
		}
		var node *relNode
		for _, n := range *level {
			if n.name == seg {
				node = n
				break
			}
		}
		if node == nil {
			node = &relNode{name: seg}
			*level = append(*level, node)
		}
		leaf = node
		level = &node.children
	}
	return leaf, nil
}

// RelConfig configures a single eager-loaded relation: filter the related
// rows with Where, order them with OrderBy, and declare deeper relations
// with With/WithRel. It is handed to the callback passed to WithRel.
//
// Where predicates and OrderBy expressions reference the related table's
// columns and are applied to that relation's batched query, so they cost
// nothing extra — one query per edge, just narrower or sorted.
type RelConfig struct {
	node *relNode
	err  *error // shared with the owning FindBuilder for error propagation
}

// Where AND-s predicates into this relation's batched query, filtering
// the related rows (e.g. only published posts).
func (c *RelConfig) Where(preds ...drops.Expression) *RelConfig {
	c.node.wheres = append(c.node.wheres, preds...)
	return c
}

// OrderBy appends ORDER BY expressions to this relation's batched query.
// Each parent's loaded slice ends up in this order.
func (c *RelConfig) OrderBy(exprs ...drops.Expression) *RelConfig {
	c.node.orderBys = append(c.node.orderBys, exprs...)
	return c
}

// With declares deeper relations to eager-load beneath this one, using
// the same dot-path syntax as FindBuilder.With.
func (c *RelConfig) With(names ...string) *RelConfig {
	for _, path := range names {
		if _, err := mergeRelPath(&c.node.children, path); err != nil && *c.err == nil {
			*c.err = err
		}
	}
	return c
}

// WithRel declares a deeper, individually configured relation beneath
// this one.
func (c *RelConfig) WithRel(name string, fn func(*RelConfig)) *RelConfig {
	leaf, err := mergeRelPath(&c.node.children, name)
	if err != nil {
		if *c.err == nil {
			*c.err = err
		}
		return c
	}
	if fn != nil {
		fn(&RelConfig{node: leaf, err: c.err})
	}
	return c
}

// Find begins a relational query against t. The result type passed to
// All/One determines what columns are scanned (via the same struct-field
// mapping rules as Select.All).
func (db *DB) Find(t *Table) *FindBuilder {
	return &FindBuilder{db: db, table: t, sel: db.Select().From(t)}
}

// Where appends predicates joined by AND.
func (f *FindBuilder) Where(preds ...drops.Expression) *FindBuilder {
	f.sel.Where(preds...)
	return f
}

// OrderBy appends ORDER BY expressions.
func (f *FindBuilder) OrderBy(exprs ...drops.Expression) *FindBuilder {
	f.sel.OrderBy(exprs...)
	return f
}

// Limit sets the LIMIT.
func (f *FindBuilder) Limit(n int64) *FindBuilder { f.sel.Limit(n); return f }

// Offset sets the OFFSET.
func (f *FindBuilder) Offset(n int64) *FindBuilder { f.sel.Offset(n); return f }

// Unscoped opts out of the table's DefaultFilter predicates for the
// root SELECT. Eager-loaded relations inherit their own table's
// scopes independently.
func (f *FindBuilder) Unscoped() *FindBuilder { f.sel.Unscoped(); return f }

// HasEagerLoads reports whether any relations have been queued for
// eager loading via With / WithRel. Used by Entity[T] to decide
// whether the fast-scan path is safe — relation loaders need the
// reflection-populated parent slice.
func (f *FindBuilder) HasEagerLoads() bool { return len(f.roots) > 0 }

// Select exposes the underlying SELECT builder so callers (typically
// Entity[T]) can stream results through their own scanner.
func (f *FindBuilder) Select() *SelectBuilder { return f.sel }

// With marks one or more relations to eager-load. Names must match
// relations declared on the table via NewRelations.
//
// Nested relations are expressed with dot paths: With("posts.comments")
// loads each parent's posts, then every comment of those posts. Paths
// that share a prefix are merged, so With("posts.comments", "posts.tags")
// fetches posts once and fans out into comments and tags. Each relation
// edge costs one extra query regardless of how many parents it spans.
//
// Use WithRel when a relation needs filtering or ordering.
func (f *FindBuilder) With(names ...string) *FindBuilder {
	for _, path := range names {
		if _, err := mergeRelPath(&f.roots, path); err != nil && f.relErr == nil {
			f.relErr = err
		}
	}
	return f
}

// WithRel eager-loads a single relation with per-relation constraints.
// The callback receives a RelConfig to filter (Where), order (OrderBy),
// and declare deeper relations (With/WithRel):
//
//	db.Find(Users).WithRel("posts", func(p *pg.RelConfig) {
//	    p.Where(Posts.Published.Eq(true)).
//	        OrderBy(Posts.CreatedAt.Desc()).
//	        With("comments")
//	}).All(ctx, &users)
//
// name may itself be a dot path; the constraints attach to its leaf. A
// relation configured by WithRel and also named in With merges into the
// same edge, so it is still fetched once.
func (f *FindBuilder) WithRel(name string, fn func(*RelConfig)) *FindBuilder {
	leaf, err := mergeRelPath(&f.roots, name)
	if err != nil {
		if f.relErr == nil {
			f.relErr = err
		}
		return f
	}
	if fn != nil {
		fn(&RelConfig{node: leaf, err: &f.relErr})
	}
	return f
}

// All runs the find and populates dest, which must be *[]Struct or
// *[]*Struct. Each requested relation is loaded with a single follow-up
// query and stitched onto the right field of every parent.
func (f *FindBuilder) All(ctx context.Context, dest any) error {
	// Surface any malformed-path error captured while building the tree.
	if f.relErr != nil {
		return f.relErr
	}
	// Validate the whole tree up front so a typo in any path — at any
	// depth — fails fast, before a single query runs.
	if err := validateRelTree(f.table, f.roots); err != nil {
		return err
	}
	if err := f.sel.All(ctx, dest); err != nil {
		return err
	}
	if len(f.roots) == 0 {
		return nil
	}
	rv := reflect.ValueOf(dest).Elem()
	if rv.Len() == 0 {
		return nil
	}
	elemType := rv.Type().Elem()
	parentIsPtr := elemType.Kind() == reflect.Ptr
	structType := elemType
	if parentIsPtr {
		structType = elemType.Elem()
	}
	if structType.Kind() != reflect.Struct {
		return fmt.Errorf("drops/pg: Find.All needs a slice of struct or *struct, got %s", structType.Kind())
	}
	for _, node := range f.roots {
		if err := f.loadRelationTree(ctx, rv, structType, parentIsPtr, f.table, node); err != nil {
			return err
		}
	}
	return nil
}

// validateRelTree checks every node against the schema, descending into
// each relation's target table, so unknown relations are reported before
// any query executes.
func validateRelTree(t *Table, nodes []*relNode) error {
	for _, node := range nodes {
		rel := t.Relation(node.name)
		if rel == nil {
			return fmt.Errorf("drops/pg: unknown relation %q on table %q", node.name, t.Name())
		}
		if len(node.children) > 0 {
			if rel.Kind == MorphToKind {
				return fmt.Errorf("drops/pg: relation %q is MorphTo — nested With() is not supported because the loaded parent's type varies row by row",
					node.name)
			}
			if err := validateRelTree(rel.To, node.children); err != nil {
				return err
			}
		}
	}
	return nil
}

// loadRelationTree loads one relation edge onto parentSlice and, when the
// node has children, recurses into the freshly loaded rows. The recursion
// operates on pointers into the live result data, so nested assignments
// land on the structs the caller will read back.
func (f *FindBuilder) loadRelationTree(
	ctx context.Context,
	parentSlice reflect.Value,
	parentType reflect.Type,
	parentIsPtr bool,
	table *Table,
	node *relNode,
) error {
	rel := table.Relation(node.name)
	if rel == nil {
		return fmt.Errorf("drops/pg: unknown relation %q on table %q", node.name, table.Name())
	}
	if parentSlice.Len() == 0 {
		return nil
	}
	children, childType, err := f.loadOne(ctx, parentSlice, parentType, parentIsPtr, rel, node)
	if err != nil {
		return err
	}
	if len(node.children) == 0 || !children.IsValid() || children.Len() == 0 {
		return nil
	}
	for _, child := range node.children {
		if err := f.loadRelationTree(ctx, children, childType, true, rel.To, child); err != nil {
			return err
		}
	}
	return nil
}

// loadOne dispatches a single relation edge to the right loader, applying
// the node's per-relation constraints. When the node has children it
// returns a []*Child slice of pointers into the live data plus the child
// struct type for the recursion.
func (f *FindBuilder) loadOne(
	ctx context.Context,
	parentSlice reflect.Value,
	parentType reflect.Type,
	parentIsPtr bool,
	rel *Relation,
	node *relNode,
) (reflect.Value, reflect.Type, error) {
	switch rel.Kind {
	case ManyToManyKind:
		return f.loadManyToMany(ctx, parentSlice, parentType, parentIsPtr, rel, node)
	case MorphToKind:
		return f.loadMorphTo(ctx, parentSlice, parentType, parentIsPtr, rel, node)
	}
	return f.loadRelation(ctx, parentSlice, parentType, parentIsPtr, rel, node)
}

// One runs the find and populates dest, a pointer to a struct. Returns
// ErrNoRows if no row matches.
func (f *FindBuilder) One(ctx context.Context, dest any) error {
	rv := reflect.ValueOf(dest)
	if rv.Kind() != reflect.Ptr || rv.Elem().Kind() != reflect.Struct {
		return fmt.Errorf("drops/pg: Find.One needs a pointer to struct, got %T", dest)
	}
	sliceType := reflect.SliceOf(rv.Type().Elem())
	slice := reflect.New(sliceType).Elem()
	f.sel.Limit(1)
	if err := f.All(ctx, slice.Addr().Interface()); err != nil {
		return err
	}
	if slice.Len() == 0 {
		return ErrNoRows
	}
	rv.Elem().Set(slice.Index(0))
	return nil
}

// loadRelation runs the secondary query for one relation and stitches
// children/parent rows onto the corresponding struct field. The node's
// Where/OrderBy constraints are applied to the batched child query. When
// the node has children it returns a []*Child slice of pointers into the
// freshly populated fields (and the child struct type) so a nested edge
// can be loaded against the live data.
func (f *FindBuilder) loadRelation(
	ctx context.Context,
	parentSlice reflect.Value,
	parentType reflect.Type,
	parentIsPtr bool,
	rel *Relation,
	node *relNode,
) (reflect.Value, reflect.Type, error) {
	var none reflect.Value
	needChildren := len(node.children) > 0

	var rowKeyCol, targetKeyCol *Column
	switch rel.Kind {
	case HasManyKind, HasOneKind, MorphManyKind:
		rowKeyCol = rel.ParentKey
		targetKeyCol = rel.ChildKey
	case BelongsToKind:
		rowKeyCol = rel.ChildKey
		targetKeyCol = rel.ParentKey
	default:
		return none, nil, fmt.Errorf("drops/pg: relation %q has unknown kind", rel.Name)
	}

	rowKeyField, ok := relationKeyField(parentType, rowKeyCol)
	if !ok {
		return none, nil, fmt.Errorf("drops/pg: relation %q: parent struct missing field for column %q",
			rel.Name, rowKeyCol.Name())
	}
	relField, ok := relationTargetField(parentType, rel.Name)
	if !ok {
		return none, nil, fmt.Errorf("drops/pg: relation %q: parent struct has no field tagged db_rel:%q (or matching name)",
			rel.Name, rel.Name)
	}
	relFieldType := parentType.FieldByIndex(relField).Type

	expectsSlice := rel.Kind == HasManyKind || rel.Kind == MorphManyKind
	if expectsSlice && relFieldType.Kind() != reflect.Slice {
		return none, nil, fmt.Errorf("drops/pg: relation %q is HasMany — expected slice field, got %s",
			rel.Name, relFieldType.Kind())
	}

	var childElemType reflect.Type
	if expectsSlice {
		childElemType = relFieldType.Elem()
	} else {
		childElemType = relFieldType
	}
	childIsPtr := childElemType.Kind() == reflect.Ptr
	childStructType := childElemType
	if childIsPtr {
		childStructType = childStructType.Elem()
	}
	if childStructType.Kind() != reflect.Struct {
		return none, nil, fmt.Errorf("drops/pg: relation %q: target field is neither struct nor slice of struct",
			rel.Name)
	}

	rowKeys := dedupeKeys(collectKeys(parentSlice, parentIsPtr, rowKeyField))
	if len(rowKeys) == 0 {
		return none, childStructType, nil
	}

	childQuery := f.db.Select().From(rel.To).Where(In(targetKeyCol, rowKeys...))
	if rel.Kind == MorphManyKind {
		childQuery.Where(Eq(rel.MorphTypeCol, rel.MorphType))
	}
	if len(node.wheres) > 0 {
		childQuery.Where(node.wheres...)
	}
	if len(node.orderBys) > 0 {
		childQuery.OrderBy(node.orderBys...)
	}
	childSliceType := reflect.SliceOf(childStructType)
	childSlice := reflect.New(childSliceType).Elem()
	if err := childQuery.All(ctx, childSlice.Addr().Interface()); err != nil {
		return none, nil, err
	}

	targetKeyField, ok := relationKeyField(childStructType, targetKeyCol)
	if !ok {
		return none, nil, fmt.Errorf("drops/pg: relation %q: target struct %s missing field for column %q",
			rel.Name, childStructType.Name(), targetKeyCol.Name())
	}

	if expectsSlice {
		grouped := map[any]reflect.Value{}
		for i := 0; i < childSlice.Len(); i++ {
			ch := childSlice.Index(i)
			k := ch.FieldByIndex(targetKeyField).Interface()
			if existing, ok := grouped[k]; ok {
				grouped[k] = reflect.Append(existing, ch)
			} else {
				grouped[k] = reflect.Append(reflect.MakeSlice(childSliceType, 0, 1), ch)
			}
		}
		for i := 0; i < parentSlice.Len(); i++ {
			parent := parentValue(parentSlice.Index(i), parentIsPtr)
			k := parent.FieldByIndex(rowKeyField).Interface()
			target := parent.FieldByIndex(relField)
			if children, ok := grouped[k]; ok {
				target.Set(coerceSlice(children, relFieldType))
			} else {
				target.Set(reflect.MakeSlice(relFieldType, 0, 0))
			}
		}
		return collectChildPtrs(parentSlice, parentIsPtr, relField, childStructType, needChildren), childStructType, nil
	}

	indexed := map[any]reflect.Value{}
	for i := 0; i < childSlice.Len(); i++ {
		ch := childSlice.Index(i)
		k := ch.FieldByIndex(targetKeyField).Interface()
		indexed[k] = ch
	}
	for i := 0; i < parentSlice.Len(); i++ {
		parent := parentValue(parentSlice.Index(i), parentIsPtr)
		k := parent.FieldByIndex(rowKeyField).Interface()
		ch, ok := indexed[k]
		target := parent.FieldByIndex(relField)
		if !ok {
			continue
		}
		if childIsPtr {
			ptr := reflect.New(childStructType)
			ptr.Elem().Set(ch)
			target.Set(ptr)
		} else {
			target.Set(ch)
		}
	}
	return collectChildPtrs(parentSlice, parentIsPtr, relField, childStructType, needChildren), childStructType, nil
}

// loadManyToMany loads a many-to-many relation via two queries: one
// against the junction table, one against the target table. The node's
// Where/OrderBy constraints are applied to the target query; OrderBy also
// re-sorts each parent's slice into target-query order (the default,
// without OrderBy, follows junction-row order). When the node has
// children it returns a []*Child slice of pointers into the freshly
// populated fields for nested loading.
func (f *FindBuilder) loadManyToMany(
	ctx context.Context,
	parentSlice reflect.Value,
	parentType reflect.Type,
	parentIsPtr bool,
	rel *Relation,
	node *relNode,
) (reflect.Value, reflect.Type, error) {
	var none reflect.Value
	needChildren := len(node.children) > 0

	rowKeyField, ok := relationKeyField(parentType, rel.ParentKey)
	if !ok {
		return none, nil, fmt.Errorf("drops/pg: relation %q: parent struct missing field for column %q",
			rel.Name, rel.ParentKey.Name())
	}
	relField, ok := relationTargetField(parentType, rel.Name)
	if !ok {
		return none, nil, fmt.Errorf("drops/pg: relation %q: parent struct has no field tagged db_rel:%q (or matching name)",
			rel.Name, rel.Name)
	}
	relFieldType := parentType.FieldByIndex(relField).Type
	if relFieldType.Kind() != reflect.Slice {
		return none, nil, fmt.Errorf("drops/pg: relation %q is ManyToMany — expected slice field, got %s",
			rel.Name, relFieldType.Kind())
	}
	childElemType := relFieldType.Elem()
	childIsPtr := childElemType.Kind() == reflect.Ptr
	childStructType := childElemType
	if childIsPtr {
		childStructType = childStructType.Elem()
	}
	if childStructType.Kind() != reflect.Struct {
		return none, nil, fmt.Errorf("drops/pg: relation %q: slice element must be struct or *struct",
			rel.Name)
	}

	rowKeys := dedupeKeys(collectKeys(parentSlice, parentIsPtr, rowKeyField))
	if len(rowKeys) == 0 {
		return none, childStructType, nil
	}

	// Step 1: junction query. Defer Close so every exit path frees the
	// cursor, including panics and the targetKeyField lookup below.
	junctionRows, err := f.db.Select(rel.ThroughFK1, rel.ThroughFK2).
		From(rel.Through).
		Where(In(rel.ThroughFK1, rowKeys...)).
		Rows(ctx)
	if err != nil {
		return none, nil, err
	}
	defer junctionRows.Close()

	// Allocate scan targets of the same Go type as the parent/child key
	// fields, so the values are comparable as map keys downstream.
	rowKeyType := parentType.FieldByIndex(rowKeyField).Type
	targetKeyField, ok := relationKeyField(childStructType, rel.ChildKey)
	if !ok {
		return none, nil, fmt.Errorf("drops/pg: relation %q: target struct %s missing field for column %q",
			rel.Name, childStructType.Name(), rel.ChildKey.Name())
	}
	targetKeyType := childStructType.FieldByIndex(targetKeyField).Type

	remoteByLocal := map[any][]any{} // local key → list of remote keys
	remoteSet := map[any]struct{}{}
	remoteKeys := []any{}
	for junctionRows.Next() {
		localPtr := reflect.New(rowKeyType)
		remotePtr := reflect.New(targetKeyType)
		if err := junctionRows.Scan(localPtr.Interface(), remotePtr.Interface()); err != nil {
			return none, nil, err
		}
		localVal := localPtr.Elem().Interface()
		remoteVal := remotePtr.Elem().Interface()
		remoteByLocal[localVal] = append(remoteByLocal[localVal], remoteVal)
		if _, seen := remoteSet[remoteVal]; !seen {
			remoteSet[remoteVal] = struct{}{}
			remoteKeys = append(remoteKeys, remoteVal)
		}
	}
	if err := junctionRows.Err(); err != nil {
		return none, nil, err
	}

	// Empty junction — assign empty slices and bail.
	if len(remoteKeys) == 0 {
		for i := 0; i < parentSlice.Len(); i++ {
			parent := parentValue(parentSlice.Index(i), parentIsPtr)
			parent.FieldByIndex(relField).Set(reflect.MakeSlice(relFieldType, 0, 0))
		}
		return none, childStructType, nil
	}

	// Step 2: target query, narrowed/sorted by the node's constraints.
	targetQuery := f.db.Select().From(rel.To).Where(In(rel.ChildKey, remoteKeys...))
	if len(node.wheres) > 0 {
		targetQuery.Where(node.wheres...)
	}
	if len(node.orderBys) > 0 {
		targetQuery.OrderBy(node.orderBys...)
	}
	childSliceType := reflect.SliceOf(childStructType)
	childSlice := reflect.New(childSliceType).Elem()
	if err := targetQuery.All(ctx, childSlice.Addr().Interface()); err != nil {
		return none, nil, err
	}

	// targetByKey resolves a remote key to its row; targetOrder records
	// each row's position in the (possibly ordered, possibly filtered)
	// target query so we can re-sort per parent when OrderBy is set.
	targetByKey := map[any]reflect.Value{}
	targetOrder := map[any]int{}
	for i := 0; i < childSlice.Len(); i++ {
		ch := childSlice.Index(i)
		k := ch.FieldByIndex(targetKeyField).Interface()
		targetByKey[k] = ch
		targetOrder[k] = i
	}

	appendTarget := func(result, tv reflect.Value) reflect.Value {
		if childIsPtr {
			ptr := reflect.New(childStructType)
			ptr.Elem().Set(tv)
			return reflect.Append(result, ptr)
		}
		return reflect.Append(result, tv)
	}

	ordered := len(node.orderBys) > 0
	for i := 0; i < parentSlice.Len(); i++ {
		parent := parentValue(parentSlice.Index(i), parentIsPtr)
		k := parent.FieldByIndex(rowKeyField).Interface()
		target := parent.FieldByIndex(relField)
		remotes := remoteByLocal[k]
		result := reflect.MakeSlice(relFieldType, 0, len(remotes))
		if ordered {
			// Keep only this parent's linked targets, in target order. A
			// Where on the target may have dropped some; targetOrder only
			// holds rows that survived, so missing keys are skipped.
			kept := make([]any, 0, len(remotes))
			seen := map[any]struct{}{}
			for _, rk := range remotes {
				if _, ok := targetOrder[rk]; !ok {
					continue
				}
				if _, dup := seen[rk]; dup {
					continue
				}
				seen[rk] = struct{}{}
				kept = append(kept, rk)
			}
			sort.SliceStable(kept, func(a, b int) bool {
				return targetOrder[kept[a]] < targetOrder[kept[b]]
			})
			for _, rk := range kept {
				result = appendTarget(result, targetByKey[rk])
			}
		} else {
			// Default: preserve junction-row order.
			for _, rk := range remotes {
				if tv, ok := targetByKey[rk]; ok {
					result = appendTarget(result, tv)
				}
			}
		}
		target.Set(result)
	}
	return collectChildPtrs(parentSlice, parentIsPtr, relField, childStructType, needChildren), childStructType, nil
}

func parentValue(v reflect.Value, isPtr bool) reflect.Value {
	if isPtr {
		return v.Elem()
	}
	return v
}

func collectKeys(slice reflect.Value, isPtr bool, fieldIdx []int) []any {
	out := make([]any, slice.Len())
	for i := 0; i < slice.Len(); i++ {
		v := parentValue(slice.Index(i), isPtr)
		out[i] = v.FieldByIndex(fieldIdx).Interface()
	}
	return out
}

func dedupeKeys(in []any) []any {
	if len(in) <= 1 {
		return in
	}
	seen := make(map[any]struct{}, len(in))
	out := make([]any, 0, len(in))
	for _, v := range in {
		if _, ok := seen[v]; ok {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	return out
}

// collectChildPtrs gathers *Child pointers from the relation field that
// was just populated on each parent, so a nested relation can be loaded
// against the live structs. It returns an invalid Value when collection
// isn't needed.
//
// Every result aliases the stored data — slice elements by address (or
// verbatim when already pointers), single struct fields by address, and
// non-nil pointer fields verbatim — so nested assignments made through
// the returned pointers are visible to the original caller. Unmatched
// to-one relations (nil pointers, zero structs) are skipped to avoid
// firing nested queries against rows that were never loaded.
func collectChildPtrs(parentSlice reflect.Value, parentIsPtr bool, relField []int, childStructType reflect.Type, needChildren bool) reflect.Value {
	if !needChildren {
		return reflect.Value{}
	}
	out := reflect.MakeSlice(reflect.SliceOf(reflect.PointerTo(childStructType)), 0, parentSlice.Len())
	for i := 0; i < parentSlice.Len(); i++ {
		parent := parentValue(parentSlice.Index(i), parentIsPtr)
		field := parent.FieldByIndex(relField)
		switch field.Kind() {
		case reflect.Slice:
			for j := 0; j < field.Len(); j++ {
				elem := field.Index(j)
				if elem.Kind() == reflect.Ptr {
					if !elem.IsNil() {
						out = reflect.Append(out, elem)
					}
				} else {
					out = reflect.Append(out, elem.Addr())
				}
			}
		case reflect.Ptr:
			if !field.IsNil() {
				out = reflect.Append(out, field)
			}
		case reflect.Struct:
			if !field.IsZero() {
				out = reflect.Append(out, field.Addr())
			}
		}
	}
	return out
}

func coerceSlice(src reflect.Value, dstType reflect.Type) reflect.Value {
	if src.Type() == dstType {
		return src
	}
	out := reflect.MakeSlice(dstType, src.Len(), src.Len())
	for i := 0; i < src.Len(); i++ {
		elem := src.Index(i)
		if dstType.Elem().Kind() == reflect.Ptr {
			ptr := reflect.New(elem.Type())
			ptr.Elem().Set(elem)
			out.Index(i).Set(ptr)
		} else {
			out.Index(i).Set(elem)
		}
	}
	return out
}

// relationKeyField returns the index path of the struct field that maps
// to col, using the same matching rules as scan (db tag, exact name,
// snake_case name).
func relationKeyField(structT reflect.Type, col *Column) ([]int, bool) {
	fm := fieldMap(structT)
	if idx, ok := fm[col.Name()]; ok {
		return idx, true
	}
	return nil, false
}

// relationTargetField returns the index path of the struct field that
// receives the relation. Lookup order: db_rel:"<name>" tag, then a
// case-insensitive name match.
func relationTargetField(structT reflect.Type, name string) ([]int, bool) {
	var found []int
	var byName []int
	var walk func(reflect.Type, []int)
	walk = func(t reflect.Type, prefix []int) {
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			if !f.IsExported() {
				continue
			}
			idx := append(append([]int(nil), prefix...), i)
			if tag := f.Tag.Get("db_rel"); tag == name {
				found = idx
				return
			}
			if byName == nil && strings.EqualFold(f.Name, name) {
				byName = idx
			}
			if f.Anonymous && f.Type.Kind() == reflect.Struct {
				walk(f.Type, idx)
			}
		}
	}
	walk(structT, nil)
	if found != nil {
		return found, true
	}
	if byName != nil {
		return byName, true
	}
	return nil, false
}

// loadMorphTo handles polymorphic belongs-to: each child carries
// (morph_type, morph_id), and we have to look up each id in the
// table registered for its morph_type. We bucket children by
// morph_type, run one query per bucket, and stitch the loaded parent
// back into the child's relation field — which must be of interface
// kind because its concrete type varies row by row.
func (f *FindBuilder) loadMorphTo(
	ctx context.Context,
	parentSlice reflect.Value,
	parentType reflect.Type,
	parentIsPtr bool,
	rel *Relation,
	node *relNode,
) (reflect.Value, reflect.Type, error) {
	var none reflect.Value
	idField, ok := relationKeyField(parentType, rel.ChildKey)
	if !ok {
		return none, nil, fmt.Errorf("drops/pg: relation %q: struct missing field for column %q",
			rel.Name, rel.ChildKey.Name())
	}
	typeField, ok := relationKeyField(parentType, rel.MorphTypeCol)
	if !ok {
		return none, nil, fmt.Errorf("drops/pg: relation %q: struct missing field for column %q",
			rel.Name, rel.MorphTypeCol.Name())
	}
	relField, ok := relationTargetField(parentType, rel.Name)
	if !ok {
		return none, nil, fmt.Errorf("drops/pg: relation %q: struct has no field tagged db_rel:%q (or matching name)",
			rel.Name, rel.Name)
	}
	relFieldType := parentType.FieldByIndex(relField).Type
	if relFieldType.Kind() != reflect.Interface {
		return none, nil, fmt.Errorf("drops/pg: relation %q: polymorphic field %s must be an interface type (typically `any`), got %s",
			rel.Name, parentType.FieldByIndex(relField).Name, relFieldType.Kind())
	}

	// Group child rows by morph_type. ids is per-type; revIndex points
	// back at the parentSlice rows that have to receive the loaded
	// parent.
	type bucket struct {
		ids       []any
		rowIdxs   []int
		entry     morphEntry
		idToParent map[any]reflect.Value
	}
	buckets := map[string]*bucket{}
	for i := 0; i < parentSlice.Len(); i++ {
		row := parentValue(parentSlice.Index(i), parentIsPtr)
		typeStr := fmt.Sprintf("%v", row.FieldByIndex(typeField).Interface())
		if typeStr == "" {
			continue
		}
		entry, ok := rel.MorphMap.lookup(typeStr)
		if !ok {
			return none, nil, fmt.Errorf("drops/pg: relation %q: unknown morph type %q (register it via pg.RegisterMorph)",
				rel.Name, typeStr)
		}
		b, ok := buckets[typeStr]
		if !ok {
			b = &bucket{entry: entry}
			buckets[typeStr] = b
		}
		b.ids = append(b.ids, row.FieldByIndex(idField).Interface())
		b.rowIdxs = append(b.rowIdxs, i)
	}

	// Run one query per bucket and build per-bucket id → parent maps.
	for _, b := range buckets {
		ids := dedupeKeys(b.ids)
		// Identify the target PK column.
		var targetPK *Column
		for _, c := range b.entry.table.Columns() {
			if c.IsPrimaryKey() {
				targetPK = c
				break
			}
		}
		if targetPK == nil {
			return none, nil, fmt.Errorf("drops/pg: relation %q: morph target table %q has no PRIMARY KEY",
				rel.Name, b.entry.table.Name())
		}
		q := f.db.Select().From(b.entry.table).Where(In(targetPK, ids...))
		if len(node.wheres) > 0 {
			q.Where(node.wheres...)
		}
		if len(node.orderBys) > 0 {
			q.OrderBy(node.orderBys...)
		}
		sliceType := reflect.SliceOf(b.entry.rowType)
		results := reflect.New(sliceType).Elem()
		if err := q.All(ctx, results.Addr().Interface()); err != nil {
			return none, nil, err
		}
		pkField, ok := relationKeyField(b.entry.rowType, targetPK)
		if !ok {
			return none, nil, fmt.Errorf("drops/pg: relation %q: morph target struct %s missing field for PK column %q",
				rel.Name, b.entry.rowType.Name(), targetPK.Name())
		}
		b.idToParent = map[any]reflect.Value{}
		for i := 0; i < results.Len(); i++ {
			row := results.Index(i)
			k := row.FieldByIndex(pkField).Interface()
			b.idToParent[k] = row
		}
	}

	// Stitch loaded parents back into each child row.
	for _, b := range buckets {
		for j, rowIdx := range b.rowIdxs {
			parent, ok := b.idToParent[b.ids[j]]
			if !ok {
				continue
			}
			child := parentValue(parentSlice.Index(rowIdx), parentIsPtr)
			ptr := reflect.New(b.entry.rowType)
			ptr.Elem().Set(parent)
			child.FieldByIndex(relField).Set(ptr)
		}
	}

	// MorphTo doesn't support deeper relation chains in this iteration
	// — the loaded parent has a heterogeneous Go type so a single
	// child slice for recursion doesn't fit. Callers needing this can
	// fan out manually.
	_ = node.children
	return reflect.Value{}, nil, nil
}
