package pg

import (
	"errors"
	"fmt"
	"reflect"

	"github.com/bernardoforcillo/drops"
)

// MapConstraint binds a database constraint name to a field on T, so a
// violation of it is reported as a [drops.FieldError] naming that
// field:
//
//	var UserEntity = pg.NewEntity[User](Users).
//	    MapConstraint("users_lower_email_idx", "Email")
//
// It is Ecto's unique_constraint, and it exists for the same reason:
// uniqueness cannot be established in application code without a race,
// so the database has to be the authority — and the only thing the
// database answers with is a constraint name.
//
// Most constraints need no call at all. PostgreSQL names what it
// creates, drops knows the same rules, and the derived defaults are
// described on [Entity.FieldError]. Reach for MapConstraint when the
// name does not follow from the table declaration: a unique index
// created by hand, a partial or expression index, a CHECK covering
// several columns, a constraint inherited from a schema drops did not
// write.
//
// It panics when T has no such field — a mapping that can never fire is
// a typo, and package-level declarations are the right place to find
// out. Calls are chainable and the last one for a given constraint
// wins; an explicit mapping always beats the derived default.
func (e *Entity[T]) MapConstraint(constraint, field string) *Entity[T] {
	rt := entityStructType[T]()
	if _, ok := rt.FieldByName(field); !ok {
		panic(fmt.Sprintf("drops/pg: MapConstraint(%q, %q): %s has no field named %s", constraint, field, rt.Name(), field))
	}
	if e.constraintFields == nil {
		e.constraintFields = make(map[string]string)
	}
	e.constraintFields[constraint] = field
	return e
}

// FieldError translates a constraint violation into a
// [drops.FieldError] naming the struct field responsible, leaving the
// original error underneath for errors.Is and errors.As.
//
// The write paths (Create, Update, Save, Delete, CreateMany,
// UpsertMany) already return their errors through it, so a caller
// normally only reads the result:
//
//	if err := UserEntity.Create(db, ctx, &u); err != nil {
//	    if fe, ok := drops.AsFieldError(err); ok && fe.Rule == drops.RuleUnique {
//	        return form.Reject(fe.Field, "is already taken")
//	    }
//	    return err
//	}
//
// It is exported for the queries that do not go through an Entity —
// a hand-built INSERT ... ON CONFLICT, a bulk COPY — where the same
// entity can still say which field a constraint belongs to.
//
// Errors that are not constraint violations, and violations of a
// constraint that resolves to no field, are returned unchanged: a
// FieldError that cannot name a field would be worse than the driver
// error it replaced.
//
// # Which field a constraint belongs to
//
// An explicit [Entity.MapConstraint] wins. Otherwise the name is read
// back through the conventions PostgreSQL itself uses when it names a
// constraint the table declaration did not:
//
//   - "<table>_pkey" — the primary key, when it is a single column;
//   - "<table>_<column>_key" — a UNIQUE column;
//   - "<table>_<column>_fkey" — a REFERENCES column;
//   - "<table>_<column>_check" — a column CHECK;
//   - the name given to AddUnique or to a unique AddIndex, which
//     resolves to the first column it covers.
//
// A not-null violation carries no constraint name at all — NOT NULL is
// a column property and PostgreSQL names the column instead — so it
// resolves through the column directly.
func (e *Entity[T]) FieldError(err error) error {
	if err == nil {
		return nil
	}
	// Already translated: Save routes through Create or Update, and
	// wrapping a FieldError in a second one would bury the first.
	if _, ok := drops.AsFieldError(err); ok {
		return err
	}
	var pe *PgError
	if !errors.As(err, &pe) {
		return err
	}
	rule, ok := constraintRule(pe.Sentinel)
	if !ok {
		return err
	}
	var field, column string
	if rule == drops.RuleNotNull {
		column = pe.Column
		field = e.fieldForColumn(column)
	} else {
		field, column = e.resolveConstraint(pe.Constraint)
	}
	if field == "" {
		return err
	}
	return &drops.FieldError{
		Field:      field,
		Column:     column,
		Constraint: pe.Constraint,
		Rule:       rule,
		Err:        err,
	}
}

// constraintRule maps a classified sentinel onto the vocabulary a form
// layer branches on. Failures that are not a constraint refusing a
// value — a deadlock, an undefined table — have no rule and produce no
// FieldError.
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

// resolveConstraint answers which field, and which column, a constraint
// name belongs to. Both are empty when nothing on the table claims it.
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

// derivedConstraintColumn reads a constraint name back through
// PostgreSQL's naming rules — see [Entity.FieldError] for the list and
// the reasoning.
func (e *Entity[T]) derivedConstraintColumn(name string) *Column {
	t := e.table
	if pks := t.primaryKeyColumns(); len(pks) == 1 && name == t.Name()+"_pkey" {
		return pks[0]
	}
	for _, c := range t.Columns() {
		stem := t.Name() + "_" + c.Name()
		switch name {
		case stem + "_key", stem + "_fkey", stem + "_check":
			return c
		}
	}
	// A constraint declared with a name of its own keeps it. Where it
	// covers several columns the first is the one a form should point
	// at: it is the column the declaration leads with, and reporting
	// against the whole set would give the caller nowhere to put the
	// message.
	if cols, ok := t.CompositeUniques()[name]; ok && len(cols) > 0 {
		return cols[0]
	}
	for _, idx := range t.Indexes() {
		if idx.name != name || !idx.unique {
			continue
		}
		for _, ex := range idx.columns {
			if ref, ok := ex.(ColRef); ok {
				return ref.col()
			}
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
// a field: the column is a detail the form layer may want, but a
// mapping is allowed to name a field that has no column of its own.
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
