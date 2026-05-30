package redis_test

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

	"github.com/bernardoforcillo/drops"
	"github.com/bernardoforcillo/drops/cache"
	"github.com/bernardoforcillo/drops/cache/redis"
)

// fakeRedis is a tiny in-process Redis-shaped server that speaks RESP2
// just well enough to exercise the client end-to-end. It supports the
// commands the cache.Cache implementation actually uses.
type fakeRedis struct {
	t        *testing.T
	listener net.Listener
	mu       sync.Mutex
	data     map[string]fakeEntry
	authPass string
	conns    []net.Conn // tracked so tests can force-close from the server side
	cmds     []string   // every command name seen, lowercase
}

type fakeEntry struct {
	value     []byte
	expiresAt time.Time // zero = no expiry
}

func newFakeRedis(t *testing.T) *fakeRedis {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	fr := &fakeRedis{t: t, listener: l, data: map[string]fakeEntry{}}
	go fr.serve()
	t.Cleanup(func() { _ = l.Close() })
	return fr
}

func (fr *fakeRedis) addr() string { return fr.listener.Addr().String() }

func (fr *fakeRedis) serve() {
	for {
		c, err := fr.listener.Accept()
		if err != nil {
			return
		}
		go fr.handle(c)
	}
}

func (fr *fakeRedis) handle(c net.Conn) {
	fr.mu.Lock()
	fr.conns = append(fr.conns, c)
	fr.mu.Unlock()
	defer c.Close()
	r := bufio.NewReader(c)
	w := bufio.NewWriter(c)
	for {
		args, err := readCmd(r)
		if err != nil {
			return
		}
		if len(args) == 0 {
			continue
		}
		fr.mu.Lock()
		fr.cmds = append(fr.cmds, strings.ToUpper(string(args[0])))
		fr.mu.Unlock()
		fr.dispatch(w, args)
		_ = w.Flush()
	}
}

// closeAllConns shuts down every currently-open server-side conn so
// the next client read sees EOF. Used to simulate a Redis restart or
// a load-balancer drain.
func (fr *fakeRedis) closeAllConns() {
	fr.mu.Lock()
	conns := fr.conns
	fr.conns = nil
	fr.mu.Unlock()
	for _, c := range conns {
		_ = c.Close()
	}
}

// cmdCount reports how many commands of the given name have been
// dispatched. The name is matched case-insensitively.
func (fr *fakeRedis) cmdCount(name string) int {
	upper := strings.ToUpper(name)
	fr.mu.Lock()
	defer fr.mu.Unlock()
	n := 0
	for _, c := range fr.cmds {
		if c == upper {
			n++
		}
	}
	return n
}

// cmdLog returns the full ordered list of dispatched commands. Useful
// for assertion error messages.
func (fr *fakeRedis) cmdLog() []string {
	fr.mu.Lock()
	defer fr.mu.Unlock()
	out := make([]string, len(fr.cmds))
	copy(out, fr.cmds)
	return out
}

func (fr *fakeRedis) dispatch(w *bufio.Writer, args [][]byte) {
	cmd := strings.ToUpper(string(args[0]))
	switch cmd {
	case "PING":
		writeSimple(w, "PONG")
	case "AUTH":
		// Accept either AUTH <pwd> or AUTH <user> <pwd>.
		var pass string
		switch len(args) {
		case 2:
			pass = string(args[1])
		case 3:
			pass = string(args[2])
		default:
			writeError(w, "wrong number of args")
			return
		}
		if fr.authPass != "" && pass != fr.authPass {
			writeError(w, "WRONGPASS")
			return
		}
		writeSimple(w, "OK")
	case "SELECT":
		writeSimple(w, "OK")
	case "GET":
		fr.mu.Lock()
		e, ok := fr.data[string(args[1])]
		expired := ok && !e.expiresAt.IsZero() && time.Now().After(e.expiresAt)
		if expired {
			delete(fr.data, string(args[1]))
			ok = false
		}
		fr.mu.Unlock()
		if !ok {
			writeNilBulk(w)
		} else {
			writeBulk(w, e.value)
		}
	case "SET":
		// SET <key> <value> [PX <ms>]
		key, val := string(args[1]), args[2]
		var ttl time.Duration
		for i := 3; i+1 < len(args); i += 2 {
			switch strings.ToUpper(string(args[i])) {
			case "PX":
				n, _ := strconv.Atoi(string(args[i+1]))
				ttl = time.Duration(n) * time.Millisecond
			case "EX":
				n, _ := strconv.Atoi(string(args[i+1]))
				ttl = time.Duration(n) * time.Second
			}
		}
		entry := fakeEntry{value: append([]byte(nil), val...)}
		if ttl > 0 {
			entry.expiresAt = time.Now().Add(ttl)
		}
		fr.mu.Lock()
		fr.data[key] = entry
		fr.mu.Unlock()
		writeSimple(w, "OK")
	case "DEL":
		n := 0
		fr.mu.Lock()
		for _, k := range args[1:] {
			if _, ok := fr.data[string(k)]; ok {
				delete(fr.data, string(k))
				n++
			}
		}
		fr.mu.Unlock()
		writeInt(w, int64(n))
	case "EXISTS":
		fr.mu.Lock()
		_, ok := fr.data[string(args[1])]
		fr.mu.Unlock()
		if ok {
			writeInt(w, 1)
		} else {
			writeInt(w, 0)
		}
	case "PTTL":
		fr.mu.Lock()
		e, ok := fr.data[string(args[1])]
		fr.mu.Unlock()
		switch {
		case !ok:
			writeInt(w, -2)
		case e.expiresAt.IsZero():
			writeInt(w, -1)
		default:
			ms := time.Until(e.expiresAt).Milliseconds()
			if ms < 0 {
				ms = 0
			}
			writeInt(w, ms)
		}
	case "MGET":
		fmt.Fprintf(w, "*%d\r\n", len(args)-1)
		fr.mu.Lock()
		for _, k := range args[1:] {
			e, ok := fr.data[string(k)]
			if !ok {
				writeNilBulk(w)
				continue
			}
			writeBulk(w, e.value)
		}
		fr.mu.Unlock()
	case "MSET":
		fr.mu.Lock()
		for i := 1; i+1 < len(args); i += 2 {
			fr.data[string(args[i])] = fakeEntry{value: append([]byte(nil), args[i+1]...)}
		}
		fr.mu.Unlock()
		writeSimple(w, "OK")
	default:
		writeError(w, "unknown command "+cmd)
	}
}

