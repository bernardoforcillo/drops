package main

import (
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// Notices from IF [NOT] EXISTS arrive on every run of an idempotent
// migration. Printing them teaches the reader to skip the notice line,
// which is where the one that mattered would have been.
func TestBoilerplateNotice(t *testing.T) {
	cases := []struct {
		name   string
		notice *pgconn.Notice
		want   bool
	}{
		{"relation already exists", &pgconn.Notice{Severity: "NOTICE", Code: "42P07"}, true},
		{"table does not exist", &pgconn.Notice{Severity: "notice", Code: "42P01"}, true},
		{"a notice worth reading", &pgconn.Notice{Severity: "NOTICE", Code: "01000", Message: "identifier will be truncated"}, false},
		{"a warning is never boilerplate", &pgconn.Notice{Severity: "WARNING", Code: "42P07"}, false},
	}
	for _, tc := range cases {
		if got := boilerplateNotice(tc.notice); got != tc.want {
			t.Errorf("%s: boilerplateNotice = %v, want %v", tc.name, got, tc.want)
		}
	}
}
