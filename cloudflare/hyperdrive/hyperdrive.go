// Package hyperdrive is the drops support for Cloudflare Hyperdrive.
//
// Hyperdrive is not a database. It is a connection pooler and query
// cache in front of PostgreSQL or MySQL that you already run, so the
// drops dialect stays [github.com/bernardoforcillo/drops/pg] or
// [github.com/bernardoforcillo/drops/mysql] and the driver stays
// whatever it was. Nothing in this package talks to Cloudflare.
//
// What it does instead is the two things that go wrong when a schema
// written for a direct connection is pointed at a pooler.
//
// The first is the connection string. [Config.DSN] builds one from a
// binding's parts, and refuses the combinations Hyperdrive will not
// serve rather than letting them fail at connect time.
//
// The second is the part nobody checks until production. A pooler
// does not hold a session, and a great deal of PostgreSQL is session
// state: a prepared statement, a SET, a temporary table, an advisory
// lock, a LISTEN. drops uses several of those, and the features that
// depend on them do not degrade behind Hyperdrive — they silently do
// the wrong thing, or hang. [Unsupported] is the list, [Check] is how
// a startup path asserts against it, and [Report] is how a human
// reads it.
//
//	if err := hyperdrive.Check(hyperdrive.Everything()...); err != nil {
//	    log.Fatal(err)
//	}
package hyperdrive

