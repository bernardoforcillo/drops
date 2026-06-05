package models

//drops:entity table=Users
type User struct {
	ID    int64  `drop:"id"`
	Name  string `drop:"name"`
	Email string `drop:"email"`
}

//drops:entity table=Posts
type Post struct {
	ID     int64  `drop:"id"`
	UserID int64  `drop:"user_id"`
	Title  string `drop:"title"`
}

// NotAnEntity is here to verify the generator skips types without the
// directive.
type NotAnEntity struct {
	X int
}
