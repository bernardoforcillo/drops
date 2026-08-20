// Package pgwire is a minimal PostgreSQL client speaking the v3 wire
// protocol directly over a socket.
//
// It exists because of a constraint the rest of drops imposes on
// itself: the library has no dependencies, so the `drops` binary —
// which lives in the same module — cannot import pgx or lib/pq. A CLI
// that cannot open a connection is not a CLI, so the protocol is
// implemented here, in the standard library, and wrapped as a
// drops.Driver like any other.
//
// What it covers is what the migration commands need and no more:
// simple and extended query, text format on both sides, trust,
// cleartext, MD5 and SCRAM-SHA-256 authentication, TLS, transactions
// and savepoints. It is not a connection pool, it is not safe for
// concurrent use beyond the mutex it holds, and it does not implement
// COPY, LISTEN or binary parameters. Application code should keep
// using pgx through drops/stdlib.
//
// It sits under cmd/ but outside internal/ on purpose: a wire
// protocol is server behaviour from end to end, and the integration
// suite — a separate module — has to be able to import it and point it
// at a real PostgreSQL.
package pgwire
