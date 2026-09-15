package cloudflarekv_test

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/bernardoforcillo/drops/cache"
	"github.com/bernardoforcillo/drops/cache/cloudflarekv"
	"github.com/bernardoforcillo/drops/cloudflare"
)

func ExampleNew() {
	cf, err := cloudflare.New("your-account-id", cloudflare.WithAPIToken("your-api-token"))
	if err != nil {
		log.Fatal(err)
	}
	c, err := cloudflarekv.New(cf, "your-namespace-id",
		cloudflarekv.WithKeyPrefix("embeddings:"))
	if err != nil {
		log.Fatal(err)
	}
	// Close marks the cache closed; there is no connection to
	// release, since every operation is an HTTPS request.
	_ = c.Close()
}

// What KV is good for: something expensive to compute and safe to
// serve slightly stale. A write takes up to a minute to be visible
// everywhere, so this is not the place for a cache that a write has
// to invalidate.
func ExampleCache_Get() {
	var c *cloudflarekv.Cache
	ctx := context.Background()

	key := "sha256:" + "…"
	cached, err := c.Get(ctx, key)
	switch {
	case err == nil:
		fmt.Printf("hit: %d bytes\n", len(cached))
	case errors.Is(err, cache.ErrNotFound):
		fresh := embed()
		// An hour is comfortably above KV's sixty-second floor.
		if err := c.Set(ctx, key, fresh, time.Hour); err != nil {
			log.Fatal(err)
		}
	default:
		log.Fatal(err)
	}
}

// A TTL below the floor is refused rather than rounded, because an
// entry given sixty seconds when it asked for five is served stale
// for fifty-five — and that bug surfaces far from the Set that caused
// it.
func ExampleWithRoundUp() {
	var c *cloudflarekv.Cache
	ctx := context.Background()

	err := c.Set(ctx, "k", []byte("v"), 5*time.Second)
	if errors.Is(err, cloudflarekv.ErrTTLTooShort) {
		// Either raise the TTL past cloudflarekv.MinTTL, or say
		// explicitly that the extra staleness is acceptable:
		//
		//	c, _ := cloudflarekv.New(cf, ns, cloudflarekv.WithRoundUp())
		fmt.Println("KV cannot expire anything faster than a minute")
	}
}

// The bulk read is a real batch rather than a loop, which is why this
// backend satisfies cache.MultiCache at all.
func ExampleCache_GetMulti() {
	var c *cloudflarekv.Cache
	ctx := context.Background()

	found, err := c.GetMulti(ctx, "a", "b", "c")
	if err != nil {
		log.Fatal(err)
	}
	// Keys that were not there are simply absent from the map.
	for k, v := range found {
		fmt.Printf("%s: %d bytes\n", k, len(v))
	}
}

func embed() []byte { return []byte("…") }
