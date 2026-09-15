package r2

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/bernardoforcillo/drops/cloudflare"
)

// Bucket is one R2 bucket's objects.
//
// It is safe for concurrent use and holds nothing but the name, so
// keeping one per bucket beside a [Client] costs nothing.
type Bucket struct {
	c    *Client
	name string
}

// Name returns the bucket's name.
func (b *Bucket) Name() string { return b.name }

// Client returns the R2 client the bucket was made from.
func (b *Bucket) Client() *Client { return b.c }

// ObjectInfo is what R2 reports about a stored object.
type ObjectInfo struct {
	// Key is the object's name, slashes and all.
	Key string `json:"key"`

	// Size is its length in bytes, or -1 where R2 reported none.
	Size int64 `json:"size"`

	// ETag is the object's entity tag. R2 reports it unquoted in a
	// listing and quoted in a Get's header; this field is the
	// unquoted form either way, so the two can be compared.
	ETag string `json:"etag"`

	// LastModified is when it was last written.
	LastModified time.Time `json:"last_modified"`

	// StorageClass is the class it is stored under.
	StorageClass StorageClass `json:"storage_class"`

	// ContentType is the type recorded with it, when there is one.
	ContentType string `json:"-"`

	// HTTPMetadata carries the rest of what R2 records — the
	// content type it reports in a listing, and the cache and
	// disposition headers it will serve the object with.
	HTTPMetadata struct {
		ContentType        string `json:"contentType"`
		ContentDisposition string `json:"contentDisposition"`
		ContentEncoding    string `json:"contentEncoding"`
		ContentLanguage    string `json:"contentLanguage"`
		CacheControl       string `json:"cacheControl"`
	} `json:"http_metadata"`

	// CustomMetadata is the user metadata stored with the object.
	CustomMetadata map[string]string `json:"custom_metadata"`
}

// PutOptions describes how an object is written.
type PutOptions struct {
	// ContentType is recorded with the object and served back with
	// it. Empty means application/octet-stream.
	ContentType string

	// StorageClass overrides the bucket's default.
	// [InfrequentAccess] is the one to reach for on a backup: cheap
	// to keep, charged to read, and a backup is read approximately
	// never.
	StorageClass StorageClass
}

// header renders the options as request headers.
func (o PutOptions) header(base http.Header) (http.Header, error) {
	if o.StorageClass == "" {
		return base, nil
	}
	if !o.StorageClass.Valid() {
		return nil, fmt.Errorf("%w: %q", ErrUnknownStorageClass, o.StorageClass)
	}
	h := http.Header{}
	for k, v := range base {
		h[k] = v
	}
	h.Set("cf-r2-storage-class", string(o.StorageClass))
	return h, nil
}

// Put writes an object from bytes.
//
// It replaces whatever was under the key: R2 has no "create only"
// mode on this endpoint, so a caller that must not overwrite has to
// check first and accept the race, or use a key that cannot collide.
func (b *Bucket) Put(ctx context.Context, key string, data []byte, opts PutOptions) (*ObjectInfo, error) {
	if err := b.check(key, int64(len(data))); err != nil {
		return nil, err
	}
	return b.put(ctx, key, cloudflare.Request{Raw: data}, opts)
}

// PutString writes an object from a string.
func (b *Bucket) PutString(ctx context.Context, key, data string, opts PutOptions) (*ObjectInfo, error) {
	return b.Put(ctx, key, []byte(data), opts)
}

// PutFile writes an object from a file on disk, streaming it rather
// than holding it in memory.
//
// This is the one to use for a database dump: a
// [github.com/bernardoforcillo/drops/cloudflare/d1.Admin.ExportTo]
// into a temporary file, then this, and the dump never exists in the
// process's heap.
//
// The size is taken from the file, which is also what makes the
// [MaxObjectSize] check possible before a single byte is sent — the
// alternative is discovering the limit after uploading 300 MB.
func (b *Bucket) PutFile(ctx context.Context, key, path string, opts PutOptions) (*ObjectInfo, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("drops/cloudflare/r2: %w", err)
	}
	if err := b.check(key, info.Size()); err != nil {
		return nil, err
	}
	return b.put(ctx, key, cloudflare.Request{
		Stream:        func() (io.ReadCloser, error) { return os.Open(path) }, //nolint:gosec // the path is the caller's own argument.
		ContentLength: info.Size(),
	}, opts)
}

