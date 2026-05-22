package qdrant

import "context"

// SearchRequest is the body for /collections/{name}/points/search.
type SearchRequest struct {
	Vector         []float32      `json:"vector"`
	Limit          int            `json:"limit"`
	Offset         int            `json:"offset,omitempty"`
	Filter         *Filter        `json:"filter,omitempty"`
	ScoreThreshold *float32       `json:"score_threshold,omitempty"`
	WithVector     bool           `json:"with_vector,omitempty"`
	WithPayload    bool           `json:"with_payload,omitempty"`
	Params         map[string]any `json:"params,omitempty"` // hnsw_ef, exact, etc.
}

// Hit is one search result.
type Hit struct {
	ID      any            `json:"id"`
	Score   float32        `json:"score"`
	Vector  []float32      `json:"vector,omitempty"`
	Payload map[string]any `json:"payload,omitempty"`
}

// Search performs a single-vector similarity search.
func (c *Client) Search(ctx context.Context, collection string, req SearchRequest) ([]Hit, error) {
	if req.Limit <= 0 {
		req.Limit = 10
	}
	var out []Hit
	if err := c.Do(ctx, "POST", "/collections/"+collection+"/points/search", req, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// RecommendRequest is the body for /collections/{name}/points/recommend.
type RecommendRequest struct {
	Positive       []any          `json:"positive,omitempty"`
	Negative       []any          `json:"negative,omitempty"`
	Limit          int            `json:"limit"`
	Offset         int            `json:"offset,omitempty"`
	Filter         *Filter        `json:"filter,omitempty"`
	ScoreThreshold *float32       `json:"score_threshold,omitempty"`
	WithVector     bool           `json:"with_vector,omitempty"`
	WithPayload    bool           `json:"with_payload,omitempty"`
	Params         map[string]any `json:"params,omitempty"`
	Strategy       string         `json:"strategy,omitempty"` // "average_vector" | "best_score"
}

// Recommend returns points similar to the positive examples and
// dissimilar to the negative examples.
func (c *Client) Recommend(ctx context.Context, collection string, req RecommendRequest) ([]Hit, error) {
	if req.Limit <= 0 {
		req.Limit = 10
	}
	var out []Hit
	if err := c.Do(ctx, "POST", "/collections/"+collection+"/points/recommend", req, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// ScrollRequest is the body for /collections/{name}/points/scroll.
type ScrollRequest struct {
	Filter      *Filter `json:"filter,omitempty"`
	Limit       int     `json:"limit,omitempty"`
	Offset      any     `json:"offset,omitempty"` // page cursor; pass last page's NextPageOffset
	WithVector  bool    `json:"with_vector,omitempty"`
	WithPayload bool    `json:"with_payload,omitempty"`
}

// ScrollPage is the response from Scroll.
type ScrollPage struct {
	Points         []Point `json:"points"`
	NextPageOffset any     `json:"next_page_offset"`
}

// Scroll iterates through a collection in deterministic order,
// optionally filtered. Pass the previous response's NextPageOffset as
// the next call's Offset until it comes back nil.
func (c *Client) Scroll(ctx context.Context, collection string, req ScrollRequest) (*ScrollPage, error) {
	if req.Limit <= 0 {
		req.Limit = 100
	}
	var out ScrollPage
	if err := c.Do(ctx, "POST", "/collections/"+collection+"/points/scroll", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
