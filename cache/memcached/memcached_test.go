package memcached_test

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bernardoforcillo/drops/cache"
	"github.com/bernardoforcillo/drops/cache/memcached"
)

// --- fake memcached server -------------------------------------------

type fakeServer struct {
	ln    net.Listener
	mu    sync.Mutex
	store map[string]fakeEntry
}

type fakeEntry struct {
	val []byte
	exp time.Time // zero = no expiry
}

func newFakeServer(t *testing.T) *fakeServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &fakeServer{ln: ln, store: map[string]fakeEntry{}}
	go s.serve()
	t.Cleanup(func() { _ = ln.Close() })
	return s
}

func (s *fakeServer) addr() string { return s.ln.Addr().String() }

func (s *fakeServer) serve() {
	for {
		c, err := s.ln.Accept()
		if err != nil {
			return
		}
		go s.handle(c)
	}
}

func (s *fakeServer) get(key string) ([]byte, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.store[key]
	if !ok {
		return nil, false
	}
	if !e.exp.IsZero() && time.Now().After(e.exp) {
		delete(s.store, key)
		return nil, false
	}
	return e.val, true
}

func (s *fakeServer) handle(c net.Conn) {
	defer c.Close()
	r := bufio.NewReader(c)
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		switch fields[0] {
		case "set":
			// set <key> <flags> <exptime> <bytes>
			key := fields[1]
			exptime, _ := strconv.Atoi(fields[3])
			n, _ := strconv.Atoi(fields[4])
			body := make([]byte, n+2)
			_, _ = io.ReadFull(r, body)
			e := fakeEntry{val: append([]byte(nil), body[:n]...)}
			if exptime > 0 {
				e.exp = time.Now().Add(time.Duration(exptime) * time.Second)
			}
			s.mu.Lock()
			s.store[key] = e
			s.mu.Unlock()
			_, _ = c.Write([]byte("STORED\r\n"))
		case "get":
			var sb strings.Builder
			for _, key := range fields[1:] {
				if v, ok := s.get(key); ok {
					sb.WriteString(fmt.Sprintf("VALUE %s 0 %d\r\n", key, len(v)))
					sb.Write(v)
					sb.WriteString("\r\n")
				}
			}
			sb.WriteString("END\r\n")
			_, _ = c.Write([]byte(sb.String()))
		case "delete":
			key := fields[1]
			s.mu.Lock()
			_, ok := s.store[key]
			delete(s.store, key)
			s.mu.Unlock()
			if ok {
				_, _ = c.Write([]byte("DELETED\r\n"))
			} else {
				_, _ = c.Write([]byte("NOT_FOUND\r\n"))
			}
		case "mg":
			// mg <key> t  → HD t<ttl> | EN
			key := fields[1]
			s.mu.Lock()
			e, ok := s.store[key]
			s.mu.Unlock()
			if !ok || (!e.exp.IsZero() && time.Now().After(e.exp)) {
				_, _ = c.Write([]byte("EN\r\n"))
				break
			}
			ttl := -1
			if !e.exp.IsZero() {
				ttl = int(time.Until(e.exp).Seconds())
			}
			_, _ = c.Write([]byte(fmt.Sprintf("HD t%d\r\n", ttl)))
		case "version":
			_, _ = c.Write([]byte("VERSION 1.6.0-fake\r\n"))
		default:
			_, _ = c.Write([]byte("ERROR\r\n"))
		}
	}
}

// --- tests ------------------------------------------------------------

