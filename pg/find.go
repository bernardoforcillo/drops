package pg

import (
	"context"
	"fmt"
	"reflect"
	"strings"

	"github.com/bernardoforcillo/drops"
)

// FindBuilder composes a SELECT and (optionally) eager-loads relations
// declared via NewRelations. It mirrors a subset of SelectBuilder's API
// — Where, OrderBy, Limit, Offset — and adds With for relations.
type FindBuilder struct {
	db    *DB
	table *Table
	sel   *SelectBuilder
	with  []*Relation
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

// With marks one or more relations to eager-load. Names must match
// relations declared on the table via Relations.
func (f *FindBuilder) With(names ...string) *FindBuilder {
	for _, name := range names {
		rel := f.table.Relation(name)
		if rel == nil {
			// Defer the error until execution so the chain stays fluent.
			f.with = append(f.with, &Relation{Name: name})
			continue
		}
		f.with = append(f.with, rel)
	}
	return f
}

// All runs the find and populates dest, which must be *[]Struct or
// *[]*Struct. Each requested relation is loaded with a single follow-up
// query and stitched onto the right field of every parent.
func (f *FindBuilder) All(ctx context.Context, dest any) error {
	for _, r := range f.with {
		if r.From == nil {
			return fmt.Errorf("drops/pg: unknown relation %q on table %q", r.Name, f.table.Name())
		}
	}
	if err := f.sel.All(ctx, dest); err != nil {
		return err
	}
	if len(f.with) == 0 {
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
	for _, rel := range f.with {
		if err := f.loadRelation(ctx, rv, structType, parentIsPtr, rel); err != nil {
			return err
		}
	}
	return nil
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
// children/parent rows onto the corresponding struct field.
func (f *FindBuilder) loadRelation(
	ctx context.Context,
	parentSlice reflect.Value,
	parentType reflect.Type,
	parentIsPtr bool,
	rel *Relation,
) error {
	if rel.Kind == ManyToManyKind {
		return f.loadManyToMany(ctx, parentSlice, parentType, parentIsPtr, rel)
	}

	var rowKeyCol, targetKeyCol *Column
	switch rel.Kind {
	case HasManyKind, HasOneKind:
		rowKeyCol = rel.ParentKey
		targetKeyCol = rel.ChildKey
	case BelongsToKind:
		rowKeyCol = rel.ChildKey
		targetKeyCol = rel.ParentKey
	default:
		return fmt.Errorf("drops/pg: relation %q has unknown kind", rel.Name)
	}

	rowKeyField, ok := relationKeyField(parentType, rowKeyCol)
	if !ok {
		return fmt.Errorf("drops/pg: relation %q: parent struct missing field for column %q",
			rel.Name, rowKeyCol.Name())
	}
	relField, ok := relationTargetField(parentType, rel.Name)
	if !ok {
		return fmt.Errorf("drops/pg: relation %q: parent struct has no field tagged db_rel:%q (or matching name)",
			rel.Name, rel.Name)
	}
	relFieldType := parentType.FieldByIndex(relField).Type

	expectsSlice := rel.Kind == HasManyKind
	if expectsSlice && relFieldType.Kind() != reflect.Slice {
		return fmt.Errorf("drops/pg: relation %q is HasMany — expected slice field, got %s",
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
		return fmt.Errorf("drops/pg: relation %q: target field is neither struct nor slice of struct",
			rel.Name)
	}

	rowKeys := dedupeKeys(collectKeys(parentSlice, parentIsPtr, rowKeyField))
	if len(rowKeys) == 0 {
		return nil
	}

	childQuery := f.db.Select().From(rel.To).Where(In(targetKeyCol, rowKeys...))
	childSliceType := reflect.SliceOf(childStructType)
	childSlice := reflect.New(childSliceType).Elem()
	if err := childQuery.All(ctx, childSlice.Addr().Interface()); err != nil {
		return err
	}

	targetKeyField, ok := relationKeyField(childStructType, targetKeyCol)
	if !ok {
		return fmt.Errorf("drops/pg: relation %q: target struct %s missing field for column %q",
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
	return nil
}

// loadManyToMany loads a many-to-many relation via two queries: one
// against the junction table, one against the target table.
func (f *FindBuilder) loadManyToMany(
	ctx context.Context,
	parentSlice reflect.Value,
	parentType reflect.Type,
	parentIsPtr bool,
	rel *Relation,
) error {
	rowKeyField, ok := relationKeyField(parentType, rel.ParentKey)
	if !ok {
		return fmt.Errorf("drops/pg: relation %q: parent struct missing field for column %q",
			rel.Name, rel.ParentKey.Name())
	}
	relField, ok := relationTargetField(parentType, rel.Name)
	if !ok {
		return fmt.Errorf("drops/pg: relation %q: parent struct has no field tagged db_rel:%q (or matching name)",
			rel.Name, rel.Name)
	}
	relFieldType := parentType.FieldByIndex(relField).Type
	if relFieldType.Kind() != reflect.Slice {
		return fmt.Errorf("drops/pg: relation %q is ManyToMany — expected slice field, got %s",
			rel.Name, relFieldType.Kind())
	}
	childElemType := relFieldType.Elem()
	childIsPtr := childElemType.Kind() == reflect.Ptr
	childStructType := childElemType
	if childIsPtr {
		childStructType = childStructType.Elem()
	}
	if childStructType.Kind() != reflect.Struct {
		return fmt.Errorf("drops/pg: relation %q: slice element must be struct or *struct",
			rel.Name)
	}

	rowKeys := dedupeKeys(collectKeys(parentSlice, parentIsPtr, rowKeyField))
	if len(rowKeys) == 0 {
		return nil
	}

	// Step 1: junction query. Defer Close so every exit path frees the
	// cursor, including panics and the targetKeyField lookup below.
	junctionRows, err := f.db.Select(rel.ThroughFK1, rel.ThroughFK2).
		From(rel.Through).
		Where(In(rel.ThroughFK1, rowKeys...)).
		Rows(ctx)
	if err != nil {
		return err
	}
	defer junctionRows.Close()

	// Allocate scan targets of the same Go type as the parent/child key
	// fields, so the values are comparable as map keys downstream.
	rowKeyType := parentType.FieldByIndex(rowKeyField).Type
	targetKeyField, ok := relationKeyField(childStructType, rel.ChildKey)
	if !ok {
		return fmt.Errorf("drops/pg: relation %q: target struct %s missing field for column %q",
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
	if err := f.db.Select().From(rel.To).
		Where(In(rel.ChildKey, remoteKeys...)).
		All(ctx, childSlice.Addr().Interface()); err != nil {
		return err
	}

	targetByKey := map[any]reflect.Value{}
	for i := 0; i < childSlice.Len(); i++ {
		ch := childSlice.Index(i)
		k := ch.FieldByIndex(targetKeyField).Interface()
		targetByKey[k] = ch
	}

	// Stitch: each parent gets a slice ordered to match its junction rows.
	for i := 0; i < parentSlice.Len(); i++ {
		parent := parentValue(parentSlice.Index(i), parentIsPtr)
		k := parent.FieldByIndex(rowKeyField).Interface()
		target := parent.FieldByIndex(relField)
		remotes := remoteByLocal[k]
		result := reflect.MakeSlice(relFieldType, 0, len(remotes))
		for _, rk := range remotes {
			tv, ok := targetByKey[rk]
			if !ok {
				continue
			}
			if childIsPtr {
				ptr := reflect.New(childStructType)
				ptr.Elem().Set(tv)
				result = reflect.Append(result, ptr)
			} else {
				result = reflect.Append(result, tv)
			}
		}
		target.Set(result)
	}
	return nil
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
