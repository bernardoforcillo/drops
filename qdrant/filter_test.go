package qdrant_test

import (
	"encoding/json"
	"testing"

	"github.com/bernardoforcillo/drops/qdrant"
)

// An empty operand list is what a lookup that found nothing compiles
// to, and it reaches DeleteByFilter by the shortest route there is.
// Every one of these used to render a condition that constrains
// nothing, which Qdrant reads as one that matches every point.
func TestEmptyOperandListsDoNotMatchEverything(t *testing.T) {
	nothing := `{"must_not":[{}]}`
	everything := `{}`

	cases := []struct {
		name string
		cond qdrant.Condition
		want string
	}{
		{"HasID with no ids", qdrant.HasID(), nothing},
		{"HasID over an empty slice", qdrant.HasID([]any{}...), nothing},
		{"In with no values", qdrant.In("lang"), nothing},
		{"NotIn with no values", qdrant.NotIn("lang"), everything},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			b, err := json.Marshal(c.cond)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if string(b) != c.want {
				t.Fatalf("rendered %s, want %s", b, c.want)
			}
		})
	}
}

// The whole point of the case above: the delete request Qdrant
// receives must not be one that empties the collection.
func TestDeleteByAnEmptyIDSetDeletesNothing(t *testing.T) {
	b, err := json.Marshal(qdrant.Must(qdrant.HasID()))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	const want = `{"must":[{"must_not":[{}]}]}`
	if string(b) != want {
		t.Fatalf("delete filter rendered %s, want %s", b, want)
	}
}
