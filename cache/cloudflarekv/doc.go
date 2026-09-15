// Package cloudflarekv provides a
// [github.com/bernardoforcillo/drops/cache.Cache] backed by
// Cloudflare Workers KV.
//
//	cf, _ := cloudflare.New(accountID, cloudflare.WithAPIToken(token))
//	c, err := cloudflarekv.New(cf, namespaceID)
//
//	_ = c.Set(ctx, "embedding:"+hash, payload, time.Hour)
//	got, err := c.Get(ctx, "embedding:"+hash)
//
// # Read this before using it as a query cache
//
// KV is not a fast cache with a wide reach. It is a read-optimised,
// eventually consistent store, and two of its properties decide what
// it is good for.
//
// A write takes up to sixty seconds to be visible everywhere. Not
// "usually fast, occasionally slow" — the propagation is the design.
// So KV is right for something expensive to compute and safe to serve
// slightly stale: an embedding, a rendered page, a compiled
// configuration. It is wrong for anything invalidated by a write that
// a subsequent read must not miss, which includes the topic
// invalidation in [github.com/bernardoforcillo/drops/cache] and any
// query cache over a table that is being written. Use the in-process
// cache, or Redis, for those.
//
// And a TTL cannot be shorter than sixty seconds — [MinTTL]. A cache
// asked for five seconds and given sixty is not a slow cache, it is a
// wrong one, so this package refuses rather than rounds. [WithRoundUp]
// makes the opposite choice explicitly, for callers who have decided
// the staleness is acceptable.
//
// # How a TTL is read back
//
// [Cache.TTL] and [Cache.Exists] answer from a marker this package
// writes into each key's KV metadata: the absolute expiry, recorded
// at Set. That is why they can answer at all — the value endpoint
// does not report an expiry — and why they answer -1, "no expiry",
// for a key written by something other than this package. [Metadata]
// says what the marker looks like if you need to write one yourself.
package cloudflarekv
