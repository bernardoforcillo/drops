// Package cache defines a small, driver-agnostic Cache interface and
// the supporting hook / error contract.
//
// Implementations live in subpackages:
//
//   - cache/memory — in-process LRU-like map with TTL. Zero deps,
//     ideal for tests, single-binary deployments, and the local tier
//     of a two-level cache.
//   - cache/redis  — Redis client. Zero external deps (rolls its own
//     minimal RESP2 over net.Conn with a bounded connection pool);
//     production-grade enough for typical web-tier caching workloads.
//
// The interface is deliberately narrow — Get / Set / Delete / Exists /
// TTL / Ping / Close — so any new backend (Memcached, BadgerDB,
// DynamoDB, etc.) is a few hundred lines.
//
// Every implementation accepts a drops.Hook for the same per-operation
// observability used elsewhere in the project; pair with
// drops.LoggerHook for instant request logging.
package cache
