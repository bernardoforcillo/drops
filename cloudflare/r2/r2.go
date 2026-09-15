package r2

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/bernardoforcillo/drops/cloudflare"
)

// Sentinel errors.
var (
	// ErrNoBucketName is returned by an operation given an empty
	// bucket name.
	ErrNoBucketName = errors.New("drops/cloudflare/r2: bucket name is empty")

	// ErrNoKey is returned by an object operation given an empty
	// key.
	ErrNoKey = errors.New("drops/cloudflare/r2: object key is empty")

	// ErrObjectTooLarge is returned before sending an object larger
	// than [MaxObjectSize]. See the package comment: the REST
	// endpoint has no multipart upload, so the limit is not one a
	// client can work around.
	ErrObjectTooLarge = errors.New("drops/cloudflare/r2: object exceeds what this API accepts")

	// ErrUnknownLocationHint is returned for a location hint R2 does
	// not define.
	ErrUnknownLocationHint = errors.New("drops/cloudflare/r2: unknown location hint")

	// ErrUnknownStorageClass is returned for a storage class R2 does
	// not define.
	ErrUnknownStorageClass = errors.New("drops/cloudflare/r2: unknown storage class")
)

// MaxObjectSize is the largest object Cloudflare's REST API accepts
// in one request. Larger objects need R2's S3-compatible API and its
// multipart upload.
const MaxObjectSize = 300 << 20 // 300 MB

// StorageClass is how R2 prices and serves an object.
type StorageClass string

// The storage classes R2 defines.
const (
	// Standard is the default: immediate access, no retrieval
	// charge.
	Standard StorageClass = "Standard"

	// InfrequentAccess is cheaper to keep and charged to read, for
	// the object written once and fetched almost never — which is
	// exactly what a database backup is.
	InfrequentAccess StorageClass = "InfrequentAccess"
)

// Valid reports whether c is a storage class R2 defines.
func (c StorageClass) Valid() bool {
	switch c {
	case Standard, InfrequentAccess:
		return true
	default:
		return false
	}
}

// LocationHint asks R2 to create a bucket near a part of the world.
type LocationHint string

// The location hints R2 defines.
const (
	LocationWesternNorthAmerica LocationHint = "wnam"
	LocationEasternNorthAmerica LocationHint = "enam"
	LocationWesternEurope       LocationHint = "weur"
	LocationEasternEurope       LocationHint = "eeur"
	LocationAsiaPacific         LocationHint = "apac"
	LocationOceania             LocationHint = "oc"
)

// Valid reports whether h is a location hint R2 defines.
func (h LocationHint) Valid() bool {
	switch h {
	case LocationWesternNorthAmerica, LocationEasternNorthAmerica,
		LocationWesternEurope, LocationEasternEurope,
		LocationAsiaPacific, LocationOceania:
		return true
	default:
		return false
	}
}

// Jurisdiction restricts where a bucket's objects may be stored.
//
// It is not a header a caller sets per request by accident: a bucket
// created under a jurisdiction can only be addressed with the same
// one, which is why it lives on the [Client] rather than on each
// call.
type Jurisdiction string

// The jurisdictions R2 defines.
const (
	JurisdictionDefault Jurisdiction = "default"
	JurisdictionEU      Jurisdiction = "eu"
	JurisdictionFedRAMP Jurisdiction = "fedramp"
)

// Client is R2 for one Cloudflare account.
type Client struct {
	cf           *cloudflare.Client
	jurisdiction Jurisdiction
}

// Option configures a [Client].
type Option func(*Client)

// WithJurisdiction scopes every request to a data-residency
// jurisdiction.
//
// A bucket created under one is invisible without it — not an error,
// simply absent — so this is set once, on the client, rather than
// per call where half the calls would forget it.
func WithJurisdiction(j Jurisdiction) Option {
	return func(c *Client) { c.jurisdiction = j }
}

