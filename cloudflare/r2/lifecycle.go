package r2

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/bernardoforcillo/drops/cloudflare"
)

// Lifecycle errors.
var (
	// ErrNoRuleID is returned for a lifecycle rule without an
	// identifier. R2 addresses rules by id, so an unnamed one cannot
	// be changed or removed later without rewriting the whole set.
	ErrNoRuleID = errors.New("drops/cloudflare/r2: lifecycle rule has no ID")

	// ErrEmptyRule is returned for a rule that does nothing —
	// neither a deletion, nor a storage-class transition, nor an
	// abort. R2 accepts it and it has no effect, which is a rule
	// that looks like a retention policy and is not one.
	ErrEmptyRule = errors.New("drops/cloudflare/r2: lifecycle rule has no transition")

	// ErrAmbiguousCondition is returned for a transition given both
	// an age and a date.
	ErrAmbiguousCondition = errors.New("drops/cloudflare/r2: a transition takes an age or a date, not both")
)

// Transition is when a lifecycle rule acts on an object.
//
// Exactly one of After and On is set: After is an age measured from
// the object's own upload, On is a wall-clock date the whole bucket
// crosses at once. The first is a retention policy, the second is a
// deadline.
type Transition struct {
	// After acts once an object is this old. R2 counts in seconds,
	// so anything finer is truncated.
	After time.Duration

	// On acts on the given date, whatever an object's age.
	On time.Time
}

// zero reports whether the transition says nothing.
func (t Transition) zero() bool { return t.After <= 0 && t.On.IsZero() }

// condition renders the transition as R2's condition object.
func (t Transition) condition() (map[string]any, error) {
	switch {
	case t.After > 0 && !t.On.IsZero():
		return nil, ErrAmbiguousCondition
	case t.After > 0:
		return map[string]any{"type": "Age", "maxAge": int64(t.After.Seconds())}, nil
	case !t.On.IsZero():
		return map[string]any{"type": "Date", "date": t.On.UTC().Format(time.RFC3339)}, nil
	default:
		return nil, nil
	}
}

// StorageClassTransition moves an object to another storage class
// once a [Transition] fires.
type StorageClassTransition struct {
	Transition

	// StorageClass is what the object becomes. Only
	// [InfrequentAccess] is a destination R2 offers: objects start
	// Standard and move down, never back.
	StorageClass StorageClass
}

// LifecycleRule is one rule in a bucket's lifecycle policy.
//
// A rule is a prefix and the things that happen to objects under it.
// For a bucket of database dumps that is usually one of two
// sentences: delete them after a retention period, or make them cheap
// to keep after a month and delete them after a year.
type LifecycleRule struct {
	// ID names the rule. R2 addresses rules by it.
	ID string

	// Enabled turns the rule off without deleting it, which is how a
	// retention policy is suspended while something is investigated.
	Enabled bool

	// Prefix scopes the rule to keys starting with it. Empty means
	// every object in the bucket — worth reading twice on a rule
	// that deletes.
	Prefix string

	// Delete removes the object.
	Delete Transition

	// Transitions move it between storage classes.
	Transitions []StorageClassTransition

	// AbortIncompleteUploadsAfter cleans up multipart uploads that
	// were started and never finished. They are invisible to a
	// listing and billed anyway, which is why a bucket written to by
	// an S3 client usually wants this even when nothing else here
	// applies.
	AbortIncompleteUploadsAfter time.Duration
}

// DeleteAfter returns a rule that deletes everything under prefix
// once it reaches age — the retention policy a bucket of database
// dumps wants.
//
//	backups.SetLifecycle(ctx, []r2.LifecycleRule{
//	    r2.DeleteAfter("d1-retention", "d1/", 90*24*time.Hour),
//	})
func DeleteAfter(id, prefix string, age time.Duration) LifecycleRule {
	return LifecycleRule{
		ID:      id,
		Enabled: true,
		Prefix:  prefix,
		Delete:  Transition{After: age},
	}
}

// CoolAfter returns a rule that moves everything under prefix to
// [InfrequentAccess] once it reaches age: cheap to keep, charged to
// read, which is the right shape for a backup nobody expects to open.
func CoolAfter(id, prefix string, age time.Duration) LifecycleRule {
	return LifecycleRule{
		ID:      id,
		Enabled: true,
		Prefix:  prefix,
		Transitions: []StorageClassTransition{{
			Transition:   Transition{After: age},
			StorageClass: InfrequentAccess,
		}},
	}
}

