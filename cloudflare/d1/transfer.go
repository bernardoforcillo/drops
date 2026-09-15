package d1

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/bernardoforcillo/drops/cloudflare"
)

// Export and import errors.
var (
	// ErrExportFailed is returned when D1 reports the export job
	// failed. The message carries D1's own reason.
	ErrExportFailed = errors.New("drops/cloudflare/d1: export failed")

	// ErrImportFailed is returned when D1 reports the import job
	// failed.
	ErrImportFailed = errors.New("drops/cloudflare/d1: import failed")

	// ErrPollTimeout is returned when a job was still running when
	// [WithPollTimeout] ran out. The job is D1's and keeps going;
	// what timed out is the waiting.
	ErrPollTimeout = errors.New("drops/cloudflare/d1: gave up waiting for the job to finish")

	// ErrUploadRejected is returned when the presigned upload of an
	// import file did not succeed.
	ErrUploadRejected = errors.New("drops/cloudflare/d1: import upload was rejected")
)

// ExportOptions narrows what an export contains. The zero value
// exports the whole database, schema and data.
type ExportOptions struct {
	// NoData exports the table definitions without their contents —
	// the schema, as SQL.
	NoData bool

	// NoSchema exports the contents without the definitions.
	NoSchema bool

	// Tables limits the export to these tables. Empty means all of
	// them.
	Tables []string
}

// Export is a finished export job.
type Export struct {
	// Filename is the name D1 generated for the dump.
	Filename string `json:"filename"`

	// SignedURL downloads the SQL. It is valid for about an hour and
	// carries its own authorisation, so it is not a link to store —
	// [Admin.ExportTo] fetches it while it is fresh.
	SignedURL string `json:"signed_url"`

	// Bookmark is the Time Travel bookmark the export was taken at,
	// so the exact state the dump captures can be named later.
	Bookmark string `json:"-"`

	// Messages is D1's log of the job.
	Messages []string `json:"-"`
}

// transferResponse is the shape both the export and the import
// endpoints answer with: a job status, a bookmark to poll against,
// and a result that is only there once it is done.
type transferResponse struct {
	AtBookmark string   `json:"at_bookmark"`
	Error      string   `json:"error"`
	Messages   []string `json:"messages"`
	Status     string   `json:"status"`
	Success    bool     `json:"success"`
	Type       string   `json:"type"`

	// Init-only, for an import.
	Filename  string `json:"filename"`
	UploadURL string `json:"upload_url"`

	Result struct {
		// Export.
		Filename  string `json:"filename"`
		SignedURL string `json:"signed_url"`

		// Import.
		FinalBookmark string `json:"final_bookmark"`
		NumQueries    int64  `json:"num_queries"`
		Meta          Meta   `json:"meta"`
	} `json:"result"`
}

// Job statuses D1 reports. Anything that is neither is taken as
// "still running", which is the safe reading of a status this package
// has not seen: treating an unknown status as finished would report
// success for a job that had not produced anything.
const (
	statusComplete = "complete"
	statusError    = "error"
)

func (r transferResponse) done() bool   { return r.Status == statusComplete }
func (r transferResponse) failed() bool { return r.Status == statusError }

// Export takes a SQL dump of a database and returns where to fetch
// it.
//
// D1 runs the export as a job: the first call starts it and each one
// after asks whether it has finished. This method does the asking,
// on [WithPollInterval], until the job finishes, the context is done,
// or [WithPollTimeout] runs out.
//
// The result's [Export.SignedURL] is good for about an hour. Use
// [Admin.ExportTo] to take the bytes directly instead of handling the
// URL.
//
// Export is the backup; Time Travel is not. Time Travel reaches back
// thirty days at most and dies with the database, so anything that
// must outlive either belongs in a dump — which is also the only
// thing that moves data between accounts, or out of D1 entirely.
func (a *Admin) Export(ctx context.Context, databaseID string, opts ExportOptions) (*Export, error) {
	if databaseID == "" {
		return nil, ErrNoDatabaseID
	}
	dump := map[string]any{}
	if opts.NoData {
		dump["no_data"] = true
	}
	if opts.NoSchema {
		dump["no_schema"] = true
	}
	if len(opts.Tables) > 0 {
		dump["tables"] = opts.Tables
	}
	body := map[string]any{"output_format": "polling"}
	if len(dump) > 0 {
		body["dump_options"] = dump
	}

	path := a.cf.AccountPath("/d1/database/", databaseID) + "/export"
	resp, err := a.runJob(ctx, path, body, func(bookmark string) map[string]any {
		return map[string]any{"output_format": "polling", "current_bookmark": bookmark}
	})
	if err != nil {
		return nil, err
	}
	if resp.failed() {
		return nil, fmt.Errorf("%w: %s", ErrExportFailed, resp.Error)
	}
	return &Export{
		Filename:  resp.Result.Filename,
		SignedURL: resp.Result.SignedURL,
		Bookmark:  resp.AtBookmark,
		Messages:  resp.Messages,
	}, nil
}

