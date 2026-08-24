package mysql

import (
	"errors"
	"fmt"
	"reflect"

	"github.com/bernardoforcillo/drops"
)

// MapConstraint binds an index or constraint name to a field on T, so a
// violation of it is reported as a [drops.FieldError] naming that
// field:
//
//	var UserEntity = mysql.NewEntity[User](Users).
//	    MapConstraint("uniq_email_per_tenant", "Email")
//
// See [Entity.FieldError] for the names that need no call because they
// follow from the table declaration. It panics when T has no such
// field: a mapping that can never fire is a typo, and a package-level
// declaration is where that should surface.
func (e *Entity[T]) MapConstraint(constraint, field string) *Entity[T] {
	rt := entityStructType[T]()
	if _, ok := rt.FieldByName(field); !ok {
		panic(fmt.Sprintf("drops/mysql: MapConstraint(%q, %q): %s has no field named %s", constraint, field, rt.Name(), field))
	}
	if e.constraintFields == nil {
		e.constraintFields = make(map[string]string)
	}
	e.constraintFields[constraint] = field
	return e
}

// FieldError translates a constraint violation into a
// [drops.FieldError] naming the struct field responsible, leaving the
// original error underneath for errors.Is and errors.As. The write
// paths (Create, CreateMany, UpsertMany, Update, Save, Delete) already
// return their errors through it.
//
// Errors that are not constraint violations, and violations that
// resolve to no field, are returned unchanged.
//
// # Which field a constraint belongs to
//
// MySQL does not report a constraint the way PostgreSQL does, so
// neither does this. A duplicate key names the *index* — "Duplicate
// entry 'a@b.c' for key 'uq_users_email'" — a referential failure names
// the foreign key inside the message text, and a not-null violation
// names the column and nothing else. What is recoverable, in order:
//
//   - an explicit [Entity.MapConstraint];
//   - "PRIMARY", the name MySQL gives every primary key, when the key
//     is a single column;
//   - the UNIQUE KEY and FOREIGN KEY names drops itself emits,
//     "uq_<table>_<column>" and "fk_<table>_<column>";
//   - a bare column name, which is what MySQL calls the index behind
//     an inline UNIQUE in a table it was not drops that created;
//   - the name of a unique index declared with [NewIndex], resolving
//     to the first column it covers;
//   - the column named in a not-null message.
//
// A CHECK constraint resolves only through MapConstraint: both servers
// report the constraint's name, and neither MySQL's "<table>_chk_1"
// nor a name of your own says which column it was about.
func (e *Entity[T]) FieldError(err error) error {
	if err == nil {
		return nil
	}
	if _, ok := drops.AsFieldError(err); ok {
		return err
	}
	var se *ServerError
	if !errors.As(err, &se) {
		return err
	}
	rule, ok := constraintRule(se.Sentinel)
	if !ok {
		return err
	}
	var field, column string
	if rule == drops.RuleNotNull {
		// "Column 'email' cannot be null" — the only place the column
		// appears, since NOT NULL has no name to report.
		column = quotedName(se.Err.Error(), "Column ")
		field = e.fieldForColumn(column)
	} else {
		field, column = e.resolveConstraint(se.Constraint)
	}
	if field == "" {
		return err
	}
	return &drops.FieldError{
		Field:      field,
		Column:     column,
		Constraint: se.Constraint,
		Rule:       rule,
		Err:        err,
	}
}

// constraintRule maps a classified sentinel onto the vocabulary a form
// layer branches on. A deadlock or a lock-wait timeout has no rule and
// produces no FieldError.
func constraintRule(sentinel error) (drops.Rule, bool) {
	switch sentinel {
	case ErrUniqueViolation:
		return drops.RuleUnique, true
	case ErrForeignKeyViolation:
		return drops.RuleForeignKey, true
	case ErrCheckViolation:
		return drops.RuleCheck, true
	case ErrNotNullViolation:
		return drops.RuleNotNull, true
	}
	return "", false
}

// resolveConstraint answers which field, and which column, an index or
// constraint name belongs to.
func (e *Entity[T]) resolveConstraint(name string) (field, column string) {
	if name == "" {
		return "", ""
	}
	if f, ok := e.constraintFields[name]; ok {
		return f, e.columnForField(f)
	}
	c := e.derivedConstraintColumn(name)
	if c == nil {
		return "", ""
	}
	return e.fieldForColumn(c.Name()), c.Name()
}

// derivedConstraintColumn reads an index or constraint name back
// through the naming rules of [Entity.FieldError].
func (e *Entity[T]) derivedConstraintColumn(name string) *Column {
	t := e.table
	if pks := t.PrimaryKeyColumns(); len(pks) == 1 && name == "PRIMARY" {
		return pks[0]
	}
	for _, c := range t.Columns() {
		// constraintName is what the DDL writer used, hash-truncated
		// past MySQL's 64-byte identifier limit and all; deriving the
		// name any other way would stop matching on a long table.
		switch name {
		case constraintName("uq_", t.Name(), c.Name()),
			constraintName("fk_", t.Name(), c.Name()),
			c.Name():
			return c
		}
	}
	for _, idx := range t.Indexes() {
		if idx.name != name || !idx.unique {
			continue
		}
		if len(idx.cols) > 0 {
			return idx.cols[0].col()
		}
	}
	return nil
}

// fieldForColumn returns the name of the struct field bound to a
// column, or "" when no field is.
func (e *Entity[T]) fieldForColumn(name string) string {
	if name == "" {
		return ""
	}
	rt := entityStructType[T]()
	for _, cf := range e.colFields {
		if cf.col.Name() == name {
			return rt.FieldByIndex(cf.field).Name
		}
	}
	return ""
}

// columnForField is the inverse, for a constraint mapped explicitly to
// a field.
func (e *Entity[T]) columnForField(field string) string {
	rt := entityStructType[T]()
	for _, cf := range e.colFields {
		if rt.FieldByIndex(cf.field).Name == field {
			return cf.col.Name()
		}
	}
	return ""
}

// entityStructType returns the struct type behind T. NewEntity has
// already rejected anything that is not one.
func entityStructType[T any]() reflect.Type {
	var zero T
	rt := reflect.TypeOf(zero)
	for rt != nil && rt.Kind() == reflect.Ptr {
		rt = rt.Elem()
	}
	return rt
}
