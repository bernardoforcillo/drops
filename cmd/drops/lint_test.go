package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/bernardoforcillo/drops/cmd/drops/dropslint"
)

// A typo in a CI -off list that quietly turns nothing off is worse
// than a failed build: the rule the author meant to disable keeps
// firing, or — the case that matters — the rule they meant to keep
// silently stops. So an unrecognised name is a usage error, and the
// message names the offender.
func TestLintOffRejectsAnUnknownRule(t *testing.T) {
	_, err := enabled("unfilteredwrite,unboundedred")
	if err == nil {
		t.Fatal("a misspelled rule was accepted")
	}
	var ue usageError
	if !errors.As(err, &ue) {
		t.Errorf("error is %T, want a usageError so the CLI prints usage", err)
	}
	if !strings.Contains(err.Error(), "unboundedred") {
		t.Errorf("the message does not name the offender: %v", err)
	}
}

// -off narrows the set; it does not reorder or duplicate it.
func TestLintOffKeepsTheRest(t *testing.T) {
	got, err := enabled(" loopload , ")
	if err != nil {
		t.Fatalf("enabled: %v", err)
	}
	var names []string
	for _, a := range got {
		names = append(names, a.Name)
	}
	if want := "unfilteredwrite unboundedread"; strings.Join(names, " ") != want {
		t.Errorf("enabled = %v, want %s", names, want)
	}
	if all := len(dropslint.All()); len(got) != all-1 {
		t.Errorf("enabled returned %d rules, want %d", len(got), all-1)
	}
}

// Turning every rule off is a command that would report "clean" over
// code nothing looked at, which is the same lie as analysing a package
// that did not compile.
func TestLintOffCannotEmptyTheSet(t *testing.T) {
	var names []string
	for _, a := range dropslint.All() {
		names = append(names, a.Name)
	}
	if _, err := enabled(strings.Join(names, ",")); err == nil {
		t.Fatal("-off accepted every rule and would have reported a clean run")
	}
}

// An empty -off is the default and turns nothing off.
func TestLintOffEmptyKeepsEverything(t *testing.T) {
	got, err := enabled("")
	if err != nil {
		t.Fatalf("enabled: %v", err)
	}
	if len(got) != len(dropslint.All()) {
		t.Errorf("enabled(\"\") returned %d rules, want %d", len(got), len(dropslint.All()))
	}
}