import (
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

// Sentinel errors.
var (
	// ErrUnsupported is returned by [Check] when a feature cannot
	// work behind Hyperdrive.
	ErrUnsupported = errors.New("drops/cloudflare/hyperdrive: feature is not available behind Hyperdrive")

	// ErrIncompleteConfig is returned by [Config.DSN] when a
	// required field is missing.
	ErrIncompleteConfig = errors.New("drops/cloudflare/hyperdrive: incomplete configuration")
)

// Engine is the database Hyperdrive fronts.
type Engine string

// The engines Hyperdrive supports.
const (
	PostgreSQL Engine = "postgresql"
	MySQL      Engine = "mysql"
)

// Config is a Hyperdrive origin — the database Hyperdrive connects to
// on your behalf, or the local connection string it hands a Worker.
//
// Inside a Worker you do not build one of these: the binding gives
// you env.HYPERDRIVE.connectionString already assembled. This is for
// everywhere else — a Container, a sidecar, a migration runner —
// where the parts arrive as separate environment variables and have
// to be put back together.
type Config struct {
	// Engine is postgresql or mysql. Defaults to [PostgreSQL].
	Engine Engine

	// Host and Port address Hyperdrive, or the origin database when
	// connecting past it.
	Host string
	Port int

	// Database is the database name.
	Database string

	// User and Password authenticate.
	User     string
	Password string

	// Params are extra connection parameters appended to the query
	// string — application_name, connect_timeout, and so on.
	Params map[string]string

	// SSLMode is the PostgreSQL sslmode. Empty leaves it unset,
	// which is right when connecting to Hyperdrive itself: the hop
	// from a Worker to Hyperdrive does not leave Cloudflare's
	// network, and Hyperdrive makes its own TLS connection to your
	// origin. Set it when this Config addresses the origin
	// directly.
	SSLMode string
}

// DSN renders the connection string.
//
// The password is percent-encoded, which matters more than it sounds:
// a generated password containing a "/" or a "@" produces a URL that
// parses as a different host, and the failure that follows names
// neither the password nor the escaping.
func (c Config) DSN() (string, error) {
	engine := c.Engine
	if engine == "" {
		engine = PostgreSQL
	}
	switch engine {
	case PostgreSQL, MySQL:
	default:
		return "", fmt.Errorf("%w: unknown engine %q", ErrIncompleteConfig, engine)
	}
	var missing []string
	if c.Host == "" {
		missing = append(missing, "Host")
	}
	if c.Database == "" {
		missing = append(missing, "Database")
	}
	if c.User == "" {
		missing = append(missing, "User")
	}
	if len(missing) > 0 {
		return "", fmt.Errorf("%w: %s", ErrIncompleteConfig, strings.Join(missing, ", "))
	}

	port := c.Port
	if port == 0 {
		if engine == MySQL {
			port = 3306
		} else {
			port = 5432
		}
	}

	scheme := "postgresql"
	if engine == MySQL {
		scheme = "mysql"
	}

	u := &url.URL{
		Scheme: scheme,
		User:   url.UserPassword(c.User, c.Password),
		Host:   c.Host + ":" + strconv.Itoa(port),
		Path:   "/" + c.Database,
	}
	q := url.Values{}
	if c.SSLMode != "" {
		q.Set("sslmode", c.SSLMode)
	}
	keys := make([]string, 0, len(c.Params))
	for k := range c.Params {
		keys = append(keys, k)
	}
	// Sorted so the DSN is stable, which is what makes it
	// comparable in a test and diffable in a log.
	sort.Strings(keys)
	for _, k := range keys {
		q.Set(k, c.Params[k])
	}
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// Feature names a drops capability that Hyperdrive changes or
// removes.
type Feature string

// The features Hyperdrive takes away. Each names the drops symbol
// that stops working, so a grep for the symbol finds the constant.
const (
	// FeatureListenNotify — [github.com/bernardoforcillo/drops/pg.Listen]
	// and Notify. LISTEN needs a connection held open for as long as
	// the subscription lasts, and a pooler hands the connection back
	// between statements. The subscription is silently never
	// delivered to.
	FeatureListenNotify Feature = "listen_notify"

	// FeatureLogicalReplication — [github.com/bernardoforcillo/drops/pg.Stream],
	// CreateSlot and the rest of pg's change data capture, plus
	// [github.com/bernardoforcillo/drops/mirror]'s logical source.
	// These speak the replication protocol, which is a different
	// startup packet, not a query. Hyperdrive does not carry it.
	FeatureLogicalReplication Feature = "logical_replication"

	// FeatureAdvisoryLocks —
	// [github.com/bernardoforcillo/drops/pg.WithAdvisoryLock] and
	// TryWithAdvisoryLock. A session-scoped advisory lock is held by
	// the connection, so behind a pooler it is released at a moment
	// nobody chose. The transaction-scoped form drops uses
	// (pg_advisory_xact_lock) is safe, because the transaction pins
	// the connection for its lifetime — but only inside an explicit
	// transaction. Taking one outside is the failure this names.
	FeatureAdvisoryLocks Feature = "session_advisory_locks"

	// FeatureStatementRegistry —
	// [github.com/bernardoforcillo/drops/pg.StatementRegistry]. It
	// cancels an in-flight statement with pg_cancel_backend against
	// the backend PID it recorded; behind a pooler the PID it
	// recorded is not the PID the statement is running on, so the
	// cancellation lands on someone else's query or on nothing.
	FeatureStatementRegistry Feature = "statement_registry"

	// FeatureSessionState — SET, SET LOCAL outside a transaction,
	// temporary tables, cursors held between statements, and
	// server-side prepared statements. None of them survive the
	// connection going back to the pool, and none of them announce
	// that they did not.
	FeatureSessionState Feature = "session_state"

	// FeatureCopy — COPY FROM STDIN, which
	// [github.com/bernardoforcillo/drops/pg]'s bulk paths use where
	// the driver offers it. It is a protocol mode rather than a
	// statement.
	FeatureCopy Feature = "copy"
)

// Unsupported describes one feature Hyperdrive takes away.
type Unsupported struct {
	// Feature is the capability.
	Feature Feature

	// Symbols are the drops identifiers that stop working, for a
	// grep that finds the call sites.
	Symbols []string

	// Why is the mechanism, in one sentence — not "unsupported" but
	// what the pooler does that breaks it.
	Why string

	// Instead is what to do, when there is something to do.
	Instead string

	// Silent reports whether the failure is quiet. These are the
	// dangerous ones: a feature that errors is found in testing, a
	// feature that returns the wrong answer is found in production.
	Silent bool
}

// Everything returns every feature Hyperdrive takes away.
//
// Pass the ones your application uses to [Check], or all of them from
// a startup path, so the list is asserted against rather than read
// once and forgotten.
func Everything() []Feature {
	out := make([]Feature, 0, len(unsupported))
	for f := range unsupported {
		out = append(out, f)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// Describe returns the entry for a feature.
func Describe(f Feature) (Unsupported, bool) {
	u, ok := unsupported[f]
	return u, ok
}

// Check returns an error naming every listed feature that cannot work
// behind Hyperdrive.
//
// It is meant for a startup path in a build that targets Hyperdrive:
// list what the application uses, and find out at boot rather than at
// the first NOTIFY that nobody receives.
//
//	// This service publishes over LISTEN/NOTIFY and runs a CDC
//	// stream; neither survives a pooler.
//	if err := hyperdrive.Check(
//	    hyperdrive.FeatureListenNotify,
//	    hyperdrive.FeatureLogicalReplication,
//	); err != nil {
//	    return err
//	}
func Check(features ...Feature) error {
	var problems []string
	for _, f := range features {
		u, ok := unsupported[f]
		if !ok {
			continue
		}
		line := fmt.Sprintf("%s (%s): %s", u.Feature, strings.Join(u.Symbols, ", "), u.Why)
		if u.Instead != "" {
			line += " " + u.Instead
		}
		problems = append(problems, line)
	}
	if len(problems) == 0 {
		return nil
	}
	return fmt.Errorf("%w:\n  - %s", ErrUnsupported, strings.Join(problems, "\n  - "))
}

// Report renders every feature as text, for a startup log or a
// migration note.
func Report() string {
	var b strings.Builder
	b.WriteString("Behind Cloudflare Hyperdrive, these drops features do not work:\n")
	for _, f := range Everything() {
		u := unsupported[f]
		b.WriteString("\n  " + string(u.Feature))
		if u.Silent {
			b.WriteString("  [fails silently]")
		}
		b.WriteString("\n    symbols: " + strings.Join(u.Symbols, ", "))
		b.WriteString("\n    why:     " + u.Why)
		if u.Instead != "" {
			b.WriteString("\n    instead: " + u.Instead)
		}
		b.WriteString("\n")
	}
	return b.String()
}

// Works lists what does keep working, because a list of what breaks
// invites the assumption that everything else is suspect.
//
// It is most of drops. Every query builder, every entity operation,
// every DDL statement and the file-based migrations are ordinary SQL
// over an ordinary connection. So are the outbox, the durable job
// queue and the saga runner: they are built on SELECT … FOR UPDATE
// SKIP LOCKED inside a transaction, and a transaction pins a
// connection for its lifetime even behind a pooler. The query cache
// works too, as long as its invalidation is not the LISTEN-based one.
func Works() []string {
	return []string{
		"query builders, entities, relations and eager loading",
		"DDL and file-based migrations",
		"transactions, including SELECT ... FOR UPDATE SKIP LOCKED",
		"the outbox, the job queue and the saga runner",
		"transaction-scoped advisory locks (pg_advisory_xact_lock)",
		"the query cache, with any invalidation that is not LISTEN-based",
		"query tags (SQLCommenter comments) and every drops.Hook",
	}
}

// unsupported is the table the rest of the package reads.
var unsupported = map[Feature]Unsupported{
	FeatureListenNotify: {
		Feature: FeatureListenNotify,
		Symbols: []string{"pg.Listen", "pg.Notify", "pg.SupportsListen"},
		Why:     "LISTEN needs one connection held for the life of the subscription, and a pooler returns the connection between statements.",
		Instead: "Poll the outbox, or publish through a Cloudflare Queue from the writer.",
		Silent:  true,
	},
	FeatureLogicalReplication: {
		Feature: FeatureLogicalReplication,
		Symbols: []string{"pg.Stream", "pg.Streamer", "pg.CreateSlot", "pg.Publication", "mirror.LogicalSource"},
		Why:     "The replication protocol is a different startup packet rather than a query, and Hyperdrive does not carry it.",
		Instead: "Connect the CDC reader to the origin database directly, past Hyperdrive. It is one long-lived connection, which is what a pooler is least useful for anyway.",
	},
	FeatureAdvisoryLocks: {
		Feature: FeatureAdvisoryLocks,
		Symbols: []string{"pg.WithAdvisoryLock", "pg.TryWithAdvisoryLock"},
		Why:     "A session-scoped advisory lock is released when the connection returns to the pool, which is a moment no caller chose.",
		Instead: "Take the lock inside an explicit transaction, where pg_advisory_xact_lock pins the connection for its lifetime.",
		Silent:  true,
	},
	FeatureStatementRegistry: {
		Feature: FeatureStatementRegistry,
		Symbols: []string{"pg.StatementRegistry", "pg.StatementRegistry.CancelAll", "pg.StatementRegistry.Drain"},
		Why:     "Cancellation goes to the backend PID the registry recorded, and behind a pooler the statement is running on a different one.",
		Instead: "Cancel through the context instead, and let the query time out at the server with statement_timeout.",
		Silent:  true,
	},
	FeatureSessionState: {
		Feature: FeatureSessionState,
		Symbols: []string{"SET", "CREATE TEMP TABLE", "DECLARE ... CURSOR", "PREPARE"},
		Why:     "None of it survives the connection going back to the pool, and none of it reports that it did not.",
		Instead: "Put a SET inside the transaction that needs it (SET LOCAL), and replace a temporary table with a CTE or a real table keyed by request.",
		Silent:  true,
	},
	FeatureCopy: {
		Feature: FeatureCopy,
		Symbols: []string{"pg.Copier", "COPY FROM STDIN"},
		Why:     "COPY is a protocol mode rather than a statement, so a pooler that speaks only the query protocol cannot proxy it.",
		Instead: "Use a multi-row INSERT, which drops' batch insert already emits.",
	},
}
