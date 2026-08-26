package pg_test

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/bernardoforcillo/drops"
	"github.com/bernardoforcillo/drops/dropstest"
	"github.com/bernardoforcillo/drops/pg"
)

// RelConfig.Limit used to be accepted and discarded on a many-to-many
// edge. The call site is identical to the HasMany edge beside it, the
// result set was not, and nothing between the two said so — which is
// the exact failure this phase deleted drops/mysql's relation API over.
// These assert on the loaded slices rather than on SQL, because the cap
// on this one edge is not in the statement: see RelConfig.Limit.
func m2mLimitSchema() *pg.Table {
	users := pg.NewTable("users")
	userID := pg.Add(users, pg.BigSerial("id").PrimaryKey())
	pg.Add(users, pg.Text("name").NotNull())

	groups := pg.NewTable("groups")
	groupID := pg.Add(groups, pg.BigSerial("id").PrimaryKey())
	pg.Add(groups, pg.Text("name").NotNull())

	userGroups := pg.NewTable("userGroups")
	ugUser := pg.Add(userGroups, pg.BigInt("userId").NotNull())
	ugGroup := pg.Add(userGroups, pg.BigInt("groupId").NotNull())

	pg.NewRelations(users).
		ManyToMany("groups", groups, userGroups, ugUser, ugGroup, userID, groupID)
	return users
}

// m2mLimitDriver answers the three queries the junction loader issues.
// groupRows is the order the target query comes back in, which is what
// an OrderBy on the edge would have produced on a real server.
func m2mLimitDriver(groupRows [][]any) *dropstest.Driver {
	return dropstest.New().Answer(func(sql string, _ []any) ([]string, [][]any, bool) {
		switch {
		case strings.Contains(sql, `FROM "users"`):
			return []string{"id", "name"}, [][]any{
				{int64(1), "Alice"},
				{int64(2), "Bob"},
			}, true
		case strings.Contains(sql, `FROM "userGroups"`):
			return []string{"userId", "groupId"}, [][]any{
				{int64(1), int64(10)},
				{int64(1), int64(11)},
				{int64(1), int64(12)},
				{int64(1), int64(13)},
				{int64(2), int64(11)},
			}, true
		case strings.Contains(sql, `FROM "groups"`):
			return []string{"id", "name"}, groupRows, true
		}
		return nil, nil, false
	})
}

func TestManyToManyLimitCapsEachParent(t *testing.T) {
	junctionOrder := [][]any{
		{int64(10), "Admins"},
		{int64(11), "Editors"},
		{int64(12), "Guests"},
		{int64(13), "Owners"},
	}
	// What an OrderBy(name DESC) on the edge makes the target query
	// return: the cap has to follow that order, not the junction's.
	descending := [][]any{
		{int64(13), "Owners"},
		{int64(12), "Guests"},
		{int64(11), "Editors"},
		{int64(10), "Admins"},
	}

	tests := []struct {
		name      string
		groupRows [][]any
		configure func(*pg.RelConfig)
		wantAlice []string
		wantBob   []string
	}{
		{
			name:      "no limit loads every linked row",
			groupRows: junctionOrder,
			configure: func(c *pg.RelConfig) {},
			wantAlice: []string{"Admins", "Editors", "Guests", "Owners"},
			wantBob:   []string{"Editors"},
		},
		{
			name:      "limit caps per parent, not globally",
			groupRows: junctionOrder,
			configure: func(c *pg.RelConfig) { c.Limit(2) },
			wantAlice: []string{"Admins", "Editors"},
			wantBob:   []string{"Editors"},
		},
		{
			name:      "offset skips within each parent",
			groupRows: junctionOrder,
			configure: func(c *pg.RelConfig) { c.Limit(2).Offset(1) },
			wantAlice: []string{"Editors", "Guests"},
			wantBob:   nil,
		},
		{
			name:      "an offset past the end leaves the parent empty",
			groupRows: junctionOrder,
			configure: func(c *pg.RelConfig) { c.Limit(2).Offset(9) },
			wantAlice: nil,
			wantBob:   nil,
		},
		{
			name:      "offset without a limit is ignored, as on the SQL path",
			groupRows: junctionOrder,
			configure: func(c *pg.RelConfig) { c.Offset(2) },
			wantAlice: []string{"Admins", "Editors", "Guests", "Owners"},
			wantBob:   []string{"Editors"},
		},
		{
			name:      "the cap follows the edge's own ordering",
			groupRows: descending,
			configure: func(c *pg.RelConfig) {
				c.OrderBy(drops.Raw(`"name" DESC`)).Limit(2)
			},
			wantAlice: []string{"Owners", "Guests"},
			wantBob:   []string{"Editors"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			users := m2mLimitSchema()
			db := pg.New(m2mLimitDriver(tt.groupRows))

			var got []m2mUser
			if err := db.Find(users).
				WithRel("groups", tt.configure).
				All(context.Background(), &got); err != nil {
				t.Fatalf("All: %v", err)
			}
			if len(got) != 2 {
				t.Fatalf("loaded %d users, want 2", len(got))
			}
			if names := groupNames(got[0]); !reflect.DeepEqual(names, tt.wantAlice) {
				t.Errorf("Alice's groups: got = %v, want %v", names, tt.wantAlice)
			}
			if names := groupNames(got[1]); !reflect.DeepEqual(names, tt.wantBob) {
				t.Errorf("Bob's groups: got = %v, want %v", names, tt.wantBob)
			}
		})
	}
}

func groupNames(u m2mUser) []string {
	var out []string
	for _, g := range u.Groups {
		out = append(out, g.Name)
	}
	return out
}
