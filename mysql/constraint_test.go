package mysql_test

import (
	"strings"
	"testing"

	"github.com/bernardoforcillo/drops/mysql"
)

type ctUser struct {
	ID    int64  `drop:"id"`
	Email string `drop:"email"`
}

// The declaration-time guard, which is the half of the feature a live
// server cannot show: a mapping naming a field that does not exist can
// never fire, and finding that out at the first failed INSERT is
// finding out in production.
func TestMapConstraintPanicsOnAFieldThatDoesNotExist(t *testing.T) {
	tbl := mysql.NewTable("ct_panic")
	mysql.Add(tbl, mysql.BigSerial("id").PrimaryKey())
	mysql.Add(tbl, mysql.Varchar("email", 191).NotNull().Unique())

	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("MapConstraint accepted a field that does not exist")
		}
		msg, _ := r.(string)
		if !strings.Contains(msg, "Emial") {
			t.Fatalf("panic = %v, want it to name the field", r)
		}
	}()
	mysql.NewEntity[ctUser](tbl).MapConstraint("uq_ct_panic_email", "Emial")
}
