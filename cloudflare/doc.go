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
//     runs against it unchanged. Its Admin type manages the databases
//     themselves: provisioning, read replication, Time Travel,
//     export and import.
//   - [github.com/bernardoforcillo/drops/cloudflare/vectorize] —
//     Vectorize, as a [github.com/bernardoforcillo/drops/vector.Store].
//   - [github.com/bernardoforcillo/drops/cache/cloudflarekv] — Workers
//     KV, as a [github.com/bernardoforcillo/drops/cache.Cache].
//   - [github.com/bernardoforcillo/drops/cloudflare/r2] — R2, which is
//     not a database but where the operations that produce a file put
//     it: a D1 export that has to outlive Time Travel, a schema dump.
//   - [github.com/bernardoforcillo/drops/cloudflare/queues] — Queues,
//     the durable hop an outbox publishes to, and a
//     [github.com/bernardoforcillo/drops/mirror.Sink] for the mirror
//     whose far end is not a store.
//   - [github.com/bernardoforcillo/drops/cloudflare/hyperdrive] —
//     Hyperdrive, which is not a backend at all but a pooler in front
//     of your own PostgreSQL or MySQL. It has a client, for creating
//     the configuration a Worker binds; what it mostly has is a list
//     of the drops features that stop working behind one.
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
// Workers KV Storage:Edit, Workers R2 Storage:Edit, Queues:Edit,
// Hyperdrive:Edit) instead.
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
//
// # Bodies
//
// [Request.Body] is JSON-encoded and [Request.Raw] is sent verbatim.
// [Request.Stream] is the third form, for a payload there is no
// reason to hold in memory — an R2 object, a database dump on its way
// to one. It is a factory rather than a reader because a retry has to
// send the body again, and a reader that has been drained has nothing
// left to send.
package cloudflare
