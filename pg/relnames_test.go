package pg

import (
	"reflect"
	"testing"
)

// A tool that generates code from a schema has to enumerate what a
// table declares, and Relation/Rel only answer about a name it
// already knows.
func TestRelationNamesListsWhatIsDeclaredInOrder(t *testing.T) {
	parents := NewTable("relnames_parents")
	parentID := Add(parents, BigSerial("id").PrimaryKey())
	children := NewTable("relnames_children")
	childParent := Add(children, BigInt("parentId").NotNull())

	if got := parents.RelationNames(); len(got) != 0 {
		t.Fatalf("a table with no relations lists %v", got)
	}

	NewRelations(parents).
		HasMany("children", children, parentID, childParent).
		HasOne("primaryChild", children, parentID, childParent)
	NewRelations(children).
		BelongsTo("parent", parents, childParent, parentID)

	// Sorted, not map order: a generator that emitted them in the
	// order Go happened to hash them would produce a different file
	// on every run.
	if got, want := parents.RelationNames(), []string{"children", "primaryChild"}; !reflect.DeepEqual(got, want) {
		t.Errorf("parents.RelationNames() = %v, want %v", got, want)
	}
	if got, want := children.RelationNames(), []string{"parent"}; !reflect.DeepEqual(got, want) {
		t.Errorf("children.RelationNames() = %v, want %v", got, want)
	}
}