// New returns an R2 client for the account cf addresses.
//
// The token needs "Workers R2 Storage:Edit" to write and
// "Workers R2 Storage:Read" to read.
func New(cf *cloudflare.Client, opts ...Option) *Client {
	c := &Client{cf: cf}
	for _, o := range opts {
		o(c)
	}
	return c
}

// Client returns the underlying Cloudflare API client.
func (c *Client) Client() *cloudflare.Client { return c.cf }

// Bucket returns a handle for one bucket's objects.
func (c *Client) Bucket(name string) *Bucket {
	return &Bucket{c: c, name: name}
}

// headers returns the headers every request carries, which today is
// the jurisdiction when one is set.
func (c *Client) headers() http.Header {
	if c.jurisdiction == "" {
		return nil
	}
	h := http.Header{}
	h.Set("cf-r2-jurisdiction", string(c.jurisdiction))
	return h
}

// BucketInfo is one R2 bucket as the API describes it.
type BucketInfo struct {
	// Name is the bucket's name, unique within the account.
	Name string `json:"name"`

	// CreationDate is when it was created, as R2 reports it. It is
	// a string rather than a time.Time because R2 has shipped more
	// than one format for it and a parse failure here would fail a
	// listing that is otherwise perfectly good; [BucketInfo.Created]
	// is the parsed reading for callers who want one.
	CreationDate string `json:"creation_date"`

	// Location is the region R2 placed it in.
	Location LocationHint `json:"location"`

	// StorageClass is the default class for objects written to it.
	StorageClass StorageClass `json:"storage_class"`

	// Jurisdiction is the data-residency rule it lives under.
	Jurisdiction Jurisdiction `json:"jurisdiction,omitempty"`
}