// body renders one rule as R2's rule object.
func (r LifecycleRule) body() (map[string]any, error) {
	if r.ID == "" {
		return nil, ErrNoRuleID
	}
	out := map[string]any{
		"id":         r.ID,
		"enabled":    r.Enabled,
		"conditions": map[string]any{"prefix": r.Prefix},
	}

	acted := false
	if !r.Delete.zero() {
		cond, err := r.Delete.condition()
		if err != nil {
			return nil, fmt.Errorf("drops/cloudflare/r2: rule %q delete: %w", r.ID, err)
		}
		out["deleteObjectsTransition"] = map[string]any{"condition": cond}
		acted = true
	}
	if len(r.Transitions) > 0 {
		list := make([]map[string]any, 0, len(r.Transitions))
		for i, t := range r.Transitions {
			if !t.StorageClass.Valid() {
				return nil, fmt.Errorf("drops/cloudflare/r2: rule %q transition %d: %w: %q",
					r.ID, i, ErrUnknownStorageClass, t.StorageClass)
			}
			cond, err := t.condition()
			if err != nil {
				return nil, fmt.Errorf("drops/cloudflare/r2: rule %q transition %d: %w", r.ID, i, err)
			}
			if cond == nil {
				return nil, fmt.Errorf("drops/cloudflare/r2: rule %q transition %d has no age or date", r.ID, i)
			}
			list = append(list, map[string]any{"condition": cond, "storageClass": string(t.StorageClass)})
		}
		out["storageClassTransitions"] = list
		acted = true
	}
	if r.AbortIncompleteUploadsAfter > 0 {
		out["abortMultipartUploadsTransition"] = map[string]any{
			"condition": map[string]any{"type": "Age", "maxAge": int64(r.AbortIncompleteUploadsAfter.Seconds())},
		}
		acted = true
	}
	if !acted {
		return nil, fmt.Errorf("%w: %q", ErrEmptyRule, r.ID)
	}
	return out, nil
}

// wireRule is the shape R2 answers a lifecycle read with.
type wireRule struct {
	ID         string `json:"id"`
	Enabled    bool   `json:"enabled"`
	Conditions struct {
		Prefix string `json:"prefix"`
	} `json:"conditions"`
	DeleteObjectsTransition struct {
		Condition wireCondition `json:"condition"`
	} `json:"deleteObjectsTransition"`
	StorageClassTransitions []struct {
		Condition    wireCondition `json:"condition"`
		StorageClass StorageClass  `json:"storageClass"`
	} `json:"storageClassTransitions"`
	AbortMultipartUploadsTransition struct {
		Condition wireCondition `json:"condition"`
	} `json:"abortMultipartUploadsTransition"`
}

type wireCondition struct {
	Type   string `json:"type"`
	MaxAge int64  `json:"maxAge"`
	Date   string `json:"date"`
}

// transition reads a condition back into a [Transition].
func (c wireCondition) transition() Transition {
	switch c.Type {
	case "Age":
		return Transition{After: time.Duration(c.MaxAge) * time.Second}
	case "Date":
		if t, err := time.Parse(time.RFC3339, c.Date); err == nil {
			return Transition{On: t}
		}
	}
	return Transition{}
}

// Lifecycle returns the bucket's lifecycle rules.
func (b *Bucket) Lifecycle(ctx context.Context) ([]LifecycleRule, error) {
	if b.name == "" {
		return nil, ErrNoBucketName
	}
	var raw struct {
		Rules []wireRule `json:"rules"`
	}
	if err := b.c.cf.Do(ctx, cloudflare.Request{
		Method: http.MethodGet,
		Path:   b.c.cf.AccountPath("/r2/buckets", b.name, "lifecycle"),
		Header: b.c.headers(),
	}, &raw); err != nil {
		return nil, err
	}

	out := make([]LifecycleRule, len(raw.Rules))
	for i, w := range raw.Rules {
		r := LifecycleRule{
			ID:      w.ID,
			Enabled: w.Enabled,
			Prefix:  w.Conditions.Prefix,
			Delete:  w.DeleteObjectsTransition.Condition.transition(),
		}
		if c := w.AbortMultipartUploadsTransition.Condition; c.Type == "Age" {
			r.AbortIncompleteUploadsAfter = time.Duration(c.MaxAge) * time.Second
		}
		for _, t := range w.StorageClassTransitions {
			r.Transitions = append(r.Transitions, StorageClassTransition{
				Transition:   t.Condition.transition(),
				StorageClass: t.StorageClass,
			})
		}
		out[i] = r
	}
	return out, nil
}