// PutStream writes an object from a reader the caller can re-open.
//
// open is called once per attempt, because a retry has to send the
// body again — see
// [github.com/bernardoforcillo/drops/cloudflare.Request.Stream].
// size is the body's length; pass -1 for unknown, which sends the
// request chunked and skips the [MaxObjectSize] check, so R2 rather
// than this package decides whether it was too big.
func (b *Bucket) PutStream(ctx context.Context, key string, open func() (io.ReadCloser, error), size int64, opts PutOptions) (*ObjectInfo, error) {
	if err := b.check(key, size); err != nil {
		return nil, err
	}
	length := size
	if length < 0 {
		length = 0
	}
	return b.put(ctx, key, cloudflare.Request{Stream: open, ContentLength: length}, opts)
}

// check refuses a key or a size the endpoint cannot take.
func (b *Bucket) check(key string, size int64) error {
	if b.name == "" {
		return ErrNoBucketName
	}
	if strings.TrimSpace(key) == "" {
		return ErrNoKey
	}
	if size > MaxObjectSize {
		return fmt.Errorf("%w: %d bytes exceeds the %d this endpoint takes — R2's S3-compatible API has multipart upload and this one does not",
			ErrObjectTooLarge, size, int64(MaxObjectSize))
	}
	return nil
}

// put sends one upload, filling in the parts every caller shares.
func (b *Bucket) put(ctx context.Context, key string, req cloudflare.Request, opts PutOptions) (*ObjectInfo, error) {
	header, err := opts.header(b.c.headers())
	if err != nil {
		return nil, err
	}
	req.Method = http.MethodPut
	req.Path = b.c.objectPath(b.name, key)
	req.Header = header
	req.ContentType = opts.ContentType
	if req.ContentType == "" {
		req.ContentType = "application/octet-stream"
	}

	var out ObjectInfo
	if err := b.c.cf.Do(ctx, req, &out); err != nil {
		return nil, err
	}
	if out.Key == "" {
		out.Key = key
	}
	out.ETag = strings.Trim(out.ETag, `"`)
	if out.ContentType == "" {
		out.ContentType = out.HTTPMetadata.ContentType
	}
	return &out, nil
}

// Object is a stored object being read.
//
// Body must be closed. Nothing else holds a connection open, so a
// forgotten one leaks one.
type Object struct {
	ObjectInfo

	// Body is the object's bytes.
	Body io.ReadCloser
}

// Get fetches an object.
//
// The whole body is already in memory by the time this returns: the
// shared client reads a response to completion so it can apply the
// retry policy to it, which is the right trade for objects the size
// this endpoint takes and the reason [Bucket.GetTo] exists for the
// ones where it is not.
func (b *Bucket) Get(ctx context.Context, key string) (*Object, error) {
	if err := b.check(key, 0); err != nil {
		return nil, err
	}
	resp, err := b.c.cf.DoRaw(ctx, cloudflare.Request{
		Method: http.MethodGet,
		Path:   b.c.objectPath(b.name, key),
		Accept: "application/octet-stream",
		Header: b.c.headers(),
	})
	if err != nil {
		return nil, err
	}
	if resp.Status < 200 || resp.Status >= 300 {
		return nil, b.statusError(http.MethodGet, key, resp)
	}
	obj := &Object{Body: io.NopCloser(bytes.NewReader(resp.Body))}
	obj.Key = key
	obj.ETag = strings.Trim(resp.Header.Get("ETag"), `"`)
	obj.ContentType = resp.Header.Get("Content-Type")
	obj.HTTPMetadata.ContentType = obj.ContentType
	obj.Size = parseSize(resp.Header.Get("Content-Length"))
	if obj.Size < 0 {
		obj.Size = int64(len(resp.Body))
	}
	if lm := resp.Header.Get("Last-Modified"); lm != "" {
		if t, parseErr := http.ParseTime(lm); parseErr == nil {
			obj.LastModified = t
		}
	}
	return obj, nil
}

// GetBytes fetches an object's contents.
func (b *Bucket) GetBytes(ctx context.Context, key string) ([]byte, error) {
	obj, err := b.Get(ctx, key)
	if err != nil {
		return nil, err
	}
	defer obj.Body.Close()
	return io.ReadAll(obj.Body)
}

// GetTo fetches an object and writes it to w.
func (b *Bucket) GetTo(ctx context.Context, key string, w io.Writer) (*ObjectInfo, error) {
	obj, err := b.Get(ctx, key)
	if err != nil {
		return nil, err
	}
	defer obj.Body.Close()
	if _, err := io.Copy(w, obj.Body); err != nil {
		return nil, fmt.Errorf("drops/cloudflare/r2: write object %q: %w", key, err)
	}
	info := obj.ObjectInfo
	return &info, nil
}

// Delete removes an object. Deleting a key that is not there
// succeeds, which is what makes a cleanup loop safe to re-run.
func (b *Bucket) Delete(ctx context.Context, key string) error {
	if err := b.check(key, 0); err != nil {
		return err
	}
	return b.c.cf.Do(ctx, cloudflare.Request{
		Method: http.MethodDelete,
		Path:   b.c.objectPath(b.name, key),
		Header: b.c.headers(),
	}, nil)
}

