package cloudflare_test

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/bernardoforcillo/drops"
	"github.com/bernardoforcillo/drops/cloudflare"
)

func ExampleNew() {
	cf, err := cloudflare.New("your-account-id",
		cloudflare.WithAPIToken("your-api-token"),
		cloudflare.WithTimeout(10*time.Second),
		cloudflare.WithHook(drops.LoggerHook(nil)),
	)
	if err != nil {
		log.Fatal(err)
	}
	// One client backs every Cloudflare package: d1.New(cf, …),
	// vectorize.New(cf, …), cloudflarekv.New(cf, …).
	_ = cf
}

// The envelope is why this package exists rather than four copies of
// net/http boilerplate: Cloudflare answers "success": false with HTTP
// 200 often enough that a status check alone lets a failure through.
func ExampleAPIError() {
	var cf *cloudflare.Client
	ctx := context.Background()

	err := cf.Do(ctx, cloudflare.Request{
		Method: http.MethodGet,
		Path:   cf.AccountPath("/d1/database/", "some-database-id"),
	}, nil)

	var apiErr *cloudflare.APIError
	switch {
	case errors.As(err, &apiErr) && apiErr.HasCode(cloudflare.CodeAuthorization):
		fmt.Println("the token is valid but lacks D1:Read on this account")
	case errors.Is(err, cloudflare.ErrNotFound):
		fmt.Println("no database with that ID")
	case errors.Is(err, cloudflare.ErrRateLimited):
		fmt.Println("throttled, and the retries gave up")
	case err != nil:
		log.Fatal(err)
	}
}

// Retries are jittered because a Worker that fans out and hits a rate
// limit retries from every colo at once; a fixed backoff has them all
// come back at the same instant and hit it again.
func ExampleWithRetryPolicy() {
	policy := cloudflare.RetryPolicy{
		MaxAttempts:       5,
		Backoff:           cloudflare.ExponentialBackoff(200*time.Millisecond, 10*time.Second),
		RespectRetryAfter: true,
		MaxRetryAfter:     30 * time.Second,
		RetryOn:           cloudflare.RetryableStatus,
	}
	cf, err := cloudflare.New("acct",
		cloudflare.WithAPIToken("tok"),
		cloudflare.WithRetryPolicy(policy),
	)
	if err != nil {
		log.Fatal(err)
	}
	_ = cf
}

// A read that Cloudflare models as a POST is safe to repeat, and has
// to say so: the method-based inference is wrong in both directions
// on this API.
func ExampleIdempotently() {
	var cf *cloudflare.Client
	ctx := context.Background()

	var out struct {
		Matches []any `json:"matches"`
	}
	err := cf.Do(ctx, cloudflare.Request{
		Method:     http.MethodPost,
		Path:       cf.AccountPath("/vectorize/v2/indexes", "emb") + "/query",
		Body:       map[string]any{"vector": []float32{1, 0}, "topK": 5},
		Idempotent: cloudflare.Idempotently(),
	}, &out)
	_ = err
}
