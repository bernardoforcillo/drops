// Package app is one half of a two-package fixture: this file and its
// twin under ../right both declare a package named "app" holding a type
// named User, so reflect.Type.String prints "app.User" for both.
//
// That is the whole point of it. A context filter keyed on the short
// type name would give the two entities one key, and registering under
// a key that is taken replaces what is there — so one of the two
// guards would silently stop applying. Nothing about the call site
// would look wrong, which is why it is worth a fixture rather than a
// comment. See pg.rowScopeFilterKey.
package app

// User is a row of the shared "users" table.
type User struct {
	ID      int64  `drop:"id,primaryKey,autoIncrement"`
	OwnerID int64  `drop:"ownerId,notNull"`
	Name    string `drop:"name,notNull"`
}
