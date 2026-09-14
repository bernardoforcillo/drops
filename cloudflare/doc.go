// Package cloudflare is the shared client the drops Cloudflare
// backends are built on: one API token, one account, one envelope,
// one retry policy.
//
// Cloudflare's REST API is uniform in a way that is worth factoring
// out. Every endpoint lives under
// /client/v4/accounts/{account_id}/…, every one authenticates with
// the same bearer token, and every one answers with the same
// envelope:
//
//	{"result": …, "success": true, "errors": [], "messages": []}
//
// The envelope is the reason this package exists rather than four
// copies of net/http boilerplate. `success: false` arrives with HTTP
// 200 often enough that a status-code check alone lets a failure
// through, and the useful part of a Cloudflare failure is not the
// status but the numeric code inside errors[] — 7003 is "no route for
// that URI", 10000 is "authentication error", 1000 is "a D1 statement
// would not parse". [APIError] carries those codes and
// [APIError.HasCode] is how a caller branches on one.
//
// # The backends
//
//   - [github.com/bernardoforcillo/drops/cloudflare/d1] — D1, as a
//     [github.com/bernardoforcillo/drops.Driver]. The SQLite dialect
//     runs against it unchanged.
//   - [github.com/bernardoforcillo/drops/cloudflare/vectorize] —
//     Vectorize, as a [github.com/bernardoforcillo/drops/vector.Store].
//   - [github.com/bernardoforcillo/drops/cache/cloudflarekv] — Workers
//     KV, as a [github.com/bernardoforcillo/drops/cache.Cache].
//   - [github.com/bernardoforcillo/drops/cloudflare/hyperdrive] —
//     Hyperdrive, which is not a backend at all but a pooler in front
//     of your own PostgreSQL or MySQL. It has no client here; what it
//     has is a list of the drops features that stop working behind it.
//
// # Credentials
//
//	cf, err := cloudflare.New(accountID, cloudflare.WithAPIToken(tok))
//
// An API token is the supported credential. Cloudflare's legacy
// global API key — the one paired with an account email — is not
// offered: it authenticates as the whole account with no way to scope
// it down, so a leaked one is a leaked account. Mint a token with
// only the permission the backend needs (D1:Edit, Vectorize:Edit,
// Workers KV Storage:Edit) instead.
//
// # Retries
//
// Do retries on 429 and on 5xx, honouring a Retry-After header when
// the response carries one and backing off exponentially with jitter
// when it does not. Only requests that are safe to repeat are
// retried: see [Client.Do] for what that means and
// [WithRetryPolicy] for how to change it.
//
// # Observability
//
// A [github.com/bernardoforcillo/drops.Hook] installed with
// [WithHook] fires once per HTTP request — after the retries, not
// once per attempt — with Kind="http" and SQL set to "<METHOD>
// <path>", the same shape drops/qdrant emits. Chain it with
// [github.com/bernardoforcillo/drops.LoggerHook] for request logging
// that reads the same as the SQL backends'.
package cloudflare
