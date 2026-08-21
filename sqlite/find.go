package sqlite

import (
	"context"
	"fmt"
	"reflect"
	"strings"

	"github.com/bernardoforcillo/drops"
)

// FindBuilder composes a root SELECT and (optionally) eager-loads
// relations declared via NewRelations. It mirrors a subset of
// SelectBuilder's API — Where, OrderBy, Limit, Offset — and adds With
// for relations.
//
// Each requested relation costs exactly one extra batched query (an
// IN (...) over the collected parent keys), so eager loading never
// degrades into N+1.
//
// Every one of those queries is composed with this package's builder
// and run through its executors, which is what carries each target
// table's own automatic predicates onto the edge: a tenant axis
// declared on the child table restricts the children, without the
// loader knowing anything about tenants. That is the property the
// entity-level scoping never had — the loader has no Entity to ask, so
// a predicate the Entity methods injected filtered the parents and
// loaded every tenant's children.
type FindBuilder struct {
	db    *DB
	table *Table
	sel   *SelectBuilder
	withs []string
}

// Find begins a relational query against t. The result type passed to
// All/One determines which columns are scanned (via the same
// struct-field mapping rules as Select.All).
func (db *DB) Find(t *Table) *FindBuilder {
	return &FindBuilder{
		db:    db,
		table: t,
		sel:   db.Select(colExprs(t)...).From(t),
	}
}

// With marks one or more relations to eager-load. Names must match
// relations declared on the table via NewRelations. Only single-level
// (non-nested) relations are supported.
func (f *FindBuilder) With(names ...string) *FindBuilder {
	f.withs = append(f.withs, names...)
	return f
}

// Where appends predicates joined by AND to the root query.
func (f *FindBuilder) Where(preds ...drops.Expression) *FindBuilder {
	f.sel.Where(preds...)
	return f
}

// OrderBy appends ORDER BY expressions to the root query.
func (f *FindBuilder) OrderBy(exprs ...drops.Expression) *FindBuilder {
	f.sel.OrderBy(exprs...)
	return f
}

// Limit sets the LIMIT on the root query.
func (f *FindBuilder) Limit(n int64) *FindBuilder { f.sel.Limit(n); return f }

// Offset sets the OFFSET on the root query.
func (f *FindBuilder) Offset(n int64) *FindBuilder { f.sel.Offset(n); return f }

// All runs the find and populates dest, which must be *[]Struct or
// *[]*Struct. Each requested relation is loaded with a single follow-up
// query and stitched onto the matching dropRel-tagged field of every
// parent.
func (f *FindBuilder) All(ctx context.Context, dest any) error {
	// Validate all requested relations up front so a typo fails fast,
	// before the root query runs.
	for _, name := range f.withs {
		if f.table.Relation(name) == nil {
			return fmt.Errorf("drops/sqlite: unknown relation %q on table %q", name, f.table.Name())
		}
	}
	if err := f.sel.All(ctx, dest); err != nil {
		return err
	}
	if len(f.withs) == 0 {
		return nil
	}
	rv := reflect.ValueOf(dest)
	if rv.Kind() != reflect.Ptr || rv.Elem().Kind() != reflect.Slice {
		return fmt.Errorf("drops/sqlite: Find.All needs a pointer to slice, got %T", dest)
	}
	parentSlice := rv.Elem()
	if parentSlice.Len() == 0 {
		return nil
	}
	elemType := parentSlice.Type().Elem()
	parentIsPtr := elemType.Kind() == reflect.Ptr
	structType := elemType
	if parentIsPtr {
		structType = elemType.Elem()
	}
	if structType.Kind() != reflect.Struct {
		return fmt.Errorf("drops/sqlite: Find.All needs a slice of struct or *struct, got %s", structType.Kind())
	}
	for _, name := range f.withs {
		rel := f.table.Relation(name)
		if err := f.loadRelation(ctx, parentSlice, structType, parentIsPtr, rel); err != nil {
			return err
		}
	}
	return nil
}

