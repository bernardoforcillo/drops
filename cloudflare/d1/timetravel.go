package d1

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/bernardoforcillo/drops/cloudflare"
)

// Time Travel errors.
var (
	// ErrNoRestorePoint is returned by [Admin.Restore] given neither
	// a bookmark nor a timestamp.
	ErrNoRestorePoint = errors.New("drops/cloudflare/d1: restore needs a bookmark or a timestamp")

	// ErrAmbiguousRestorePoint is returned by [Admin.Restore] given
	// both. D1 takes one, and picking for the caller would silently
	// ignore half of what they asked for.
	ErrAmbiguousRestorePoint = errors.New("drops/cloudflare/d1: restore takes a bookmark or a timestamp, not both")
)

// TimeTravelRetention is how far back D1 keeps a database's history:
// thirty days on the Workers Paid plan, seven on the free one.
//
// A bookmark older than the window is not a slow restore, it is an
// invalid one — which is what makes the difference between Time
// Travel and a backup. [Admin.Export] is the backup.
const (
	TimeTravelRetentionPaid = 30 * 24 * time.Hour
	TimeTravelRetentionFree = 7 * 24 * time.Hour
)

// Bookmark returns the database's current Time Travel bookmark: a
// marker for the state it is in right now.
//
// Take one before a migration and the way back is a single call. The
// bookmarks are lexicographically sortable and derivable from a
// timestamp, which is why [Admin.BookmarkAt] can answer for a moment
// nobody thought to mark at the time.
//
// This is a different bookmark from the one a [Session] carries.
// Both are positions in the database's history, but a session's
// bookmark is a consistency floor for a read and this one is a
// restore point for the whole database; they are not interchangeable,
// and D1 does not accept one where it wants the other.
func (a *Admin) Bookmark(ctx context.Context, databaseID string) (string, error) {
	return a.bookmark(ctx, databaseID, nil)
}

// BookmarkAt returns the nearest bookmark at or before a moment —
// the restore point for "just before the deploy at 14:05".
//
// A moment outside the retention window has no bookmark, and D1 says
// so rather than answering with the oldest one it still has.
func (a *Admin) BookmarkAt(ctx context.Context, databaseID string, at time.Time) (string, error) {
	return a.bookmark(ctx, databaseID, &at)
}

func (a *Admin) bookmark(ctx context.Context, databaseID string, at *time.Time) (string, error) {
	if databaseID == "" {
		return "", ErrNoDatabaseID
	}
	q := url.Values{}
	if at != nil {
		q.Set("timestamp", at.UTC().Format(time.RFC3339))
	}
	var out struct {
		Bookmark string `json:"bookmark"`
	}
	err := a.cf.Do(ctx, cloudflare.Request{
		Method: http.MethodGet,
		Path:   a.cf.AccountPath("/d1/database/", databaseID) + "/time_travel/bookmark",
		Query:  q,
	}, &out)
	if err != nil {
		return "", ClassifyError(err)
	}
	return out.Bookmark, nil
}

// Restored is what a Time Travel restore reports.
type Restored struct {
	// Bookmark is where the database stands after the restore.
	Bookmark string `json:"bookmark"`

	// PreviousBookmark is where it stood before — the way to undo
	// the restore, and the field to log before doing anything else.
	// A restore is itself a change to the database's history, so
	// this is the only handle on the state that was replaced.
	PreviousBookmark string `json:"previous_bookmark"`

	// Message is D1's own description of what it did.
	Message string `json:"message"`
}

// RestoreOptions names the point to restore to. Exactly one of the
// two must be set.
type RestoreOptions struct {
	// Bookmark restores to a marker from [Admin.Bookmark].
	Bookmark string

	// At restores to a moment, which D1 resolves to the nearest
	// bookmark at or before it.
	At time.Time
}

// Restore rewinds a database to an earlier point in its history.
//
// This is not a copy into a new database: the database is restored in
// place, and everything written after the restore point is gone.
// [Restored.PreviousBookmark] is the only way back, so store it
// before doing anything else with the result.
//
// Time Travel needs a database at version "production"
// ([Database.Version]), and reaches back as far as
// [TimeTravelRetentionPaid] or [TimeTravelRetentionFree].
func (a *Admin) Restore(ctx context.Context, databaseID string, opts RestoreOptions) (*Restored, error) {
	if databaseID == "" {
		return nil, ErrNoDatabaseID
	}
	hasBookmark := opts.Bookmark != ""
	hasTime := !opts.At.IsZero()
	switch {
	case hasBookmark && hasTime:
		return nil, ErrAmbiguousRestorePoint
	case !hasBookmark && !hasTime:
		return nil, ErrNoRestorePoint
	}

	q := url.Values{}
	if hasBookmark {
		q.Set("bookmark", opts.Bookmark)
	} else {
		q.Set("timestamp", opts.At.UTC().Format(time.RFC3339))
	}

	var out Restored
	err := a.cf.Do(ctx, cloudflare.Request{
		Method: http.MethodPost,
		Path:   a.cf.AccountPath("/d1/database/", databaseID) + "/time_travel/restore",
		Query:  q,
	}, &out)
	if err != nil {
		return nil, ClassifyError(err)
	}
	return &out, nil
}

// decodeJSON decodes raw into v with UseNumber, so an integer past
// 2^53 keeps its low bits on the way through — the same reading the
// statement path uses, applied to the management payloads for the
// same reason.
func decodeJSON(raw json.RawMessage, v any) error {
	if len(raw) == 0 {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("drops/cloudflare/d1: decode result: %w", err)
	}
	return nil
}
