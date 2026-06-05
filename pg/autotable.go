package pg

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"time"
)

// AutoTable derives a Table from the `drop` struct tags on T. Tag
// syntax:
//
//	drop:"<col_name>[,opt[,opt[,...]]]"
//
// where each opt is one of:
//
//	pk                   — PRIMARY KEY
//	autoinc              — use the serial family (BigSerial / Serial /
//	                       SmallSerial) for the column's type
//	notnull              — NOT NULL
//	unique               — UNIQUE
//	default=<sql>        — raw DEFAULT clause (no parameterisation)
//	version              — mark as the optimistic-lock version column
//
// Use `drop:"-"` to skip a field entirely. Untagged exported fields are
// also skipped.
//
// Go type ↔ ColumnType mapping mirrors the manual constructors:
//
//	bool                  → boolean
//	int16                 → smallint   (smallserial if autoinc)
//	int32 / int           → integer    (serial      if autoinc)
//	int64                 → bigint     (bigserial   if autoinc)
//	float32               → real
//	float64               → double precision
//	string                → text
//	[]byte                → bytea
//	time.Time             → timestamptz
//	json.RawMessage       → jsonb
//	*T                    → same as T, column is nullable unless
//	                        `notnull` is set explicitly
//
// Custom types — uuid, jsonb columns backed by app structs, etc. —
// fall back to drops.Custom; declare them by hand instead.
func AutoTable[T any](name string) *Table {
	tbl := NewTable(name)
	populateTable[T](tbl)
	return tbl
}

// AutoSchemaTable is the schema-qualified twin of AutoTable.
func AutoSchemaTable[T any](schema, name string) *Table {
	tbl := NewSchemaTable(schema, name)
	populateTable[T](tbl)
	return tbl
}

// NewAutoEntity bundles AutoTable + NewEntity into one call —
// typically the only line a small entity needs.
//
//	var UserEntity = pg.NewAutoEntity[User]("users")
func NewAutoEntity[T any](name string) *Entity[T] {
	return NewEntity[T](AutoTable[T](name))
}

// populateTable inspects T and registers a column for every db-tagged
// exported field. Panics on misconfiguration because schema
// declarations live at process startup.
func populateTable[T any](tbl *Table) {
	var zero T
	rt := reflect.TypeOf(zero)
	for rt.Kind() == reflect.Ptr {
		rt = rt.Elem()
	}
	if rt.Kind() != reflect.Struct {
		panic(fmt.Sprintf("drops/pg: AutoTable requires T to be a struct, got %s", rt.Kind()))
	}
	walkAutoFields(rt, func(f reflect.StructField, opts autoOpts) {
		col := makeAutoColumn(rt.Name(), f, opts)
		tbl.add(col)
	})
}

// autoOpts is the parsed metadata from a single `drop:` tag.
type autoOpts struct {
	Name    string
	PK      bool
	AutoInc bool
	NotNull bool
	Unique  bool
	Default string
	Version bool
}

// walkAutoFields applies fn to every exported drop-tagged field on rt,
// in declaration order. Anonymous embedded structs are walked into.
func walkAutoFields(rt reflect.Type, fn func(reflect.StructField, autoOpts)) {
	for i := 0; i < rt.NumField(); i++ {
		f := rt.Field(i)
		if !f.IsExported() {
			continue
		}
		if f.Anonymous && f.Type.Kind() == reflect.Struct {
			walkAutoFields(f.Type, fn)
			continue
		}
		raw, ok := f.Tag.Lookup("drop")
		if !ok || raw == "-" {
			continue
		}
		opts, err := parseAutoTag(raw)
		if err != nil {
			panic(fmt.Sprintf("drops/pg: AutoTable %s.%s: %v", rt.Name(), f.Name, err))
		}
		fn(f, opts)
	}
}

// parseAutoTag turns a `drop:"name,opt,opt=val"` string into autoOpts.
func parseAutoTag(raw string) (autoOpts, error) {
	parts := strings.Split(raw, ",")
	if len(parts) == 0 || parts[0] == "" {
		return autoOpts{}, fmt.Errorf("empty drop tag")
	}
	opts := autoOpts{Name: strings.TrimSpace(parts[0])}
	for _, p := range parts[1:] {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		k, v, hasVal := strings.Cut(p, "=")
		switch k {
		case "pk":
			opts.PK = true
		case "autoinc":
			opts.AutoInc = true
		case "notnull":
			opts.NotNull = true
		case "unique":
			opts.Unique = true
		case "version":
			opts.Version = true
		case "default":
			if !hasVal {
				return opts, fmt.Errorf("`default` option requires a value: default=<sql>")
			}
			opts.Default = v
		default:
			return opts, fmt.Errorf("unknown drop tag option %q", k)
		}
	}
	return opts, nil
}

// makeAutoColumn assembles a *Column from a parsed field. Pointer
// types make the column nullable by default unless `notnull` is set.
func makeAutoColumn(structName string, f reflect.StructField, opts autoOpts) *Column {
	ft := f.Type
	for ft.Kind() == reflect.Ptr {
		ft = ft.Elem()
	}
	ct := autoColumnType(structName, f.Name, ft, opts.AutoInc)
	c := &Column{name: opts.Name, typ: ct}
	if opts.PK {
		c.primary = true
		c.notNull = true
	}
	if opts.NotNull {
		c.notNull = true
	}
	if opts.Unique {
		c.unique = true
	}
	if opts.Default != "" {
		c.hasDefault = true
		c.defaultSQL = opts.Default
	}
	if opts.Version {
		c.version = true
	}
	return c
}

// autoColumnType maps a Go type to a ColumnType. autoinc upgrades
// integer types to the serial family.
func autoColumnType(structName, fieldName string, ft reflect.Type, autoinc bool) ColumnType {
	switch ft.Kind() {
	case reflect.Bool:
		return simpleType("boolean")
	case reflect.Int16:
		if autoinc {
			return simpleType("smallserial")
		}
		return simpleType("smallint")
	case reflect.Int32, reflect.Int:
		if autoinc {
			return simpleType("serial")
		}
		return simpleType("integer")
	case reflect.Int64:
		if autoinc {
			return simpleType("bigserial")
		}
		return simpleType("bigint")
	case reflect.Float32:
		return simpleType("real")
	case reflect.Float64:
		return simpleType("double precision")
	case reflect.String:
		return simpleType("text")
	}
	// Named / composite types.
	switch ft {
	case reflect.TypeOf(time.Time{}):
		return simpleType("timestamptz")
	case reflect.TypeOf(json.RawMessage{}):
		return simpleType("jsonb")
	case reflect.TypeOf([]byte{}):
		return simpleType("bytea")
	}
	panic(fmt.Sprintf("drops/pg: AutoTable %s.%s: unsupported field type %s — declare the column by hand",
		structName, fieldName, ft))
}
