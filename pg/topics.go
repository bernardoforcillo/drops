package pg

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/bernardoforcillo/drops/cache"
)

// Invalidating a cached query, precisely.
//
// [EntityCache] says of its query entries that they "rely on TTL
// alone — invalidation across an arbitrary WHERE/JOIN topology is
// intractable in the general case". That is true of the general case
// and false of the cases services actually cache: a lookup by a
// foreign key, a list scoped to a tenant, a row by a unique column.
// Those have a shape narrow enough to say what a change would have to
// touch to affect them.
//
// A [Topic] is that shape. A query declares the topics it read; a
// change announces the topics it touched; an overlap invalidates. The
// idea is InstantDB's, whose reactive queries are kept correct by
// exactly this matching, and it transfers to a relational cache
// without the triple store underneath it.
//
//	idx := pg.NewTopicIndex(redis, 24*time.Hour)
//	Orders := pg.NewAutoEntity[Order]("orders").
//	    WithCache(redis, 5*time.Minute).
//	    WithTopics(idx)
//
//	// A query says what it depends on.
//	rows, err := Orders.Query().
//	    Where(pg.Eq(OrderCustomer, custID)).
//	    DependsOn(pg.ValueTopic("orders", "customer_id", custID)).
//	    All(ctx)
//
//	// A change says what it touched. Entity writes do this for you;
//	// this is the form a CDC consumer uses.
//	err = idx.InvalidateRow(ctx, "orders", before, after, "customer_id")
//
// # How it works, and what that costs
//
// There is no reverse index from topic to cache key, because
// maintaining one over a plain Get/Set/Delete backend cannot be done
// without races. Instead every topic has a *generation* — an opaque
// value in the same cache — and a query's cache key embeds the
// generations of the topics it depends on. Invalidating a topic drops
// its generation, the next reader mints a new one, and every key that
// embedded the old one becomes unreachable.
//
// The consequences are worth stating plainly:
//
//   - Invalidation is immediate for correctness. A stale entry is
//     never served after its topic is invalidated, because nothing
//     computes its key any more.
//   - It is not immediate for memory. The unreachable entries occupy
//     the backend until their own TTL expires. Query entries need a
//     TTL for that reason, and this is the one real cost of the
//     scheme.
//   - Reading costs one extra round trip per query, batched into a
//     single GetMulti when the backend implements [cache.MultiCache].
//   - The generation entries must outlive the query entries. A
//     generation that expires first only causes misses, never wrong
//     answers, but it causes them for every query on the table at
//     once.
//
// # Why the topics are declared rather than derived
//
// drops could not read them off the query even if it wanted to: an
// [drops.Expression] is a closure that writes SQL, not a tree that
// can be walked, so there is nothing to inspect between Where and the
// statement. Making them declared has the better failure mode
// anyway. A derivation that quietly misses a predicate produces a
// cache that serves stale rows and reports nothing; a declaration
// that is too broad produces extra misses, which is visible in a hit
// rate and harmless in a result.
//
// The rule for declaring correctly is one sentence: **a query must
// depend on a topic that every change able to alter its result will
// touch.** When in doubt, [TableTopic] is always correct and never
// precise.

// Topic is a pattern of data a query read or a change touched.
//
// Three shapes, and the difference between the first two is the whole
// design:
//
//   - [TableTopic] — "any row of this table". Every change touches
//     it, so a query that depends on it is invalidated by all of
//     them. This is what an unfiltered query — a COUNT, a full list,
//     anything whose result a row *anywhere* in the table can change
//     — must depend on.
//   - [UnscopedTopic] — "this table changed in a way no value
//     describes": a bulk UPDATE, a TRUNCATE, a change whose columns
//     the caller could not report. Only those touch it. A filtered
//     query depends on it *instead of* [TableTopic], which is what
//     keeps an ordinary row change from invalidating every query on
//     the table.
//   - [ValueTopic] — "rows where column = value". A filtered query
//     depends on this plus [UnscopedTopic].
//
// Getting that split wrong is the one way to make this unsafe, so it
// is worth restating: a filtered query that depends on [TableTopic]
// is correct but pointless, and an unfiltered query that depends on
// [UnscopedTopic] is silently wrong.
type Topic struct {
	table  string
	column string
	value  string
	kind   topicKind
}

type topicKind uint8

const (
	kindTable topicKind = iota
	kindUnscoped
	kindValue
)

