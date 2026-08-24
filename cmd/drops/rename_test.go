package main

import (
	"bufio"
	"bytes"
	"flag"
	"io"
	"reflect"
	"strings"
	"testing"
)

func TestRenameAnswersFromFlags(t *testing.T) {
	a := &renameAnswers{}
	fs := flag.NewFlagSet("generate", flag.ContinueOnError)
	a.register(fs)
	if err := fs.Parse([]string{
		"--rename-column", "users.email=emailAddress",
		"--rename-table", "users=people",
		"--drop-column", "posts.draft",
		"--drop-table", "audit",
	}); err != nil {
		t.Fatal(err)
	}
	got, err := a.decisions()
	if err != nil {
		t.Fatal(err)
	}
	want := []bridgeRename{
		{Kind: "column", Table: "users", From: "email", To: "emailAddress", IsRename: true},
		{Kind: "table", From: "users", To: "people", IsRename: true},
		{Kind: "column", Table: "posts", From: "draft"},
		{Kind: "table", From: "audit"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("decisions:\n got %+v\nwant %+v", got, want)
	}
}

// A misspelled answer is the command line being wrong, and saying so
// beats sending a decision the generator will not match to anything and
// reporting the original ambiguity a second time.
func TestRenameAnswersRejectMalformedSpecs(t *testing.T) {
	for _, spec := range []struct{ flag, value string }{
		{"--rename-column", "users.email"},        // no new name
		{"--rename-column", "email=emailAddress"}, // no table
		{"--rename-table", "users"},               // no new name
		{"--drop-column", "email"},                // no table
	} {
		a := &renameAnswers{}
		fs := flag.NewFlagSet("generate", flag.ContinueOnError)
		fs.SetOutput(new(bytes.Buffer))
		a.register(fs)
		if err := fs.Parse([]string{spec.flag, spec.value}); err != nil {
			t.Fatal(err)
		}
		if _, err := a.decisions(); err == nil {
			t.Errorf("%s %s was accepted", spec.flag, spec.value)
		}
	}
}

func TestPromptRenamesRecordsAYes(t *testing.T) {
	out := new(bytes.Buffer)
	got, err := promptRenames(bufio.NewReader(strings.NewReader("y\n")), out, []bridgeCandidate{
		{Kind: "column", Table: "users", From: "email", To: "emailAddress"},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []bridgeRename{{Kind: "column", Table: "users", From: "email", To: "emailAddress", IsRename: true}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("answers: %+v", got)
	}
	if !strings.Contains(out.String(), "is users.emailAddress a rename of users.email?") {
		t.Errorf("prompt: %q", out.String())
	}
}

// Anything that is not a yes is a no, and the no is recorded against
// the old name alone so it covers every pair the diff offered for it.
func TestPromptRenamesTreatsAnythingElseAsNo(t *testing.T) {
	got, err := promptRenames(bufio.NewReader(strings.NewReader("\n")), new(bytes.Buffer), []bridgeCandidate{
		{Kind: "column", Table: "users", From: "email", To: "emailAddress"},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []bridgeRename{{Kind: "column", Table: "users", From: "email"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("answers: %+v", got)
	}
}

// One yes settles both names, so the second pair the diff offered for
// the same column is never put to anybody.
func TestPromptRenamesStopsAskingAboutAnAnsweredColumn(t *testing.T) {
	out := new(bytes.Buffer)
	got, err := promptRenames(bufio.NewReader(strings.NewReader("y\n")), out, []bridgeCandidate{
		{Kind: "column", Table: "users", From: "email", To: "emailAddress"},
		{Kind: "column", Table: "users", From: "email", To: "contact"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].To != "emailAddress" {
		t.Errorf("answers: %+v", got)
	}
	if strings.Contains(out.String(), "contact") {
		t.Errorf("asked about a column already answered:\n%s", out.String())
	}
}

// Two rounds of questions read from one stdin. A reader built per round
// buffers the whole of stdin on the first read and drops what it did not
// use, so the second round found EOF where the second answer was.
func TestPromptRenamesReadsASecondRoundFromTheSameStdin(t *testing.T) {
	in := bufio.NewReader(strings.NewReader("y\ny\n"))
	first, err := promptRenames(in, io.Discard, []bridgeCandidate{
		{Kind: "table", From: "users", To: "people"},
	})
	if err != nil {
		t.Fatalf("first round: %v", err)
	}
	if len(first) != 1 || !first[0].IsRename {
		t.Fatalf("first round: %+v", first)
	}
	second, err := promptRenames(in, io.Discard, []bridgeCandidate{
		{Kind: "column", Table: "people", From: "email", To: "emailAddress"},
	})
	if err != nil {
		t.Fatalf("the second answer was read away by the first round: %v", err)
	}
	if len(second) != 1 || !second[0].IsRename {
		t.Fatalf("second round: %+v", second)
	}
}
