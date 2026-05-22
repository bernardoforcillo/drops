package qdrant

import (
	"context"
	"errors"
)

// Point is a single vector + payload + ID record.
//
// ID may be an int (uint64) or a string (UUID); Qdrant accepts either
// and the package passes the value through to JSON as-is. Vector is
// the dense embedding; for named-vector collections use NamedVectors
// instead via a custom payload type.
type Point struct {
	ID      any            `json:"id"`
	Vector  []float32      `json:"vector"`
	Payload map[string]any `json:"payload,omitempty"`
}

// WriteOptions tune the consistency of write operations. The default
// (zero value) is "fire and forget" — return as soon as Qdrant has
// queued the operation. Wait=true blocks until the data is durable
// in the local segments.
type WriteOptions struct {
	Wait bool
	// Ordering hint; "weak" (default), "medium", "strong".
	Ordering string
}

// Upsert inserts or replaces a batch of points. Empty batches return
// ErrNoPoints rather than issuing a no-op HTTP call.
func (c *Client) Upsert(ctx context.Context, collection string, points []Point, opts ...WriteOptions) error {
	if len(points) == 0 {
		return ErrNoPoints
	}
	body := struct {
		Points []Point `json:"points"`
	}{Points: points}
	return c.Do(ctx, "PUT", "/collections/"+collection+"/points"+writeQuery(opts), body, nil)
}

// ErrNoPoints is returned by Upsert / DeleteByIDs when called with an
// empty batch.
var ErrNoPoints = errors.New("qdrant: no points supplied")

// DeleteByIDs deletes the listed points.
func (c *Client) DeleteByIDs(ctx context.Context, collection string, ids []any, opts ...WriteOptions) error {
	if len(ids) == 0 {
		return ErrNoPoints
	}
	body := struct {
		Points []any `json:"points"`
	}{Points: ids}
	return c.Do(ctx, "POST", "/collections/"+collection+"/points/delete"+writeQuery(opts), body, nil)
}

// DeleteByFilter deletes every point that matches the filter.
func (c *Client) DeleteByFilter(ctx context.Context, collection string, f *Filter, opts ...WriteOptions) error {
	if f == nil {
		return errors.New("qdrant: DeleteByFilter requires a non-nil filter")
	}
	body := struct {
		Filter *Filter `json:"filter"`
	}{Filter: f}
	return c.Do(ctx, "POST", "/collections/"+collection+"/points/delete"+writeQuery(opts), body, nil)
}

// RetrieveOptions configures Retrieve.
type RetrieveOptions struct {
	WithVector  bool
	WithPayload bool
}

// Retrieve fetches the points with the given IDs.
func (c *Client) Retrieve(ctx context.Context, collection string, ids []any, opts ...RetrieveOptions) ([]Point, error) {
	if len(ids) == 0 {
		return nil, ErrNoPoints
	}
	var o RetrieveOptions
	if len(opts) > 0 {
		o = opts[0]
	}
	body := struct {
		IDs         []any `json:"ids"`
		WithVector  bool  `json:"with_vector,omitempty"`
		WithPayload bool  `json:"with_payload,omitempty"`
	}{IDs: ids, WithVector: o.WithVector, WithPayload: o.WithPayload}
	var out []Point
	if err := c.Do(ctx, "POST", "/collections/"+collection+"/points", body, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// Count returns the number of points matching an optional filter.
// Pass nil for a full count.
func (c *Client) Count(ctx context.Context, collection string, f *Filter, exact bool) (int64, error) {
	body := struct {
		Filter *Filter `json:"filter,omitempty"`
		Exact  bool    `json:"exact"`
	}{Filter: f, Exact: exact}
	var out struct {
		Count int64 `json:"count"`
	}
	if err := c.Do(ctx, "POST", "/collections/"+collection+"/points/count", body, &out); err != nil {
		return 0, err
	}
	return out.Count, nil
}

// writeQuery turns WriteOptions into a query string (?wait=true&ordering=…).
func writeQuery(opts []WriteOptions) string {
	if len(opts) == 0 {
		return ""
	}
	o := opts[0]
	parts := []string{}
	if o.Wait {
		parts = append(parts, "wait=true")
	}
	if o.Ordering != "" {
		parts = append(parts, "ordering="+o.Ordering)
	}
	if len(parts) == 0 {
		return ""
	}
	q := "?"
	for i, p := range parts {
		if i > 0 {
			q += "&"
		}
		q += p
	}
	return q
}