// One runs the find and populates dest, a pointer to a struct. Returns
// ErrNoRows when no row matches.
func (f *FindBuilder) One(ctx context.Context, dest any) error {
	rv := reflect.ValueOf(dest)
	if rv.Kind() != reflect.Ptr || rv.Elem().Kind() != reflect.Struct {
		return fmt.Errorf("drops/sqlite: Find.One needs a pointer to struct, got %T", dest)
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

// loadRelation dispatches one relation edge to the right loader.
func (f *FindBuilder) loadRelation(
	ctx context.Context,
	parentSlice reflect.Value,
	parentType reflect.Type,
	parentIsPtr bool,
	rel *Relation,
) error {
	switch rel.Kind {
	case ManyToMany:
		return f.loadManyToMany(ctx, parentSlice, parentType, parentIsPtr, rel)
	case HasMany, HasOne, BelongsTo:
		return f.loadSimple(ctx, parentSlice, parentType, parentIsPtr, rel)
	default:
		return fmt.Errorf("drops/sqlite: relation %q has unknown kind", rel.Name)
	}
}

// loadSimple handles HasMany, HasOne and BelongsTo. In every case the
// join reads rel.Local from the owning rows and matches it against
// rel.Foreign on the target rows:
//
//   - HasMany:   parent.Local IN (keys) → child.Foreign, slice field.
//   - HasOne:    same join, single (pointer or value) field.
//   - BelongsTo: parent.Local is this table's FK, matched against
//     Target.Foreign, single (pointer) field.
func (f *FindBuilder) loadSimple(
	ctx context.Context,
	parentSlice reflect.Value,
	parentType reflect.Type,
	parentIsPtr bool,
	rel *Relation,
) error {
	rowKeyCol := rel.Local
	targetKeyCol := rel.Foreign

	rowKeyField, ok := relationKeyField(parentType, rowKeyCol)
	if !ok {
		return fmt.Errorf("drops/sqlite: relation %q: parent struct missing field for column %q",
			rel.Name, rowKeyCol.Name())
	}
	relField, ok := relationTargetField(parentType, rel.Name)
	if !ok {
		return fmt.Errorf("drops/sqlite: relation %q: parent struct has no field tagged dropRel:%q (or matching name)",
			rel.Name, rel.Name)
	}
	relFieldType := parentType.FieldByIndex(relField).Type

	expectsSlice := rel.Kind == HasMany
	if expectsSlice && relFieldType.Kind() != reflect.Slice {
		return fmt.Errorf("drops/sqlite: relation %q is HasMany — expected slice field, got %s",
			rel.Name, relFieldType.Kind())
	}

	childElemType := relFieldType
	if expectsSlice {
		childElemType = relFieldType.Elem()
	}
	childIsPtr := childElemType.Kind() == reflect.Ptr
	childStructType := childElemType
	if childIsPtr {
		childStructType = childStructType.Elem()
	}
	if childStructType.Kind() != reflect.Struct {
		return fmt.Errorf("drops/sqlite: relation %q: target field is neither struct nor slice of struct",
			rel.Name)
	}

	rowKeys := dedupeKeys(collectKeys(parentSlice, parentIsPtr, rowKeyField))
	if len(rowKeys) == 0 {
		return nil
	}

	childSliceType := reflect.SliceOf(childStructType)
	childSlice := reflect.New(childSliceType).Elem()
	childQuery := f.db.Select(colExprs(rel.Target)...).
		From(rel.Target).
		Where(inPredicate(targetKeyCol, rowKeys))
	if err := childQuery.All(ctx, childSlice.Addr().Interface()); err != nil {
		return err
	}

	targetKeyField, ok := relationKeyField(childStructType, targetKeyCol)
	if !ok {
		return fmt.Errorf("drops/sqlite: relation %q: target struct %s missing field for column %q",
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
		return nil
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
		if !ok {
			continue
		}
		target := parent.FieldByIndex(relField)
		if childIsPtr {
			ptr := reflect.New(childStructType)
			ptr.Elem().Set(ch)
			target.Set(ptr)
		} else {
			target.Set(ch)
		}
	}
	return nil
}

// loadManyToMany loads a many-to-many relation via two queries: one
// against the junction table (Through), one against the target. The
// junction query selects (ThroughLocal, ThroughForeign) for every
// parent key; the target query then fetches all linked rows in a single
// IN (...). Each parent's slice preserves junction-row order.
func (f *FindBuilder) loadManyToMany(
	ctx context.Context,
	parentSlice reflect.Value,
	parentType reflect.Type,
	parentIsPtr bool,
	rel *Relation,
) error {
	rowKeyField, ok := relationKeyField(parentType, rel.LocalKey)
	if !ok {
		return fmt.Errorf("drops/sqlite: relation %q: parent struct missing field for column %q",
			rel.Name, rel.LocalKey.Name())
	}
	relField, ok := relationTargetField(parentType, rel.Name)
	if !ok {
		return fmt.Errorf("drops/sqlite: relation %q: parent struct has no field tagged dropRel:%q (or matching name)",
			rel.Name, rel.Name)
	}
	relFieldType := parentType.FieldByIndex(relField).Type
	if relFieldType.Kind() != reflect.Slice {
		return fmt.Errorf("drops/sqlite: relation %q is ManyToMany — expected slice field, got %s",
			rel.Name, relFieldType.Kind())
	}
	childElemType := relFieldType.Elem()
	childIsPtr := childElemType.Kind() == reflect.Ptr
	childStructType := childElemType
	if childIsPtr {
		childStructType = childStructType.Elem()
	}
	if childStructType.Kind() != reflect.Struct {
		return fmt.Errorf("drops/sqlite: relation %q: slice element must be struct or *struct",
			rel.Name)
	}

	rowKeys := dedupeKeys(collectKeys(parentSlice, parentIsPtr, rowKeyField))
	if len(rowKeys) == 0 {
		return nil
	}

	targetKeyField, ok := relationKeyField(childStructType, rel.TargetKey)
	if !ok {
		return fmt.Errorf("drops/sqlite: relation %q: target struct %s missing field for column %q",
			rel.Name, childStructType.Name(), rel.TargetKey.Name())
	}

	// Step 1: junction query. Scan into typed pointers so the keys are
	// comparable map keys downstream.
	rowKeyType := parentType.FieldByIndex(rowKeyField).Type
	targetKeyType := childStructType.FieldByIndex(targetKeyField).Type

	// ToSQLCtx, not ToSQL. This is the one query in the eager loader
	// that renders its own SQL instead of going through an executor,
	// because it scans two loose key columns rather than a row type —
	// and rendering is where a table's context filters do NOT appear.
	// A junction table is exactly the kind that carries them: a
	// membership row is what says which tenant an association belongs
	// to, so an unresolved junction read linked this tenant's parents
	// to every tenant's children, and the target query that follows
	// cannot notice because it is handed the keys as values.
	junctionSQL, junctionArgs, err := f.db.Select(rel.ThroughLocal, rel.ThroughForeign).
		From(rel.Through).
		Where(inPredicate(rel.ThroughLocal, rowKeys)).
		ToSQLCtx(ctx)
	if err != nil {
		return err
	}
	junctionRows, err := f.db.Query(ctx, junctionSQL, junctionArgs...)
	if err != nil {
		return err
	}
	defer junctionRows.Close()

	remoteByLocal := map[any][]any{} // local key → list of remote keys
	remoteSet := map[any]struct{}{}
	remoteKeys := []any{}
	for junctionRows.Next() {
		localPtr := reflect.New(rowKeyType)
		remotePtr := reflect.New(targetKeyType)
		if err := junctionRows.Scan(localPtr.Interface(), remotePtr.Interface()); err != nil {
			return err
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
		return err
	}

	// Empty junction — assign empty slices and bail.
	if len(remoteKeys) == 0 {
		for i := 0; i < parentSlice.Len(); i++ {
			parent := parentValue(parentSlice.Index(i), parentIsPtr)
			parent.FieldByIndex(relField).Set(reflect.MakeSlice(relFieldType, 0, 0))
		}
		return nil
	}

	// Step 2: target query.
	childSliceType := reflect.SliceOf(childStructType)
	childSlice := reflect.New(childSliceType).Elem()
	targetQuery := f.db.Select(colExprs(rel.Target)...).
		From(rel.Target).
		Where(inPredicate(rel.TargetKey, remoteKeys))
	if err := targetQuery.All(ctx, childSlice.Addr().Interface()); err != nil {
		return err
	}

	targetByKey := map[any]reflect.Value{}
	for i := 0; i < childSlice.Len(); i++ {
		ch := childSlice.Index(i)
		k := ch.FieldByIndex(targetKeyField).Interface()
		targetByKey[k] = ch
	}

	appendTarget := func(result, tv reflect.Value) reflect.Value {
		if childIsPtr {
			ptr := reflect.New(childStructType)
			ptr.Elem().Set(tv)
			return reflect.Append(result, ptr)
		}
		return reflect.Append(result, tv)
	}

	for i := 0; i < parentSlice.Len(); i++ {
		parent := parentValue(parentSlice.Index(i), parentIsPtr)
		k := parent.FieldByIndex(rowKeyField).Interface()
		remotes := remoteByLocal[k]
		result := reflect.MakeSlice(relFieldType, 0, len(remotes))
		for _, rk := range remotes {
			if tv, ok := targetByKey[rk]; ok {
				result = appendTarget(result, tv)
			}
		}
		parent.FieldByIndex(relField).Set(result)
	}
	return nil
}

// --- helpers ----------------------------------------------------------

// colExprs returns t's columns as a slice of drops.Expression, for
// SELECT projection.
func colExprs(t *Table) []drops.Expression {
	cols := t.Columns()
	out := make([]drops.Expression, len(cols))
	for i, c := range cols {
		out[i] = c
	}
	return out
}

// inPredicate renders `"tbl"."col" IN (?, ?, ...)`, binding each key.
func inPredicate(col *Column, keys []any) drops.Expression {
	return drops.ExprFunc(func(b *drops.Builder) {
		col.WriteSQL(b)
		b.WriteString(" IN (")
		for i, k := range keys {
			if i > 0 {
				b.WriteString(", ")
			}
			b.AddArg(k)
		}
		b.WriteByte(')')
	})
}

func parentValue(v reflect.Value, isPtr bool) reflect.Value {
	if isPtr {
		return v.Elem()
	}
	return v
}

// collectKeys reads fieldIdx from every row of slice.
func collectKeys(slice reflect.Value, isPtr bool, fieldIdx []int) []any {
	out := make([]any, slice.Len())
	for i := 0; i < slice.Len(); i++ {
		v := parentValue(slice.Index(i), isPtr)
		out[i] = v.FieldByIndex(fieldIdx).Interface()
	}
	return out
}

// dedupeKeys removes duplicate values, preserving first-seen order.
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

// coerceSlice converts src ([]Child) into dstType, which may be
// []Child or []*Child.
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
// to col, using the shared scanner's column→field rules.
func relationKeyField(structT reflect.Type, col *Column) ([]int, bool) {
	fm := drops.StructFields(structT)
	if idx, ok := fm[col.Name()]; ok {
		return idx, true
	}
	return nil, false
}

// relationTargetField returns the index path of the struct field that
// receives the relation. Lookup order: dropRel:"<name>" tag, then a
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
			if tag := f.Tag.Get("dropRel"); tag == name {
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
