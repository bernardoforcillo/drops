package d1

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/bernardoforcillo/drops/cloudflare"
)

// Admin errors.
var (
	// ErrNoDatabaseName is returned by [Admin.Create] without one.
	ErrNoDatabaseName = errors.New("drops/cloudflare/d1: database name is empty")

	// ErrUnknownJurisdiction is returned for a jurisdiction D1 does
	// not define.
	ErrUnknownJurisdiction = errors.New("drops/cloudflare/d1: unknown jurisdiction")

	// ErrUnknownLocationHint is returned for a primary location hint
	// D1 does not define.
	ErrUnknownLocationHint = errors.New("drops/cloudflare/d1: unknown primary location hint")

	// ErrUnknownReplicationMode is returned for a read-replication
	// mode D1 does not define.
	ErrUnknownReplicationMode = errors.New("drops/cloudflare/d1: unknown read replication mode")
)

// Admin manages D1 databases themselves — as opposed to [Driver],
// which runs statements inside one.
//
// It is a separate type, and needs a [cloudflare.Client], because the
// two are separately authorised and separately reachable. Creating a
// database is an account-level operation over Cloudflare's REST API;
// running a statement can go through a Worker binding that has no
// account credential at all. A service that only queries should hold
// a Driver and no Admin, and a token scoped to D1:Read cannot create
// anything — which is the arrangement to want.
//
// The operations here are what a database-per-tenant deployment needs
// and what a single-database one uses at most once:
//
//   - [Admin.Create], [Admin.List], [Admin.Get], [Admin.Delete] —
//     provisioning. D1's 10 GB ceiling on one database is what makes
//     database-per-tenant a real design rather than an eccentric one,
//     and this is the half of it drops could not supply before.
//   - [Admin.SetReadReplication] — turning replicas on, which is what
//     makes [Session] worth using.
//   - [Admin.Bookmark] and [Admin.Restore] — Time Travel, D1's
//     point-in-time restore.
//   - [Admin.Export] and [Admin.Import] — a SQL dump out and back in.
type Admin struct {
	cf   *cloudflare.Client
	poll time.Duration
	wait time.Duration
}

// AdminOption configures an [Admin].
type AdminOption func(*Admin)

// WithPollInterval sets how often [Admin.Export] and [Admin.Import]
// ask D1 whether the job has finished. Defaults to one second.
func WithPollInterval(d time.Duration) AdminOption {
	return func(a *Admin) {
		if d > 0 {
			a.poll = d
		}
	}
}

// WithPollTimeout caps how long those two will wait in total.
// Defaults to ten minutes; pass 0 to wait as long as the context
// allows.
//
// It is a separate bound from the context's because the two answer
// different questions: the context is how long the caller has, and
// this is how long a job is allowed to look alive without finishing.
func WithPollTimeout(d time.Duration) AdminOption {
	return func(a *Admin) { a.wait = d }
}

// NewAdmin returns an Admin for the account cf addresses.
//
// The token needs D1:Edit for anything that writes, D1:Read for the
// rest.
func NewAdmin(cf *cloudflare.Client, opts ...AdminOption) *Admin {
	a := &Admin{cf: cf, poll: time.Second, wait: 10 * time.Minute}
	for _, o := range opts {
		o(a)
	}
	return a
}

// Client returns the underlying Cloudflare API client.
func (a *Admin) Client() *cloudflare.Client { return a.cf }

// Driver returns a [Driver] for one of this account's databases,
// over the same client — the shortest path from "I just created it"
// to "I can run migrations on it".
func (a *Admin) Driver(databaseID string, opts ...Option) *Driver {
	return New(a.cf, databaseID, opts...)
}

// Jurisdiction restricts where a database runs and stores its data.
type Jurisdiction string

// The jurisdictions D1 defines.
const (
	JurisdictionEU      Jurisdiction = "eu"
	JurisdictionFedRAMP Jurisdiction = "fedramp"
	JurisdictionUS      Jurisdiction = "us"
)

// Valid reports whether j is a jurisdiction D1 defines.
func (j Jurisdiction) Valid() bool {
	switch j {
	case JurisdictionEU, JurisdictionFedRAMP, JurisdictionUS:
		return true
	default:
		return false
	}
}

// LocationHint asks D1 to put a new database's primary in a
// particular part of the world.
//
// It is a hint, not a placement: D1 honours it where it can. It is
// also ignored entirely when a [Jurisdiction] is set, because a
// jurisdiction is a rule and a hint is not.
type LocationHint string

