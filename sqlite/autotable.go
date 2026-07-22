package sqlite

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"time"
)

// moneyTypeName / moneyPkgPath identify sqlite.Money so AutoTable maps
// it to INTEGER (minor units) regardless of the field's declared type.
const moneyTypeName = "Money"

var moneyPkgPath = reflect.TypeOf(Money{}).PkgPath()

// AutoTable derives a Table from the `drop` struct tags on T. Tag syntax:
//
//	drop:"<col_name>[,opt[,opt...]]"
//
// where each opt is one of: primaryKey, autoIncrement, notNull, unique,
// pii, default=<sql>. Use `drop:"-"` (or an untagged field) to skip.
//
// Go type → SQLite affinity:
//
//	bool                  → BOOLEAN (INTEGER 0/1)
//	int16/int32/int/int64 → INTEGER
//	float32/float64       → REAL
//	string                → TEXT
//	[]byte                → BLOB
//	time.Time             → DATETIME
//	json.RawMessage       → TEXT
//	sqlite.Money          → INTEGER
//	*T                    → same as T (nullable unless notNull is set)
//
// SQLite has no serial family — autoIncrement simply keeps INTEGER; pair
// it with primaryKey to get an auto-assigning rowid alias.
func AutoTable[T any](name string) *Table {
	tbl := NewTable(name)
	populateTable[T](tbl)
	return tbl
}

// NewAutoEntity bundles AutoTable + NewEntity into one call.
func NewAutoEntity[T any](name string) *Entity[T] {
	return NewEntity[T](AutoTable[T](name))
}

func populateTable[T any](tbl *Table) {
	var zero T
	rt := reflect.TypeOf(zero)
	for rt.Kind() == reflect.Ptr {
		rt = rt.Elem()
	}
	if rt.Kind() != reflect.Struct {
		panic(fmt.Sprintf("drops/sqlite: AutoTable requires T to be a struct, got %s", rt.Kind()))
	}
	walkAutoFields(rt, func(f reflect.StructField, opts autoOpts) {
		tbl.add(makeAutoColumn(rt.Name(), f, opts))
	})
}

type autoOpts struct {
	Name    string
	PK      bool
	AutoInc bool
	NotNull bool
	Unique  bool
	Default string
	PII     bool
}

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
			panic(fmt.Sprintf("drops/sqlite: AutoTable %s.%s: %v", rt.Name(), f.Name, err))
		}
		fn(f, opts)
	}
}

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
		case "primaryKey":
			opts.PK = true
		case "autoIncrement":
			opts.AutoInc = true
		case "notNull":
			opts.NotNull = true
		case "unique":
			opts.Unique = true
		case "pii":
			opts.PII = true
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

func makeAutoColumn(structName string, f reflect.StructField, opts autoOpts) *Column {
	ft := f.Type
	for ft.Kind() == reflect.Ptr {
		ft = ft.Elem()
	}
	if isMoneyType(ft) {
		c := &Column{name: opts.Name, typ: simpleType("INTEGER")}
		applyAutoOpts(c, opts)
		return c
	}
	c := &Column{name: opts.Name, typ: autoColumnType(structName, f.Name, ft)}
	applyAutoOpts(c, opts)
	return c
}

func applyAutoOpts(c *Column, opts autoOpts) {
	if opts.PK {
		c.primary = true
		c.notNull = true
	}
	if opts.NotNull {
		c.notNull = true
	}
	if opts.AutoInc {
		c.autoInc = true
	}
	if opts.Unique {
		c.unique = true
	}
	if opts.Default != "" {
		c.hasDefault = true
		c.defaultSQL = opts.Default
	}
	if opts.PII {
		c.pii = true
	}
}

func isMoneyType(ft reflect.Type) bool {
	return ft.Kind() == reflect.Struct && ft.Name() == moneyTypeName && ft.PkgPath() == moneyPkgPath
}

func autoColumnType(structName, fieldName string, ft reflect.Type) ColumnType {
	switch ft.Kind() {
	case reflect.Bool:
		return simpleType("BOOLEAN")
	case reflect.Int16, reflect.Int32, reflect.Int, reflect.Int64,
		reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uint:
		return simpleType("INTEGER")
	case reflect.Float32, reflect.Float64:
		return simpleType("REAL")
	case reflect.String:
		return simpleType("TEXT")
	}
	switch ft {
	case reflect.TypeOf(time.Time{}):
		return simpleType("DATETIME")
	case reflect.TypeOf(json.RawMessage{}):
		return simpleType("TEXT")
	case reflect.TypeOf([]byte{}):
		return simpleType("BLOB")
	}
	panic(fmt.Sprintf("drops/sqlite: AutoTable %s.%s: unsupported field type %s — declare the column by hand",
		structName, fieldName, ft))
}