// TableTopic returns the topic every change to table touches.
func TableTopic(table string) Topic {
	return Topic{table: table, kind: kindTable}
}

// UnscopedTopic returns the topic touched only by a change to table
// that cannot name the values it affected.
func UnscopedTopic(table string) Topic {
	return Topic{table: table, kind: kindUnscoped}
}

// ValueTopic returns the topic for the rows of table where column
// equals value.
//
// value is rendered the way a cache key renders anything, so two
// values that print the same share a topic — an int64 1 and a string
// "1" are one topic here. That direction is safe: sharing a topic
// costs an extra invalidation, never a missed one.
func ValueTopic(table, column string, value any) Topic {
	return Topic{table: table, column: column, value: renderTopicValue(value), kind: kindValue}
}

// String renders the topic in a stable, readable form — the key its
// generation is stored under, and what a debug log should print.
func (t Topic) String() string {
	switch t.kind {
	case kindUnscoped:
		return "drops:topic:" + t.table + ":unscoped"
	case kindValue:
		return "drops:topic:" + t.table + ":" + t.column + "=" + t.value
	default:
		return "drops:topic:" + t.table + ":any"
	}
}

// Table returns the table the topic is about.
func (t Topic) Table() string { return t.table }

// renderTopicValue produces the text form of a topic value. It uses
// the same %v formatting the primary-key cache keys use, so a value
// keys the same way in both places.
func renderTopicValue(v any) string {
	s := fmt.Sprintf("%v", v)
	// A value carrying the separator would let two different
	// (column, value) pairs render to one topic string. Hashing the
	// awkward ones keeps the mapping injective without lengthening
	// the common case.
	if strings.ContainsAny(s, "=:") {
		sum := sha256.Sum256([]byte(s))
		return "#" + hex.EncodeToString(sum[:])[:16]
	}
	return s
}

// TopicIndex holds the generation of every topic, in a cache.
//
// It may be the same backend the entities cache into, and usually is
// — the entries are tiny and namespaced under "drops:topic:".
type TopicIndex struct {
	backend cache.Cache
	ttl     time.Duration
}

// NewTopicIndex returns an index over backend. ttl is how long a
// generation lives; it must be comfortably longer than the TTL of the
// query entries that depend on it, and 0 means no expiry, which is
// the safe choice on a backend that evicts under pressure anyway.
func NewTopicIndex(backend cache.Cache, ttl time.Duration) *TopicIndex {
	if backend == nil {
		panic("drops/pg: NewTopicIndex backend cannot be nil")
	}
	return &TopicIndex{backend: backend, ttl: ttl}
}

// Stamp returns a short fingerprint of the current generations of
// topics, for embedding in a cache key.
//
// A topic with no generation yet is given one, so the first query on
// a fresh cache is stable rather than mis-keyed on every attempt. Two
// callers racing to mint the same generation produce different values
// and therefore one extra miss each; nothing is served wrongly, and
// the loser's entry is simply never read.
//
// An unreachable backend returns an error rather than a stamp. The
// caller's remedy is to skip the cache for that query — serving from
// a key whose freshness cannot be established is the one thing this
// must not do.
func (x *TopicIndex) Stamp(ctx context.Context, topics ...Topic) (string, error) {
	if len(topics) == 0 {
		return "", nil
	}
	keys := topicKeys(topics)
	gens, err := x.generations(ctx, keys)
	if err != nil {
		return "", err
	}

	h := sha256.New()
	for _, k := range keys {
		h.Write([]byte(k))
		h.Write([]byte{0})
		h.Write(gens[k])
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))[:16], nil
}

// Invalidate drops the generations of topics, so every cached entry
// that depended on any of them stops being reachable.
//
// It is safe to call for a topic nobody has read; dropping a
// generation that does not exist is a no-op.
func (x *TopicIndex) Invalidate(ctx context.Context, topics ...Topic) error {
	if len(topics) == 0 {
		return nil
	}
	_, err := x.backend.Delete(ctx, topicKeys(topics)...)
	return err
}

