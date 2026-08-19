package mysql_test

import (
	"fmt"

	"github.com/bernardoforcillo/drops"
	"github.com/bernardoforcillo/drops/mysql"
)

// A schema declared once, in MySQL terms.
var (
	Posts       = mysql.NewTable("posts")
	PostID      = mysql.Add(Posts, mysql.BigSerial("id").PrimaryKey())
	PostTitle   = mysql.Add(Posts, mysql.Varchar("title", 255).NotNull())
	PostSlug    = mysql.Add(Posts, mysql.Varchar("slug", 191).NotNull().Unique())
	PostViews   = mysql.Add(Posts, mysql.Integer("views").NotNull().Default("0"))
	PostUpdated = mysql.Add(Posts, mysql.Timestamp("updatedAt", false).
			NotNull().
			Default("CURRENT_TIMESTAMP(6)").
			OnUpdateExpr("CURRENT_TIMESTAMP(6)"))
)

func ExampleCreateTable() {
	Posts.Engine("InnoDB").Charset("utf8mb4")
	sql, _ := drops.StringWithDialect(mysql.Dialect, mysql.CreateTable(Posts))
	fmt.Println(sql)
	// Output:
	// CREATE TABLE `posts` (
	// 	`id` BIGINT NOT NULL AUTO_INCREMENT,
	// 	`title` VARCHAR(255) NOT NULL,
	// 	`slug` VARCHAR(191) NOT NULL,
	// 	`views` INT NOT NULL DEFAULT 0,
	// 	`updatedAt` DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6) ON UPDATE CURRENT_TIMESTAMP(6),
	// 	PRIMARY KEY (`id`),
	// 	UNIQUE KEY `uq_posts_slug` (`slug`)
	// ) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4
}

// Comparisons are checked against the column's Go type, so a mistyped
// one does not compile.
func ExampleSelectBuilder() {
	db := mysql.New(&fakeDriver{})
	sql, args := db.Select(PostID, PostTitle).
		From(Posts).
		Where(PostViews.Gte(100)).
		OrderBy(PostViews.Desc()).
		Limit(10).
		ToSQL()
	fmt.Println(sql)
	fmt.Println(args...)
	// Output:
	// SELECT `posts`.`id`, `posts`.`title` FROM `posts` WHERE (`posts`.`views` >= ?) ORDER BY `posts`.`views` DESC LIMIT ?
	// 100 10
}

// MySQL's upsert has no conflict target: it fires on a collision with
// any unique index, here the primary key or the slug.
func ExampleInsertBuilder_OnDuplicateKeyUpdate() {
	db := mysql.New(&fakeDriver{})
	sql, _ := db.Insert(Posts).
		Row(PostTitle.Val("Hello"), PostSlug.Val("hello"), PostViews.Val(0)).
		OnDuplicateKeyUpdate(PostTitle.Expr(mysql.NewValueOf(PostTitle))).
		ToSQL()
	fmt.Println(sql)
	// Output:
	// INSERT INTO `posts` (`title`, `slug`, `views`) VALUES (?, ?, ?) ON DUPLICATE KEY UPDATE `title` = VALUES(`title`)
}

// ORDER BY and LIMIT on a DELETE are a MySQL extension, and the safe
// way to clear a large backlog without one enormous transaction.
func ExampleDeleteBuilder_Limit() {
	db := mysql.New(&fakeDriver{})
	sql, _ := db.Delete(Posts).Where(PostViews.Lt(1)).OrderBy(PostID.Asc()).Limit(1000).ToSQL()
	fmt.Println(sql)
	// Output:
	// DELETE FROM `posts` WHERE (`posts`.`views` < ?) ORDER BY `posts`.`id` ASC LIMIT ?
}
