package redis_test

import (
	"context"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bernardoforcillo/drops/cache/redis"
)

// closeRecordingConn reports whether the socket was ever closed, which
// is the only thing distinguishing a connection returned to the pool
// from one leaked.
type closeRecordingConn struct {
	net.Conn
	closed *atomic.Bool
}

func (c *closeRecordingConn) Close() error {
	c.closed.Store(true)
	return c.Conn.Close()
}

// The warm-up goroutine is the one background worker in the redis
// backend, and the dial it performs is slow by nature — DialTimeout
// defaults to five seconds, and a warm-up racing a shutdown is the
// normal shape of a failed boot. Close drains the pool once and then
// relies on put to discard anything handed back afterwards; warm-up
// does not go through put, so a connection completed after that drain
// is parked in a pool nobody will read again and stays open until the
// process exits. On a service that builds a cache per tenant that is a
// file-descriptor leak with no counter attached to it.
func TestWarmUpDoesNotLeakAConnectionDialledDuringClose(t *testing.T) {
	fr := newFakeRedis(t)

	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	var closed atomic.Bool

	c := redis.New(redis.Options{
		Addr:         fr.addr(),
		MaxConns:     2,
		MinIdleConns: 1,
		Dialer: func(_ context.Context, network, addr string) (net.Conn, error) {
			select {
			case entered <- struct{}{}:
			default:
			}
			<-release
			nc, err := net.Dial(network, addr)
			if err != nil {
				return nil, err
			}
			return &closeRecordingConn{Conn: nc, closed: &closed}, nil
		},
	})

	// Hold the warm-up inside the dialer, close the cache underneath
	// it, and only then let the dial complete.
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("warm-up never reached the dialer")
	}
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	close(release)

	deadline := time.Now().Add(2 * time.Second)
	for !closed.Load() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !closed.Load() {
		t.Error("the connection warm-up dialled during Close was never closed")
	}
}
