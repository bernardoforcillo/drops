package qdrant

import (
	"context"
	"errors"
	"fmt"
)

// Distance enumerates the supported similarity metrics. The values
// are the exact strings Qdrant expects in JSON.
type Distance string

const (
	DistanceCosine    Distance = "Cosine"
	DistanceEuclid    Distance = "Euclid"
	DistanceDot       Distance = "Dot"
	DistanceManhattan Distance = "Manhattan"
)

// VectorParams configures a single named vector or the (default)
// unnamed vector of a collection.
type VectorParams struct {
	Size     int      `json:"size"`
	Distance Distance `json:"distance"`
	// OnDisk stores the vector data on disk instead of in memory.
	OnDisk *bool `json:"on_disk,omitempty"`
}

// HNSWConfig is the HNSW-index tuning bag. Pointer fields let zero-
// valued integers reach Qdrant as "not set" rather than 0.
type HNSWConfig struct {
	M                *int  `json:"m,omitempty"`
	EFConstruct      *int  `json:"ef_construct,omitempty"`
	FullScanThreshold *int `json:"full_scan_threshold,omitempty"`
	OnDisk           *bool `json:"on_disk,omitempty"`
}

// CollectionConfig is the payload for CreateCollection.
type CollectionConfig struct {
	Vectors           VectorParams `json:"vectors"`
	ShardNumber       *int         `json:"shard_number,omitempty"`
	ReplicationFactor *int         `json:"replication_factor,omitempty"`
	WriteConsistency  *int         `json:"write_consistency_factor,omitempty"`
	OnDiskPayload     *bool        `json:"on_disk_payload,omitempty"`
	HNSW              *HNSWConfig  `json:"hnsw_config,omitempty"`
}

// CollectionInfo is the response from GET /collections/{name}.
type CollectionInfo struct {
	Status        string         `json:"status"`
	VectorsCount  int            `json:"vectors_count"`
	PointsCount   int            `json:"points_count"`
	SegmentsCount int            `json:"segments_count"`
	Config        map[string]any `json:"config"`
}

// CreateCollection creates a new collection. Returns nil if the
// operation succeeds (Qdrant responds with `{"result": true}`).
func (c *Client) CreateCollection(ctx context.Context, name string, cfg CollectionConfig) error {
	if name == "" {
		return errors.New("qdrant: collection name is empty")
	}
	if cfg.Vectors.Size <= 0 {
		return errors.New("qdrant: CollectionConfig.Vectors.Size must be > 0")
	}
	if cfg.Vectors.Distance == "" {
		cfg.Vectors.Distance = DistanceCosine
	}
	var ok bool
	if err := c.Do(ctx, "PUT", "/collections/"+name, cfg, &ok); err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("qdrant: CreateCollection %q returned false", name)
	}
	return nil
}

// DeleteCollection drops a collection. Idempotent on the server side.
func (c *Client) DeleteCollection(ctx context.Context, name string) error {
	var ok bool
	return c.Do(ctx, "DELETE", "/collections/"+name, nil, &ok)
}

// CollectionExists reports whether the collection exists.
func (c *Client) CollectionExists(ctx context.Context, name string) (bool, error) {
	if _, err := c.CollectionInfo(ctx, name); err != nil {
		if errors.Is(err, ErrCollectionMissing) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// CollectionInfo returns metadata about a collection.
func (c *Client) CollectionInfo(ctx context.Context, name string) (*CollectionInfo, error) {
	var info CollectionInfo
	if err := c.Do(ctx, "GET", "/collections/"+name, nil, &info); err != nil {
		return nil, err
	}
	return &info, nil
}

// ListCollections returns every collection's name.
func (c *Client) ListCollections(ctx context.Context) ([]string, error) {
	var out struct {
		Collections []struct {
			Name string `json:"name"`
		} `json:"collections"`
	}
	if err := c.Do(ctx, "GET", "/collections", nil, &out); err != nil {
		return nil, err
	}
	names := make([]string, len(out.Collections))
	for i, c := range out.Collections {
		names[i] = c.Name
	}
	return names, nil
}