func newClient(t *testing.T) *memcached.Cache {
	t.Helper()
	s := newFakeServer(t)
	c, err := memcached.New(memcached.Options{Addr: s.addr(), MaxConns: 4})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func TestSetGet(t *testing.T) {
	ctx := context.Background()
	c := newClient(t)

	if err := c.Set(ctx, "k", []byte("hello"), time.Minute); err != nil {
		t.Fatal(err)
	}
	v, err := c.Get(ctx, "k")
	if err != nil || string(v) != "hello" {
		t.Fatalf("Get: %q %v", v, err)
	}
}

func TestGetMiss(t *testing.T) {
	c := newClient(t)
	if _, err := c.Get(context.Background(), "absent"); !errors.Is(err, cache.ErrNotFound) {
		t.Errorf("want ErrNotFound, got %v", err)
	}
}

func TestDeleteAndExists(t *testing.T) {
	ctx := context.Background()
	c := newClient(t)
	_ = c.Set(ctx, "k", []byte("v"), time.Minute)

	if ok, _ := c.Exists(ctx, "k"); !ok {
		t.Error("Exists should be true")
	}
	n, err := c.Delete(ctx, "k")
	if err != nil || n != 1 {
		t.Fatalf("Delete: n=%d err=%v", n, err)
	}
	if ok, _ := c.Exists(ctx, "k"); ok {
		t.Error("Exists should be false after delete")
	}
	// Deleting a missing key counts 0.
	if n, _ := c.Delete(ctx, "k"); n != 0 {
		t.Errorf("delete missing: n=%d", n)
	}
}

func TestTTL(t *testing.T) {
	ctx := context.Background()
	c := newClient(t)

	_ = c.Set(ctx, "expiring", []byte("v"), time.Minute)
	ttl, err := c.TTL(ctx, "expiring")
	if err != nil {
		t.Fatal(err)
	}
	if ttl <= 0 || ttl > time.Minute {
		t.Errorf("ttl out of range: %v", ttl)
	}

	_ = c.Set(ctx, "forever", []byte("v"), 0)
	ttl, err = c.TTL(ctx, "forever")
	if err != nil || ttl != -1 {
		t.Errorf("no-expiry ttl: %v %v", ttl, err)
	}

	if _, err := c.TTL(ctx, "absent"); !errors.Is(err, cache.ErrNotFound) {
		t.Errorf("absent ttl: want ErrNotFound, got %v", err)
	}
}

func TestPing(t *testing.T) {
	if err := newClient(t).Ping(context.Background()); err != nil {
		t.Errorf("Ping: %v", err)
	}
}

func TestMultiAndPrefix(t *testing.T) {
	ctx := context.Background()
	s := newFakeServer(t)
	c, _ := memcached.New(memcached.Options{Addr: s.addr(), KeyPrefix: "app:"})
	defer c.Close()

	if err := c.SetMulti(ctx, map[string][]byte{"a": []byte("1"), "b": []byte("2")}, time.Minute); err != nil {
		t.Fatal(err)
	}
	// Stored under the prefix on the wire.
	if v, ok := s.get("app:a"); !ok || string(v) != "1" {
		t.Errorf("prefix not applied on set: %q %v", v, ok)
	}
	got, err := c.GetMulti(ctx, "a", "b", "missing")
	if err != nil {
		t.Fatal(err)
	}
	// Keys come back prefixed (wire keys).
	if string(got["app:a"]) != "1" || string(got["app:b"]) != "2" || len(got) != 2 {
		t.Errorf("GetMulti wrong: %v", got)
	}
}

func TestInvalidKey(t *testing.T) {
	c := newClient(t)
	if err := c.Set(context.Background(), "bad key", []byte("v"), 0); !errors.Is(err, cache.ErrInvalidKey) {
		t.Errorf("want ErrInvalidKey for spaced key, got %v", err)
	}
	if err := c.Set(context.Background(), "", []byte("v"), 0); !errors.Is(err, cache.ErrInvalidKey) {
		t.Errorf("want ErrInvalidKey for empty key, got %v", err)
	}
}

func TestClosedRejects(t *testing.T) {
	c := newClient(t)
	_ = c.Close()
	if _, err := c.Get(context.Background(), "k"); !errors.Is(err, cache.ErrClosed) {
		t.Errorf("want ErrClosed, got %v", err)
	}
}

func TestConcurrentPooled(t *testing.T) {
	ctx := context.Background()
	c := newClient(t)
	var wg sync.WaitGroup
	for i := 0; i < 30; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			k := "k" + strconv.Itoa(i)
			if err := c.Set(ctx, k, []byte(k), time.Minute); err != nil {
				t.Errorf("set: %v", err)
				return
			}
			v, err := c.Get(ctx, k)
			if err != nil || string(v) != k {
				t.Errorf("get %s: %q %v", k, v, err)
			}
		}(i)
	}
	wg.Wait()
}
