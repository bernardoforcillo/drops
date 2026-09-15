// Package r2 is Cloudflare R2 object storage, for the drops
// operations that produce a file rather than a row.
//
// R2 is not a database and drops does not run queries against it.
// What it is, to a library like this one, is where the artefacts go:
// a [github.com/bernardoforcillo/drops/cloudflare/d1] export that has
// to outlive Time Travel's thirty days, a schema dump kept beside a
// migration, a large document whose row holds only the key to it.
// Those all have the same shape — write a file, name it, fetch it
// back — and that shape is this package.
//
//	cf, _ := cloudflare.New(accountID, cloudflare.WithAPIToken(token))
//	store := r2.New(cf).Bucket("backups")
//	err := store.PutFile(ctx, "d1/tenants-2026-09-15.sql", path, r2.PutOptions{})
//
// # Which R2 API this is
//
// R2 has three front doors and this package uses the least famous of
// them: Cloudflare's own REST API, at
// /accounts/{account}/r2/buckets/…. That is a deliberate choice with
// a real cost, so it is worth stating both halves.
//
// What it buys is that R2 becomes one more thing the account's API
// token reaches. No SigV4 signing, no second set of credentials to
// mint and rotate, no second SDK — the same
// [github.com/bernardoforcillo/drops/cloudflare.Client] carries it,
// with the same retry policy and the same hook, so an R2 write shows
// up in a log next to the D1 statement that produced it.
//
// What it costs is the ceiling. This endpoint takes objects up to
// 300 MB and has no multipart upload, so there is no resuming a
// failed one and no going above the limit by splitting it.
// [MaxObjectSize] is the number, [ErrObjectTooLarge] is what a larger
// [Bucket.PutFile] returns before it starts sending, and R2's
// S3-compatible API — with an S3 SDK, and R2 access keys rather than
// an API token — is the answer for anything bigger. A database dump
// reaches 300 MB long before a 10 GB D1 database does, so check
// against it rather than assuming.
//
// # Keys are paths
//
// An object key may contain slashes and they are not escaped away:
// "d1/2026-09-15/tenants.sql" addresses that key, and [Bucket.List]
// with a prefix and a delimiter walks it as though it were a
// directory. Every other character in a key is escaped, so a key
// arriving from a tenant name cannot climb out of the prefix it was
// meant to be under.
//
// # Consistency
//
// R2 is strongly consistent for reads after a write of an object,
// which is what makes it usable as the durable half of a pipeline —
// unlike [github.com/bernardoforcillo/drops/cache/cloudflarekv],
// whose sixty-second propagation is the reason that package is a
// cache and this one is not.
package r2