// The location hints D1 defines.
const (
	LocationWesternNorthAmerica LocationHint = "wnam"
	LocationEasternNorthAmerica LocationHint = "enam"
	LocationWesternEurope       LocationHint = "weur"
	LocationEasternEurope       LocationHint = "eeur"
	LocationAsiaPacific         LocationHint = "apac"
	LocationOceania             LocationHint = "oc"
)

// Valid reports whether h is a location hint D1 defines.
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

// ReplicationMode is whether D1 keeps read replicas of a database.
type ReplicationMode string

// The replication modes D1 defines.
const (
	// ReplicationAuto lets D1 create replicas and place them around
	// the world. Reads may then be served by an instance that is
	// behind the primary, which is what [Session] exists to make
	// safe — turning this on without using sessions is how a
	// read-your-writes bug gets deployed.
	ReplicationAuto ReplicationMode = "auto"

	// ReplicationDisabled keeps every read on the primary.
	ReplicationDisabled ReplicationMode = "disabled"
)

// Valid reports whether m is a replication mode D1 defines.
func (m ReplicationMode) Valid() bool {
	switch m {
	case ReplicationAuto, ReplicationDisabled:
		return true
	default:
		return false
	}
}

// Database is one D1 database as the API describes it.
type Database struct {
	// UUID is the database ID — what [New] and [Admin.Driver] take.
	UUID string `json:"uuid"`

	// Name is the name it was created under, unique within the
	// account.
	Name string `json:"name"`

	// Version is D1's own generation marker. Time Travel needs
	// "production".
	Version string `json:"version"`

	// CreatedAt is when it was created.
	CreatedAt time.Time `json:"created_at"`

	// FileSize is its size in bytes. Compare against
	// [Limits.DatabaseSize]: on a paid plan that ceiling is 10 GB,
	// and a database-per-tenant scheme wants this watched per tenant
	// rather than discovered at the wall.
	FileSize int64 `json:"file_size"`

	// NumTables is how many tables it holds.
	NumTables int `json:"num_tables"`

	// Jurisdiction is the data-residency rule it was created under,
	// empty for none.
	Jurisdiction Jurisdiction `json:"jurisdiction,omitempty"`

	// ReadReplication reports whether D1 keeps replicas of it.
	ReadReplication struct {
		Mode ReplicationMode `json:"mode"`
	} `json:"read_replication"`
}

// Replicated reports whether reads of this database may be served by
// a replica — that is, whether [Session] is needed for a read to be
// consistent with what the same caller just wrote.
func (d Database) Replicated() bool { return d.ReadReplication.Mode == ReplicationAuto }

// CreateOptions describes a database to create. Only Name is
// required.
type CreateOptions struct {
	// Name is the database's name, unique within the account.
	Name string

	// Jurisdiction restricts where the data may live. Setting it
	// makes D1 ignore PrimaryLocation.
	Jurisdiction Jurisdiction

	// PrimaryLocation asks for the primary to be placed near a
	// region. Left empty, D1 places it near whoever created it —
	// which for a CI job is wherever CI happens to run, so it is
	// worth setting.
	PrimaryLocation LocationHint

	// ReadReplication turns replicas on at creation. Empty leaves
	// D1's default.
	ReadReplication ReplicationMode
}

// Create makes a new database and returns it.
//
// The returned [Database.UUID] is what [New] takes. There is no DDL
// here: the database arrives empty, and the schema goes in through
// the dialect's migrations like any other SQLite database.
func (a *Admin) Create(ctx context.Context, opts CreateOptions) (*Database, error) {
	if opts.Name == "" {
		return nil, ErrNoDatabaseName
	}
	body := map[string]any{"name": opts.Name}
	if opts.Jurisdiction != "" {
		if !opts.Jurisdiction.Valid() {
			return nil, fmt.Errorf("%w: %q", ErrUnknownJurisdiction, opts.Jurisdiction)
		}
		body["jurisdiction"] = string(opts.Jurisdiction)
	}
	if opts.PrimaryLocation != "" {
		if !opts.PrimaryLocation.Valid() {
			return nil, fmt.Errorf("%w: %q", ErrUnknownLocationHint, opts.PrimaryLocation)
		}
		body["primary_location_hint"] = string(opts.PrimaryLocation)
	}
	if opts.ReadReplication != "" {
		if !opts.ReadReplication.Valid() {
			return nil, fmt.Errorf("%w: %q", ErrUnknownReplicationMode, opts.ReadReplication)
		}
		body["read_replication"] = map[string]any{"mode": string(opts.ReadReplication)}
	}

	var out Database
	err := a.cf.Do(ctx, cloudflare.Request{
		Method: http.MethodPost,
		Path:   a.cf.AccountPath("/d1/database"),
		Body:   body,
	}, &out)
	if err != nil {
		return nil, ClassifyError(err)
	}
	return &out, nil
}

