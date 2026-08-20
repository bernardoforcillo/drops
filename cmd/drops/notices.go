package main

import (
	"strings"

	"github.com/jackc/pgx/v5/pgconn"
)

// boilerplateCodes are the SQLSTATEs PostgreSQL attaches to the
// notices an IF [NOT] EXISTS clause produces when it does nothing:
// duplicate schema, duplicate table, duplicate object, and their
// counterparts on the way down.
var boilerplateCodes = map[string]bool{
	"42P06": true, // schema "x" already exists, skipping
	"42P07": true, // relation "x" already exists, skipping
	"42710": true, // constraint/object "x" already exists, skipping
	"42704": true, // object "x" does not exist, skipping
	"42P01": true, // table "x" does not exist, skipping
}

// boilerplateNotice reports whether a notice says only that an
// IF [NOT] EXISTS clause found nothing to do.
//
// Those arrive on every run of an idempotent migration, so printing
// them trains the reader to skip the notice line — which is where the
// one that mattered would have been.
func boilerplateNotice(n *pgconn.Notice) bool {
	if n == nil {
		return true
	}
	return strings.EqualFold(n.Severity, "notice") && boilerplateCodes[n.Code]
}