// Created parses [BucketInfo.CreationDate], and reports whether it
// could.
func (b BucketInfo) Created() (time.Time, bool) {
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05.000Z"} {
		if t, err := time.Parse(layout, b.CreationDate); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// CreateBucketOptions describes a bucket to create.
type CreateBucketOptions struct {
	// Location asks for the bucket to be placed near a region.
	Location LocationHint

	// StorageClass is the default class for objects written to it.
	StorageClass StorageClass
}

// CreateBucket makes a bucket.
func (c *Client) CreateBucket(ctx context.Context, name string, opts CreateBucketOptions) (*BucketInfo, error) {
	if name == "" {
		return nil, ErrNoBucketName
	}
	body := map[string]any{"name": name}
	if opts.Location != "" {
		if !opts.Location.Valid() {
			return nil, fmt.Errorf("%w: %q", ErrUnknownLocationHint, opts.Location)
		}
		body["locationHint"] = string(opts.Location)
	}
	if opts.StorageClass != "" {
		if !opts.StorageClass.Valid() {
			return nil, fmt.Errorf("%w: %q", ErrUnknownStorageClass, opts.StorageClass)
		}
		body["storageClass"] = string(opts.StorageClass)
	}
	var out BucketInfo
	err := c.cf.Do(ctx, cloudflare.Request{
		Method: http.MethodPost,
		Path:   c.cf.AccountPath("/r2/buckets"),
		Body:   body,
		Header: c.headers(),
	}, &out)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// GetBucket returns one bucket's metadata.
func (c *Client) GetBucket(ctx context.Context, name string) (*BucketInfo, error) {
	if name == "" {
		return nil, ErrNoBucketName
	}
	var out BucketInfo
	err := c.cf.Do(ctx, cloudflare.Request{
		Method: http.MethodGet,
		Path:   c.cf.AccountPath("/r2/buckets/", name),
		Header: c.headers(),
	}, &out)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// DeleteBucket removes a bucket. R2 refuses to delete one that still
// holds objects.
func (c *Client) DeleteBucket(ctx context.Context, name string) error {
	if name == "" {
		return ErrNoBucketName
	}
	return c.cf.Do(ctx, cloudflare.Request{
		Method: http.MethodDelete,
		Path:   c.cf.AccountPath("/r2/buckets/", name),
		Header: c.headers(),
	}, nil)
}

// maxPages caps how far a listing will walk, so a cursor that never
// advances cannot spin forever.
const maxPages = 1000

// ListBuckets returns the account's buckets, walking every page.
//
// nameContains narrows the search; pass "" for all of them.
func (c *Client) ListBuckets(ctx context.Context, nameContains string) ([]BucketInfo, error) {
	var out []BucketInfo
	cursor := ""
	for page := 0; page < maxPages; page++ {
		q := url.Values{}
		q.Set("per_page", "1000")
		if nameContains != "" {
			q.Set("name_contains", nameContains)
		}
		if cursor != "" {
			q.Set("cursor", cursor)
		}
		var body struct {
			Buckets []BucketInfo `json:"buckets"`
		}
		env, err := c.cf.DoEnvelope(ctx, cloudflare.Request{
			Method: http.MethodGet,
			Path:   c.cf.AccountPath("/r2/buckets"),
			Query:  q,
			Header: c.headers(),
		})
		if err != nil {
			return nil, err
		}
		if err := decodeJSON(env.Result, &body); err != nil {
			return nil, err
		}
		out = append(out, body.Buckets...)

		next, err := nextCursor(env.ResultInfo)
		if err != nil {
			return nil, err
		}
		if next == "" || next == cursor || len(body.Buckets) == 0 {
			return out, nil
		}
		cursor = next
	}
	return out, fmt.Errorf("drops/cloudflare/r2: stopped listing buckets after %d pages", maxPages)
}

// objectPath builds the path for one object, keeping the slashes in
// the key as path separators while escaping everything else.
//
// That distinction is the whole point: a key is a path, so
// "d1/2026/dump.sql" must address three segments, while a key built
// from a tenant's name must not be able to address something else.
//
// It does not go through [cloudflare.Client.AccountPath] because
// url.PathEscape is not enough here. PathEscape leaves "." and ".."
// exactly as they are — they are legal path characters — so a key
// segment of ".." would travel as a dot segment, and anything
// between this process and R2 is entitled by RFC 3986 to resolve it
// away. A key of "../../secrets" would then address a different URL
// than the one it names. Encoding those two segments is what stops
// that, and it is why [escapeSegment] exists rather than a call to
// the shared helper.
func (c *Client) objectPath(bucket, key string) string {
	var b strings.Builder
	b.WriteString("/accounts/")
	b.WriteString(url.PathEscape(c.cf.AccountID()))
	b.WriteString("/r2/buckets/")
	b.WriteString(url.PathEscape(bucket))
	b.WriteString("/objects")
	for _, seg := range splitKey(key) {
		b.WriteString("/")
		b.WriteString(escapeSegment(seg))
	}
	return b.String()
}

// escapeSegment path-escapes one key segment, encoding the two
// segments url.PathEscape would leave as dot segments. See
// [Client.objectPath] for why.
//
// The encoded form still names the same key: percent-encoded dots are
// not dot segments, so R2 decodes "%2E%2E" back to the literal key
// ".." rather than to a step up the path.
func escapeSegment(seg string) string {
	switch seg {
	case ".":
		return "%2E"
	case "..":
		return "%2E%2E"
	default:
		return url.PathEscape(seg)
	}
}

// splitKey breaks a key into its path segments, dropping the empty
// ones a leading, trailing or doubled slash would produce.
func splitKey(key string) []string {
	raw := strings.Split(key, "/")
	out := make([]string, 0, len(raw))
	for _, p := range raw {
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// nextCursor reads the paging cursor out of an envelope's
// result_info, which R2 leaves absent on the last page.
func nextCursor(raw []byte) (string, error) {
	if len(raw) == 0 {
		return "", nil
	}
	var info struct {
		Cursor string `json:"cursor"`
	}
	if err := decodeJSON(raw, &info); err != nil {
		return "", err
	}
	return info.Cursor, nil
}

// parseSize reads a Content-Length-style header, answering -1 for one
// that is absent or unreadable rather than a misleading zero.
func parseSize(s string) int64 {
	if s == "" {
		return -1
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return -1
	}
	return n
}