// Exists reports whether an object is there.
//
// It is a listing of one key rather than a HEAD, because the REST
// object API has no HEAD: a Get would fetch the body to answer a
// question about its existence, which for a 300 MB backup is an
// expensive way to say yes.
func (b *Bucket) Exists(ctx context.Context, key string) (bool, error) {
	if err := b.check(key, 0); err != nil {
		return false, err
	}
	objs, _, err := b.ListPage(ctx, ListOptions{Prefix: key, Limit: 1})
	if err != nil {
		return false, err
	}
	for _, o := range objs {
		if o.Key == key {
			return true, nil
		}
	}
	return false, nil
}

// ListOptions narrows a listing.
type ListOptions struct {
	// Prefix restricts the listing to keys starting with it.
	Prefix string

	// Delimiter groups keys sharing everything up to its first
	// occurrence after the prefix — "/" makes a listing read like a
	// directory.
	Delimiter string

	// StartAfter resumes from just past a key, lexicographically.
	StartAfter string

	// Limit caps how many keys one page returns.
	Limit int

	// Cursor continues a listing from where [Bucket.ListPage] left
	// off.
	Cursor string
}

// List returns every object matching opts, walking every page.
//
// A bucket can hold more keys than fit in memory, so this is for a
// bounded prefix — the day's backups, one tenant's documents.
// [Bucket.ListPage] is the one for a bucket whose size is not known.
func (b *Bucket) List(ctx context.Context, opts ListOptions) ([]ObjectInfo, error) {
	var out []ObjectInfo
	for page := 0; page < maxPages; page++ {
		batch, cursor, err := b.ListPage(ctx, opts)
		if err != nil {
			return nil, err
		}
		out = append(out, batch...)
		if cursor == "" || cursor == opts.Cursor || len(batch) == 0 {
			return out, nil
		}
		opts.Cursor = cursor
	}
	return out, fmt.Errorf("drops/cloudflare/r2: stopped listing %q after %d pages", b.name, maxPages)
}

// ListPage returns one page of objects and the cursor for the next,
// which is empty at the end of the listing.
func (b *Bucket) ListPage(ctx context.Context, opts ListOptions) ([]ObjectInfo, string, error) {
	if b.name == "" {
		return nil, "", ErrNoBucketName
	}
	q := url.Values{}
	if opts.Prefix != "" {
		q.Set("prefix", opts.Prefix)
	}
	if opts.Delimiter != "" {
		q.Set("delimiter", opts.Delimiter)
	}
	if opts.StartAfter != "" {
		q.Set("start_after", opts.StartAfter)
	}
	if opts.Limit > 0 {
		q.Set("per_page", fmt.Sprint(opts.Limit))
	}
	if opts.Cursor != "" {
		q.Set("cursor", opts.Cursor)
	}

	env, err := b.c.cf.DoEnvelope(ctx, cloudflare.Request{
		Method: http.MethodGet,
		Path:   b.c.cf.AccountPath("/r2/buckets", b.name, "objects"),
		Query:  q,
		Header: b.c.headers(),
	})
	if err != nil {
		return nil, "", err
	}
	var objs []ObjectInfo
	if err := decodeJSON(env.Result, &objs); err != nil {
		return nil, "", err
	}
	for i := range objs {
		objs[i].ETag = strings.Trim(objs[i].ETag, `"`)
		if objs[i].ContentType == "" {
			objs[i].ContentType = objs[i].HTTPMetadata.ContentType
		}
	}
	cursor, err := nextCursor(env.ResultInfo)
	if err != nil {
		return nil, "", err
	}
	return objs, cursor, nil
}

// statusError turns a non-2xx raw reply into the same *APIError shape
// the envelope path produces, so errors.Is against
// [github.com/bernardoforcillo/drops/cloudflare.ErrNotFound] answers
// on both paths.
func (b *Bucket) statusError(method, key string, resp *cloudflare.Response) error {
	path := b.c.objectPath(b.name, key)
	var env struct {
		Errors []cloudflare.Error `json:"errors"`
	}
	_ = json.Unmarshal(resp.Body, &env)
	return cloudflare.NewAPIError(method, path, resp, env.Errors)
}

// decodeJSON decodes raw into v with UseNumber, so an object's size
// past 2^53 — which R2 allows even where this endpoint does not —
// keeps its low bits.
func decodeJSON(raw json.RawMessage, v any) error {
	if len(raw) == 0 {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("drops/cloudflare/r2: decode result: %w", err)
	}
	return nil
}