// ExportTo runs an export and writes the SQL to w.
//
// It is the whole operation in one call, which is what a backup job
// wants: the signed URL expires, and a two-step API invites it being
// stored somewhere it will be stale by the time it is used.
func (a *Admin) ExportTo(ctx context.Context, databaseID string, w io.Writer, opts ExportOptions) (*Export, error) {
	exp, err := a.Export(ctx, databaseID, opts)
	if err != nil {
		return nil, err
	}
	if exp.SignedURL == "" {
		return nil, fmt.Errorf("%w: D1 reported the export complete but gave no download URL", ErrExportFailed)
	}
	// The signed URL carries its own authorisation, so this request
	// deliberately does not send the API token: it goes to R2, not
	// to the Cloudflare API, and a bearer token has no business
	// there.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, exp.SignedURL, nil)
	if err != nil {
		return nil, fmt.Errorf("drops/cloudflare/d1: build download request: %w", err)
	}
	resp, err := a.cf.HTTPClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("drops/cloudflare/d1: download export: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("%w: downloading the dump returned %d: %s",
			ErrExportFailed, resp.StatusCode, truncate(string(snippet), 200))
	}
	if _, err := io.Copy(w, resp.Body); err != nil {
		return nil, fmt.Errorf("drops/cloudflare/d1: write export: %w", err)
	}
	return exp, nil
}

// Imported is what a finished import reports.
type Imported struct {
	// FinalBookmark is the Time Travel bookmark for the state
	// directly after the import succeeded — the restore point that
	// undoes anything written after it.
	FinalBookmark string

	// NumQueries is how many statements the dump contained.
	NumQueries int64

	// Meta is what D1 reported about the ingestion.
	Meta Meta

	// Messages is D1's log of the job.
	Messages []string
}

// Import loads a SQL dump into a database.
//
// The dump is what [Admin.Export] produces, or what `wrangler d1
// export` writes, or any file of SQL statements. It is applied to the
// database as it stands — it does not replace it — so importing into
// a database that already holds those tables fails on the CREATE
// rather than silently merging.
//
// The upload is three round trips D1 requires and this method hides:
// an init that hashes the file so D1 can tell whether it already has
// it, a presigned upload to R2, and an ingest that is then polled to
// completion.
func (a *Admin) Import(ctx context.Context, databaseID string, sql []byte) (*Imported, error) {
	sum := md5.Sum(sql) //nolint:gosec // Cloudflare's import API specifies an MD5 etag; it is a content check, not a security boundary.
	return a.importFrom(ctx, databaseID, hex.EncodeToString(sum[:]), int64(len(sql)), func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(sql)), nil
	})
}

// ImportFile loads a SQL dump from a file on disk.
//
// It reads the file twice — once to hash it, once to upload it — so a
// dump larger than memory imports without being held in it. That is
// the difference from [Admin.Import], and the reason both exist: D1
// requires the hash before the upload begins, so the choice is
// between a second read and a full buffer.
func (a *Admin) ImportFile(ctx context.Context, databaseID, path string) (*Imported, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("drops/cloudflare/d1: import file: %w", err)
	}
	etag, err := fileMD5(path)
	if err != nil {
		return nil, err
	}
	return a.importFrom(ctx, databaseID, etag, info.Size(), func() (io.ReadCloser, error) {
		return os.Open(path) //nolint:gosec // the path is the caller's own argument.
	})
}

