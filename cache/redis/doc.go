// Package redis provides a cache.Cache implementation backed by a
// Redis server.
//
// Wire-level: a minimal RESP2 client over net.Conn (no external deps).
// Connection-level: a bounded pool with configurable max size, idle
// timeout, and lazy connection. Operations honour the calling
// context's deadline by setting Read/Write deadlines on the wire.
//
// Supported commands cover the cache-relevant subset:
//
//	GET, SET (with EX/PX/NX/XX), DEL, EXISTS, TTL/PTTL, PING,
//	MGET, MSET, INCR/DECR, EXPIRE, FLUSHDB, SELECT, AUTH
//
// For richer Redis usage (pub/sub, streams, transactions, scripts,
// cluster mode, sentinel), reach for a full client like
// github.com/redis/go-redis/v9 — this package's scope is the
// cache.Cache contract plus a few utility commands.
package redis