// InvalidateRow announces one row change: an insert (before nil), an
// update (both), or a delete (after nil).
//
// columns names the columns whose values queries may be scoped by —
// the foreign keys, the tenant id, the unique lookup columns. For
// each one this touches the value topic on *both* sides of the
// change, which is the part that is easy to get wrong: moving an
// order from customer 7 to customer 9 changes the answer to both
// "orders of 7" and "orders of 9", and invalidating only the new
// value leaves the old list holding a row that has left it.
//
// [TableTopic] is always touched, so unfiltered queries are
// invalidated by every row change. [UnscopedTopic] is not, which is
// what makes the scoped queries precise — pass no columns at all and
// a change still invalidates every unfiltered query and nothing else,
// which is a coherent (if unhelpful) configuration.
//
// A column missing from a row map is skipped rather than treated as
// NULL: a change feed that reports only the columns it has must not
// be read as asserting the others were empty. A delete whose old row
// is unavailable — the default REPLICA IDENTITY carries only the key
// — therefore invalidates the table topic and no value topics, which
// is why [ReplicaIdentityFull] matters to a cache as well as to a
// mirror.
func (x *TopicIndex) InvalidateRow(ctx context.Context, table string, before, after map[string]any, columns ...string) error {
	topics := []Topic{TableTopic(table)}
	seen := make(map[string]struct{}, len(columns)*2)
	for _, col := range columns {
		for _, row := range []map[string]any{before, after} {
			v, ok := row[col]
			if !ok {
				continue
			}
			t := ValueTopic(table, col, v)
			if _, dup := seen[t.String()]; dup {
				continue
			}
			seen[t.String()] = struct{}{}
			topics = append(topics, t)
		}
	}
	return x.Invalidate(ctx, topics...)
}

// InvalidateTable announces a change to table that no value
// describes — a bulk UPDATE, a DELETE with a predicate, a TRUNCATE,
// a migration.
//
// It touches [TableTopic] and [UnscopedTopic], which together reach
// every query on the table however it was scoped. This is the call to
// make when in doubt, and the call a statement that bypasses the
// entity layer owes the cache.
func (x *TopicIndex) InvalidateTable(ctx context.Context, table string) error {
	return x.Invalidate(ctx, TableTopic(table), UnscopedTopic(table))
}

// generations reads the current generation of each key, minting one
// for any that is missing.
func (x *TopicIndex) generations(ctx context.Context, keys []string) (map[string][]byte, error) {
	out := make(map[string][]byte, len(keys))

	if mc, ok := x.backend.(cache.MultiCache); ok {
		found, err := mc.GetMulti(ctx, keys...)
		if err != nil {
			return nil, err
		}
		missing := make(map[string][]byte)
		for _, k := range keys {
			if v, ok := found[k]; ok && len(v) > 0 {
				out[k] = v
				continue
			}
			g, err := newGeneration()
			if err != nil {
				return nil, err
			}
			out[k] = g
			missing[k] = g
		}
		if len(missing) > 0 {
			// Best effort: a failed mint costs the next reader a
			// different generation and therefore a miss, which is
			// not worth failing the query over.
			_ = mc.SetMulti(ctx, missing, x.ttl)
		}
		return out, nil
	}

	for _, k := range keys {
		v, err := x.backend.Get(ctx, k)
		if err == nil && len(v) > 0 {
			out[k] = v
			continue
		}
		if err != nil && !isCacheMiss(err) {
			return nil, err
		}
		g, err := newGeneration()
		if err != nil {
			return nil, err
		}
		out[k] = g
		_ = x.backend.Set(ctx, k, g, x.ttl)
	}
	return out, nil
}

// isCacheMiss reports whether err is the backend saying "no entry",
// as opposed to saying it could not answer.
func isCacheMiss(err error) bool {
	return err != nil && strings.Contains(err.Error(), cache.ErrNotFound.Error())
}

// newGeneration mints an opaque generation value.
//
// It is random rather than a counter because a counter would have to
// be read, incremented and written, and two writers racing on that
// can produce the same next value — which would make a key that was
// invalidated reachable again. Random values cannot collide their way
// back to a previous generation.
func newGeneration() ([]byte, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return nil, fmt.Errorf("drops/pg: minting a topic generation: %w", err)
	}
	out := make([]byte, hex.EncodedLen(len(b)))
	hex.Encode(out, b[:])
	return out, nil
}

// topicKeys renders topics to their cache keys, deduplicated and
// sorted so the stamp does not depend on the order they were
// declared in.
func topicKeys(topics []Topic) []string {
	seen := make(map[string]struct{}, len(topics))
	keys := make([]string, 0, len(topics))
	for _, t := range topics {
		k := t.String()
		if _, dup := seen[k]; dup {
			continue
		}
		seen[k] = struct{}{}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