// readCmd parses one RESP array of bulk strings (the client always
// sends this form).
func readCmd(r *bufio.Reader) ([][]byte, error) {
	first, err := r.ReadByte()
	if err != nil {
		return nil, err
	}
	if first != '*' {
		return nil, fmt.Errorf("want '*', got %c", first)
	}
	line, err := readLine(r)
	if err != nil {
		return nil, err
	}
	n, err := strconv.Atoi(string(line))
	if err != nil {
		return nil, err
	}
	args := make([][]byte, n)
	for i := 0; i < n; i++ {
		head, err := r.ReadByte()
		if err != nil {
			return nil, err
		}
		if head != '$' {
			return nil, fmt.Errorf("want '$', got %c", head)
		}
		ln, err := readLine(r)
		if err != nil {
			return nil, err
		}
		bn, err := strconv.Atoi(string(ln))
		if err != nil {
			return nil, err
		}
		buf := make([]byte, bn+2)
		if _, err := io.ReadFull(r, buf); err != nil {
			return nil, err
		}
		args[i] = buf[:bn]
	}
	return args, nil
}

func readLine(r *bufio.Reader) ([]byte, error) {
	b, err := r.ReadBytes('\n')
	if err != nil {
		return nil, err
	}
	if len(b) < 2 {
		return nil, fmt.Errorf("short line")
	}
	return b[:len(b)-2], nil
}

func writeSimple(w *bufio.Writer, s string) { fmt.Fprintf(w, "+%s\r\n", s) }
func writeError(w *bufio.Writer, s string)  { fmt.Fprintf(w, "-%s\r\n", s) }
func writeInt(w *bufio.Writer, n int64)     { fmt.Fprintf(w, ":%d\r\n", n) }
func writeNilBulk(w *bufio.Writer)          { _, _ = w.WriteString("$-1\r\n") }
func writeBulk(w *bufio.Writer, b []byte) {
	fmt.Fprintf(w, "$%d\r\n", len(b))
	_, _ = w.Write(b)
	_, _ = w.WriteString("\r\n")
}

// --- Cache integration tests ---------------------------------------

func newRedis(t *testing.T, opts redis.Options) *redis.Cache {
	t.Helper()
	fr := newFakeRedis(t)
	opts.Addr = fr.addr()
	if opts.DialTimeout == 0 {
		opts.DialTimeout = time.Second
	}
	c := redis.New(opts)
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func TestRedisRoundTrip(t *testing.T) {
	c := newRedis(t, redis.Options{})
	ctx := context.Background()

	if err := c.Ping(ctx); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if err := c.Set(ctx, "k", []byte("v"), 0); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got, err := c.Get(ctx, "k")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(got) != "v" {
		t.Errorf("Get = %q, want v", got)
	}
}

func TestRedisGetMissingReturnsErrNotFound(t *testing.T) {
	c := newRedis(t, redis.Options{})
	_, err := c.Get(context.Background(), "missing")
	if !errors.Is(err, cache.ErrNotFound) {
		t.Errorf("got %v, want ErrNotFound", err)
	}
}

func TestRedisTTLAndExistsAndDel(t *testing.T) {
	c := newRedis(t, redis.Options{})
	ctx := context.Background()
	_ = c.Set(ctx, "k", []byte("v"), 250*time.Millisecond)

	ok, err := c.Exists(ctx, "k")
	if err != nil || !ok {
		t.Errorf("Exists = %v, %v", ok, err)
	}
	d, err := c.TTL(ctx, "k")
	if err != nil {
		t.Fatal(err)
	}
	if d <= 0 || d > 250*time.Millisecond {
		t.Errorf("TTL = %s", d)
	}
	n, err := c.Delete(ctx, "k", "missing")
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("Delete = %d, want 1", n)
	}
}