// importFrom runs D1's three-step import against a body the caller
// can open on demand — which is what lets ImportFile stream.
func (a *Admin) importFrom(ctx context.Context, databaseID, etag string, size int64, open func() (io.ReadCloser, error)) (*Imported, error) {
	if databaseID == "" {
		return nil, ErrNoDatabaseID
	}
	path := a.cf.AccountPath("/d1/database/", databaseID) + "/import"

	// 1. init: D1 answers with somewhere to put the file. The etag
	//    is how it recognises a file it already has, so an import
	//    retried after a failed ingest does not upload twice.
	var init transferResponse
	if err := a.cf.Do(ctx, cloudflare.Request{
		Method: http.MethodPost,
		Path:   path,
		Body:   map[string]any{"action": "init", "etag": etag},
	}, &init); err != nil {
		return nil, ClassifyError(err)
	}
	if init.UploadURL == "" {
		return nil, fmt.Errorf("%w: D1 gave no upload URL for the dump", ErrImportFailed)
	}

	// 2. upload: a presigned PUT straight to R2, without the API
	//    token — the signature is the authorisation.
	if err := a.upload(ctx, init.UploadURL, size, open); err != nil {
		return nil, err
	}

	// 3. ingest, then poll until D1 has run the statements.
	filename := init.Filename
	if filename == "" {
		filename = init.Result.Filename
	}
	resp, err := a.runJob(ctx, path,
		map[string]any{"action": "ingest", "etag": etag, "filename": filename},
		func(bookmark string) map[string]any {
			return map[string]any{"action": "poll", "current_bookmark": bookmark}
		})
	if err != nil {
		return nil, err
	}
	if resp.failed() {
		return nil, fmt.Errorf("%w: %s", ErrImportFailed, resp.Error)
	}
	return &Imported{
		FinalBookmark: resp.Result.FinalBookmark,
		NumQueries:    resp.Result.NumQueries,
		Meta:          resp.Result.Meta,
		Messages:      resp.Messages,
	}, nil
}

// upload PUTs the dump to the presigned URL D1 handed back.
func (a *Admin) upload(ctx context.Context, uploadURL string, size int64, open func() (io.ReadCloser, error)) error {
	body, err := open()
	if err != nil {
		return fmt.Errorf("drops/cloudflare/d1: open dump: %w", err)
	}
	defer body.Close()

	req, err := http.NewRequestWithContext(ctx, http.MethodPut, uploadURL, body)
	if err != nil {
		return fmt.Errorf("drops/cloudflare/d1: build upload request: %w", err)
	}
	req.ContentLength = size
	req.Header.Set("Content-Type", "application/sql")

	resp, err := a.cf.HTTPClient().Do(req)
	if err != nil {
		return fmt.Errorf("drops/cloudflare/d1: upload dump: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("%w: the presigned upload returned %d: %s",
			ErrUploadRejected, resp.StatusCode, truncate(string(snippet), 200))
	}
	return nil
}

// runJob starts a polled job and waits for it to finish.
//
// start is the body that begins it; poll builds the body that asks
// again, from the bookmark the job reported. Both endpoints work this
// way, which is why they share this.
func (a *Admin) runJob(ctx context.Context, path string, start map[string]any, poll func(bookmark string) map[string]any) (*transferResponse, error) {
	deadline := time.Time{}
	if a.wait > 0 {
		deadline = time.Now().Add(a.wait)
	}

	body := start
	for {
		var resp transferResponse
		if err := a.cf.Do(ctx, cloudflare.Request{
			Method: http.MethodPost,
			Path:   path,
			Body:   body,
		}, &resp); err != nil {
			return nil, ClassifyError(err)
		}
		if resp.done() || resp.failed() {
			return &resp, nil
		}
		if resp.AtBookmark == "" {
			// Without a bookmark there is nothing to poll against,
			// and asking again with the starting body would restart
			// the job rather than check on it.
			return nil, fmt.Errorf("%w: D1 reported status %q without a bookmark to poll", ErrPollTimeout, resp.Status)
		}
		if !deadline.IsZero() && time.Now().After(deadline) {
			return nil, fmt.Errorf("%w: still %q after %s (d1.WithPollTimeout changes this); the job is still running at D1",
				ErrPollTimeout, resp.Status, a.wait)
		}
		body = poll(resp.AtBookmark)

		timer := time.NewTimer(a.poll)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

// fileMD5 hashes a file without holding it in memory.
func fileMD5(path string) (string, error) {
	f, err := os.Open(path) //nolint:gosec // the path is the caller's own argument.
	if err != nil {
		return "", fmt.Errorf("drops/cloudflare/d1: hash dump: %w", err)
	}
	defer f.Close()
	h := md5.New() //nolint:gosec // Cloudflare's import API specifies an MD5 etag.
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("drops/cloudflare/d1: hash dump: %w", err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