// SetLifecycle replaces the bucket's lifecycle policy with rules.
//
// It replaces: R2 has no add-one-rule endpoint, so whatever is not in
// rules is gone. A caller adding a rule to a bucket somebody else also
// configures has to read the current set with [Bucket.Lifecycle],
// append to it, and write the whole thing back — and has to accept
// that two of them doing it at once is a last-write-wins race, because
// there is no version to make the write conditional on.
//
// Passing no rules removes the policy entirely, which is a thing to do
// on purpose rather than by handing this a slice that happened to be
// empty; [Bucket.ClearLifecycle] is the spelling that says so.
func (b *Bucket) SetLifecycle(ctx context.Context, rules []LifecycleRule) error {
	if b.name == "" {
		return ErrNoBucketName
	}
	seen := make(map[string]struct{}, len(rules))
	body := make([]map[string]any, 0, len(rules))
	for _, r := range rules {
		one, err := r.body()
		if err != nil {
			return err
		}
		if _, dup := seen[r.ID]; dup {
			return fmt.Errorf("drops/cloudflare/r2: two lifecycle rules share the ID %q — R2 addresses rules by it", r.ID)
		}
		seen[r.ID] = struct{}{}
		body = append(body, one)
	}
	return b.c.cf.Do(ctx, cloudflare.Request{
		Method: http.MethodPut,
		Path:   b.c.cf.AccountPath("/r2/buckets", b.name, "lifecycle"),
		Body:   map[string]any{"rules": body},
		Header: b.c.headers(),
	}, nil)
}

// ClearLifecycle removes every lifecycle rule from the bucket, so
// nothing is deleted or transitioned automatically any more.
func (b *Bucket) ClearLifecycle(ctx context.Context) error {
	return b.SetLifecycle(ctx, nil)
}

// Permission is what a temporary credential may do.
type Permission string

// The permissions R2 defines for a temporary credential.
const (
	// AdminReadWrite can read and write objects and manage the
	// bucket itself.
	AdminReadWrite Permission = "admin-read-write"

	// AdminReadOnly can read objects and the bucket's configuration.
	AdminReadOnly Permission = "admin-read-only"

	// ObjectReadWrite can read and write objects, and nothing else.
	ObjectReadWrite Permission = "object-read-write"

	// ObjectReadOnly can read objects, and nothing else. It is the
	// one to hand out.
	ObjectReadOnly Permission = "object-read-only"
)

// Valid reports whether p is a permission R2 defines.
func (p Permission) Valid() bool {
	switch p {
	case AdminReadWrite, AdminReadOnly, ObjectReadWrite, ObjectReadOnly:
		return true
	default:
		return false
	}
}

// CredentialRequest asks R2 for a scoped, time-limited S3 credential.
type CredentialRequest struct {
	// Bucket is the bucket the credential may reach.
	Bucket string

	// ParentAccessKeyID is the R2 access key the new credential is
	// derived from and signed by. It is an R2 access key — the kind
	// minted for the S3 API — not the API token this package
	// authenticates with, and the credential cannot outlive it.
	ParentAccessKeyID string

	// Permission is what the credential may do.
	Permission Permission

	// TTL is how long it lives.
	TTL time.Duration

	// Objects narrows it to these exact keys.
	Objects []string

	// Prefixes narrows it to keys under these prefixes.
	Prefixes []string
}

// Credentials are an S3 credential R2 minted.
//
// They are not usable with this package: everything here
// authenticates with the account's API token against Cloudflare's
// REST API, and these are for the S3 protocol. Minting one is how a
// scoped, expiring handle on a bucket is handed to something else —
// a browser uploading directly, a partner fetching one prefix, a job
// in another account — without giving it a credential that outlives
// the task or reaches past it.
type Credentials struct {
	AccessKeyID     string `json:"accessKeyId"`
	SecretAccessKey string `json:"secretAccessKey"`
	SessionToken    string `json:"sessionToken"`
}

// TemporaryCredentials mints a scoped, expiring S3 credential for a
// bucket.
func (c *Client) TemporaryCredentials(ctx context.Context, req CredentialRequest) (*Credentials, error) {
	switch {
	case req.Bucket == "":
		return nil, ErrNoBucketName
	case req.ParentAccessKeyID == "":
		return nil, errors.New("drops/cloudflare/r2: a temporary credential needs a parent R2 access key ID to be signed by")
	case !req.Permission.Valid():
		return nil, fmt.Errorf("drops/cloudflare/r2: unknown permission %q", req.Permission)
	case req.TTL <= 0:
		return nil, errors.New("drops/cloudflare/r2: a temporary credential needs a TTL; one that never expires is not temporary")
	}

	body := map[string]any{
		"bucket":            req.Bucket,
		"parentAccessKeyId": req.ParentAccessKeyID,
		"permission":        string(req.Permission),
		"ttlSeconds":        int64(req.TTL.Seconds()),
	}
	if len(req.Objects) > 0 {
		body["objects"] = req.Objects
	}
	if len(req.Prefixes) > 0 {
		body["prefixes"] = req.Prefixes
	}

	var out Credentials
	if err := c.cf.Do(ctx, cloudflare.Request{
		Method: http.MethodPost,
		Path:   c.cf.AccountPath("/r2/temp-access-credentials"),
		Body:   body,
		Header: c.headers(),
	}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