func TestRedisGetMultiAndSetMulti(t *testing.T) {
	c := newRedis(t, redis.Options{})
	ctx := context.Background()
	_ = c.SetMulti(ctx, map[string][]byte{"a": []byte("1"), "b": []byte("2")}, 0)
	got, err := c.GetMulti(ctx, "a", "b", "missing")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || string(got["a"]) != "1" || string(got["b"]) != "2" {
		t.Errorf("GetMulti = %v", got)
	}
}

func TestRedisKeyPrefix(t *testing.T) {
	fr := newFakeRedis(t)
	c := redis.New(redis.Options{Addr: fr.addr(), KeyPrefix: "app:"})
	defer c.Close()
	ctx := context.Background()
	_ = c.Set(ctx, "k", []byte("v"), 0)
	fr.mu.Lock()
	_, ok := fr.data["app:k"]
	fr.mu.Unlock()
	if !ok {
		t.Errorf("prefix not applied; keys = %v", fr.keys())
	}
}

func (fr *fakeRedis) keys() []string {
	fr.mu.Lock()
	defer fr.mu.Unlock()
	out := make([]string, 0, len(fr.data))
	for k := range fr.data {
		out = append(out, k)
	}
	return out
}

func TestRedisHookFires(t *testing.T) {
	var (
		mu     sync.Mutex
		events []drops.QueryEvent
	)
	c := newRedis(t, redis.Options{Hook: func(_ context.Context, e drops.QueryEvent) {
		mu.Lock()
		defer mu.Unlock()
		events = append(events, e)
	}})
	ctx := context.Background()
	_ = c.Set(ctx, "k", []byte("v"), 0)
	_, _ = c.Get(ctx, "k")
	_ = c.Ping(ctx)

	mu.Lock()
	defer mu.Unlock()
	if len(events) != 3 {
		t.Fatalf("got %d events: %+v", len(events), events)
	}
	want := []string{"cache.set", "cache.get", "cache.ping"}
	for i, e := range events {
		if e.Kind != want[i] {
			t.Errorf("event[%d] kind = %q, want %q", i, e.Kind, want[i])
		}
		if e.Duration <= 0 {
			t.Errorf("event[%d] zero duration", i)
		}
	}
}

func TestRedisPoolReusesConnections(t *testing.T) {
	c := newRedis(t, redis.Options{MaxConns: 2})
	ctx := context.Background()
	// 100 sequential ops on MaxConns=2 must succeed; the pool should
	// recycle connections rather than open a new one per call.
	for i := 0; i < 100; i++ {
		if err := c.Set(ctx, "k", []byte("v"), 0); err != nil {
			t.Fatalf("Set %d: %v", i, err)
		}
		if _, err := c.Get(ctx, "k"); err != nil {
			t.Fatalf("Get %d: %v", i, err)
		}
	}
}

func TestRedisCloseRejectsSubsequentCalls(t *testing.T) {
	c := newRedis(t, redis.Options{})
	_ = c.Close()
	if err := c.Ping(context.Background()); !errors.Is(err, cache.ErrClosed) {
		t.Errorf("Ping after Close: %v", err)
	}
	if _, err := c.Get(context.Background(), "k"); !errors.Is(err, cache.ErrClosed) {
		t.Errorf("Get after Close: %v", err)
	}
}

func TestRedisAuthFailure(t *testing.T) {
	fr := newFakeRedis(t)
	fr.authPass = "correct"
	c := redis.New(redis.Options{Addr: fr.addr(), Password: "wrong"})
	defer c.Close()
	if err := c.Ping(context.Background()); err == nil {
		t.Fatal("expected AUTH failure")
	}
}

func TestRedisAuthSuccess(t *testing.T) {
	fr := newFakeRedis(t)
	fr.authPass = "secret"
	c := redis.New(redis.Options{Addr: fr.addr(), Password: "secret"})
	defer c.Close()
	if err := c.Ping(context.Background()); err != nil {
		t.Errorf("Ping with correct password: %v", err)
	}
}

func TestRedisEmptyKeyRejected(t *testing.T) {
	c := newRedis(t, redis.Options{})
	if err := c.Set(context.Background(), "", []byte("v"), 0); !errors.Is(err, cache.ErrInvalidKey) {
		t.Errorf("empty Set: %v", err)
	}
}