// Get returns one database by ID.
func (a *Admin) Get(ctx context.Context, databaseID string) (*Database, error) {
	if databaseID == "" {
		return nil, ErrNoDatabaseID
	}
	var out Database
	err := a.cf.Do(ctx, cloudflare.Request{
		Method: http.MethodGet,
		Path:   a.cf.AccountPath("/d1/database/", databaseID),
	}, &out)
	if err != nil {
		return nil, ClassifyError(err)
	}
	return &out, nil
}

// Delete removes a database and everything in it.
//
// There is no undo and Time Travel does not survive it: a bookmark
// points into a database's history, and the database is gone. Export
// first if the data matters.
func (a *Admin) Delete(ctx context.Context, databaseID string) error {
	if databaseID == "" {
		return ErrNoDatabaseID
	}
	err := a.cf.Do(ctx, cloudflare.Request{
		Method: http.MethodDelete,
		Path:   a.cf.AccountPath("/d1/database/", databaseID),
	}, nil)
	return ClassifyError(err)
}

// FindByName returns the database with the given name, or
// [cloudflare.ErrNotFound].
//
// Names are unique within an account, which makes this the lookup a
// database-per-tenant scheme actually wants: the tenant's identifier
// names the database, and the UUID never has to be stored anywhere.
func (a *Admin) FindByName(ctx context.Context, name string) (*Database, error) {
	if name == "" {
		return nil, ErrNoDatabaseName
	}
	dbs, err := a.List(ctx, name)
	if err != nil {
		return nil, err
	}
	for i := range dbs {
		if dbs[i].Name == name {
			return &dbs[i], nil
		}
	}
	// The name filter is a search rather than an exact match, so an
	// answer that does not contain the name is a miss.
	return nil, fmt.Errorf("drops/cloudflare/d1: no database named %q: %w", name, cloudflare.ErrNotFound)
}

// maxListPages caps how far [Admin.List] will walk. At a hundred per
// page it is fifty thousand databases, which is far past the point
// where listing them all is the right operation — the cap is there so
// a paging bug at either end cannot spin forever.
const maxListPages = 500

// List returns the account's D1 databases, walking every page.
//
// nameFilter narrows the search to names containing it; pass "" for
// all of them.
func (a *Admin) List(ctx context.Context, nameFilter string) ([]Database, error) {
	var out []Database
	for page := 1; page <= maxListPages; page++ {
		q := url.Values{}
		q.Set("page", strconv.Itoa(page))
		q.Set("per_page", "100")
		if nameFilter != "" {
			q.Set("name", nameFilter)
		}
		env, err := a.cf.DoEnvelope(ctx, cloudflare.Request{
			Method: http.MethodGet,
			Path:   a.cf.AccountPath("/d1/database"),
			Query:  q,
		})
		if err != nil {
			return nil, ClassifyError(err)
		}
		var batch []Database
		if err := decodeJSON(env.Result, &batch); err != nil {
			return nil, err
		}
		out = append(out, batch...)
		if len(batch) < 100 {
			return out, nil
		}
	}
	return out, fmt.Errorf("drops/cloudflare/d1: stopped listing databases after %d pages", maxListPages)
}

// SetReadReplication turns D1's read replicas on or off for a
// database, and returns it as it stands afterwards.
//
// Turning them on is not free of consequences: a read may then be
// served by an instance that is behind the primary. Use [Session] for
// anything that must not see the database go backwards — which is
// most things that read after writing.
func (a *Admin) SetReadReplication(ctx context.Context, databaseID string, mode ReplicationMode) (*Database, error) {
	if databaseID == "" {
		return nil, ErrNoDatabaseID
	}
	if !mode.Valid() {
		return nil, fmt.Errorf("%w: %q — use d1.ReplicationAuto or d1.ReplicationDisabled", ErrUnknownReplicationMode, mode)
	}
	var out Database
	err := a.cf.Do(ctx, cloudflare.Request{
		Method: http.MethodPatch,
		Path:   a.cf.AccountPath("/d1/database/", databaseID),
		Body:   map[string]any{"read_replication": map[string]any{"mode": string(mode)}},
	}, &out)
	if err != nil {
		return nil, ClassifyError(err)
	}
	return &out, nil
}
