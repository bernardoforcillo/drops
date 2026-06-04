package models

//drops:entity table=Users
type User struct {
	ID    int64  `db:"id"`
	Name  string `db:"name"`
	Email string `db:"email"`
}

//drops:entity table=Posts
type Post struct {
	ID     int64  `db:"id"`
	UserID int64  `db:"user_id"`
	Title  string `db:"title"`
}

// NotAnEntity is here to verify the generator skips types without the
// directive.
type NotAnEntity struct {
	X int
}
