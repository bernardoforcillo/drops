package hyperdrive

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/bernardoforcillo/drops/cloudflare"
)

// Configuration API errors.
var (
	// ErrNoConfigID is returned by an operation given an empty
	// Hyperdrive configuration ID.
	ErrNoConfigID = errors.New("drops/cloudflare/hyperdrive: configuration ID is empty")

	// ErrNoConfigName is returned by [Client.Create] without a name.
	ErrNoConfigName = errors.New("drops/cloudflare/hyperdrive: configuration name is empty")

	// ErrNoOrigin is returned when a configuration names no origin
	// database to put Hyperdrive in front of.
	ErrNoOrigin = errors.New("drops/cloudflare/hyperdrive: configuration has no origin")

	// ErrNoPassword is returned by [Client.Create] without one.
	// Hyperdrive holds the credential and never gives it back, so a
	// configuration created without one cannot be repaired by
	// reading it and is only fixable by replacing it.
	ErrNoPassword = errors.New("drops/cloudflare/hyperdrive: origin has no password")
)

// Client manages Hyperdrive configurations over Cloudflare's REST
// API.
//
// It is the one part of this package that talks to Cloudflare. The
// rest — [Config.DSN], [Check], [Report] — is about what happens to a
// drops feature once a pooler is in the path, and needs no network at
// all.
//
// What it is for is provisioning: the Hyperdrive in front of a
// database has to exist before a Worker can bind it, and creating it
// from the code that owns the database beats creating it by hand in a
// dashboard and writing the ID down somewhere.
//
//	hc := hyperdrive.NewClient(cf)
//	cfg, err := hc.Create(ctx, hyperdrive.NewConfig{
//	    Name:   "primary",
//	    Origin: hyperdrive.Origin{Engine: hyperdrive.PostgreSQL, Host: …, Database: …, User: …},
//	    Password: os.Getenv("ORIGIN_PASSWORD"),
//	})
//
// Creating one does not make the features in [Unsupported] work. Call
// [Check] as well, at boot, for the ones this deployment relies on.
type Client struct {
	cf *cloudflare.Client
}

// NewClient returns a Hyperdrive configuration client for the account
// cf addresses.
//
// The token needs "Hyperdrive:Edit" to create or change a
// configuration, "Hyperdrive:Read" to list them.
func NewClient(cf *cloudflare.Client) *Client { return &Client{cf: cf} }

// Client returns the underlying Cloudflare API client.
func (c *Client) Client() *cloudflare.Client { return c.cf }

// Origin is the database Hyperdrive connects to on your behalf.
//
// It carries no password. Cloudflare accepts one when a configuration
// is created or replaced and never returns it, so keeping it out of
// the type that round-trips is what stops a read-modify-write from
// quietly blanking the credential — the password is a separate
// argument on the two calls that can set it.
type Origin struct {
	// Engine is postgresql or mysql. Defaults to [PostgreSQL].
	Engine Engine

	// Host and Port address the origin database. Port defaults to
	// 5432 for PostgreSQL and 3306 for MySQL.
	Host string
	Port int

	// Database is the database name.
	Database string

	// User is the user Hyperdrive authenticates as.
	User string

	// AccessClientID is the Cloudflare Access client ID, for an
	// origin reached through an Access-protected tunnel rather than
	// over the public internet.
	AccessClientID string
}

// Caching is Hyperdrive's query cache.
//
// It caches the results of read queries at the edge, which is the
// other half of what Hyperdrive is for and the half that changes what
// a query returns. A cached SELECT can be up to MaxAge seconds stale,
// so anything that must read its own write has to be excluded — which
// Hyperdrive does by not caching inside a transaction, and which is
// why the read-modify-write belongs in one.
type Caching struct {
	// Disabled turns the query cache off entirely.
	Disabled bool

	// MaxAge is how long a cached result may be served, in seconds.
	// Zero leaves Cloudflare's default of 60.
	MaxAge int

	// StaleWhileRevalidate is how long a stale result may be served
	// while a fresh one is fetched, in seconds. Zero leaves
	// Cloudflare's default of 15.
	StaleWhileRevalidate int
}

// Configuration is one Hyperdrive configuration as the API describes
// it.
type Configuration struct {
	// ID is the configuration's identifier — what a Worker's
	// binding refers to.
	ID string

	// Name is its name in the dashboard.
	Name string

	// Origin is the database it fronts. Its password is never
	// present: Cloudflare does not return it.
	Origin Origin

	// Caching is the query cache's settings.
	Caching Caching

	// OriginConnectionLimit is the soft ceiling on connections
	// Hyperdrive will open to the origin.
	OriginConnectionLimit int

	// CreatedOn and ModifiedOn are when it was created and last
	// changed.
	CreatedOn  time.Time
	ModifiedOn time.Time
}

// wireConfig is the JSON shape, kept separate from [Configuration] so
// the exported type can use Go's names and types.
type wireConfig struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Origin struct {
		Scheme         string `json:"scheme"`
		Host           string `json:"host"`
		Port           int    `json:"port"`
		Database       string `json:"database"`
		User           string `json:"user"`
		AccessClientID string `json:"access_client_id"`
	} `json:"origin"`
	Caching struct {
		Disabled             bool `json:"disabled"`
		MaxAge               int  `json:"max_age"`
		StaleWhileRevalidate int  `json:"stale_while_revalidate"`
	} `json:"caching"`
	OriginConnectionLimit int       `json:"origin_connection_limit"`
	CreatedOn             time.Time `json:"created_on"`
	ModifiedOn            time.Time `json:"modified_on"`
}

// toConfiguration converts a decoded reply.
func (w wireConfig) toConfiguration() Configuration {
	engine := PostgreSQL
	if w.Origin.Scheme == string(MySQL) {
		engine = MySQL
	}
	return Configuration{
		ID:   w.ID,
		Name: w.Name,
		Origin: Origin{
			Engine:         engine,
			Host:           w.Origin.Host,
			Port:           w.Origin.Port,
			Database:       w.Origin.Database,
			User:           w.Origin.User,
			AccessClientID: w.Origin.AccessClientID,
		},
		Caching: Caching{
			Disabled:             w.Caching.Disabled,
			MaxAge:               w.Caching.MaxAge,
			StaleWhileRevalidate: w.Caching.StaleWhileRevalidate,
		},
		OriginConnectionLimit: w.OriginConnectionLimit,
		CreatedOn:             w.CreatedOn,
		ModifiedOn:            w.ModifiedOn,
	}
}

// NewConfig describes a Hyperdrive configuration to create.
type NewConfig struct {
	// Name identifies the configuration in the dashboard.
	Name string

	// Origin is the database to put Hyperdrive in front of.
	Origin Origin

	// Password is the origin user's password. Required: Cloudflare
	// stores it and never returns it.
	Password string

	// Caching configures the query cache. The zero value leaves
	// Cloudflare's defaults.
	Caching Caching

	// OriginConnectionLimit is the soft ceiling on connections to
	// the origin. Zero leaves Cloudflare's default, which is 20 on
	// the free tier and 60 on a paid one.
	OriginConnectionLimit int
}

// body renders the create or replace payload.
func (n NewConfig) body() (map[string]any, error) {
	if n.Name == "" {
		return nil, ErrNoConfigName
	}
	engine := n.Origin.Engine
	if engine == "" {
		engine = PostgreSQL
	}
	switch engine {
	case PostgreSQL, MySQL:
	default:
		return nil, fmt.Errorf("%w: unknown engine %q", ErrIncompleteConfig, engine)
	}
	if n.Origin.Host == "" || n.Origin.Database == "" || n.Origin.User == "" {
		return nil, fmt.Errorf("%w: Host, Database and User are all required", ErrNoOrigin)
	}
	if n.Password == "" {
		return nil, ErrNoPassword
	}

	port := n.Origin.Port
	if port == 0 {
		port = defaultPort(engine)
	}
	origin := map[string]any{
		"scheme":   string(engine),
		"host":     n.Origin.Host,
		"port":     port,
		"database": n.Origin.Database,
		"user":     n.Origin.User,
		"password": n.Password,
	}
	if n.Origin.AccessClientID != "" {
		origin["access_client_id"] = n.Origin.AccessClientID
	}

	body := map[string]any{"name": n.Name, "origin": origin}
	caching := map[string]any{}
	if n.Caching.Disabled {
		caching["disabled"] = true
	}
	if n.Caching.MaxAge > 0 {
		caching["max_age"] = n.Caching.MaxAge
	}
	if n.Caching.StaleWhileRevalidate > 0 {
		caching["stale_while_revalidate"] = n.Caching.StaleWhileRevalidate
	}
	if len(caching) > 0 {
		body["caching"] = caching
	}
	if n.OriginConnectionLimit > 0 {
		body["origin_connection_limit"] = n.OriginConnectionLimit
	}
	return body, nil
}

// defaultPort is the port an engine listens on when none is given.
func defaultPort(e Engine) int {
	if e == MySQL {
		return 3306
	}
	return 5432
}

// Create makes a Hyperdrive configuration and returns it.
//
// The returned [Configuration.ID] is what a Worker's binding names.
// The password is not in the result and cannot be read back later.
func (c *Client) Create(ctx context.Context, cfg NewConfig) (*Configuration, error) {
	body, err := cfg.body()
	if err != nil {
		return nil, err
	}
	var out wireConfig
	if err := c.cf.Do(ctx, cloudflare.Request{
		Method: http.MethodPost,
		Path:   c.cf.AccountPath("/hyperdrive/configs"),
		Body:   body,
	}, &out); err != nil {
		return nil, err
	}
	got := out.toConfiguration()
	return &got, nil
}

// Get returns one configuration.
func (c *Client) Get(ctx context.Context, configID string) (*Configuration, error) {
	if configID == "" {
		return nil, ErrNoConfigID
	}
	var out wireConfig
	if err := c.cf.Do(ctx, cloudflare.Request{
		Method: http.MethodGet,
		Path:   c.cf.AccountPath("/hyperdrive/configs/", configID),
	}, &out); err != nil {
		return nil, err
	}
	got := out.toConfiguration()
	return &got, nil
}

// Replace overwrites a configuration in place, keeping its ID — so a
// Worker's binding does not have to change.
//
// It is a replace rather than a patch, which means the password has
// to be supplied again: Cloudflare never returns it, so there is
// nothing to carry over from the existing configuration, and a
// replace that omitted it would leave Hyperdrive unable to reach the
// origin.
func (c *Client) Replace(ctx context.Context, configID string, cfg NewConfig) (*Configuration, error) {
	if configID == "" {
		return nil, ErrNoConfigID
	}
	body, err := cfg.body()
	if err != nil {
		return nil, err
	}
	var out wireConfig
	if err := c.cf.Do(ctx, cloudflare.Request{
		Method: http.MethodPut,
		Path:   c.cf.AccountPath("/hyperdrive/configs/", configID),
		Body:   body,
	}, &out); err != nil {
		return nil, err
	}
	got := out.toConfiguration()
	return &got, nil
}

// SetCaching changes a configuration's query cache without touching
// its origin — the one edit that does not need the password again.
func (c *Client) SetCaching(ctx context.Context, configID string, caching Caching) (*Configuration, error) {
	if configID == "" {
		return nil, ErrNoConfigID
	}
	body := map[string]any{"caching": map[string]any{
		"disabled":               caching.Disabled,
		"max_age":                caching.MaxAge,
		"stale_while_revalidate": caching.StaleWhileRevalidate,
	}}
	if caching.MaxAge <= 0 {
		delete(body["caching"].(map[string]any), "max_age")
	}
	if caching.StaleWhileRevalidate <= 0 {
		delete(body["caching"].(map[string]any), "stale_while_revalidate")
	}
	var out wireConfig
	if err := c.cf.Do(ctx, cloudflare.Request{
		Method: http.MethodPatch,
		Path:   c.cf.AccountPath("/hyperdrive/configs/", configID),
		Body:   body,
	}, &out); err != nil {
		return nil, err
	}
	got := out.toConfiguration()
	return &got, nil
}

// Delete removes a configuration. A Worker still bound to it will
// fail to connect.
func (c *Client) Delete(ctx context.Context, configID string) error {
	if configID == "" {
		return ErrNoConfigID
	}
	return c.cf.Do(ctx, cloudflare.Request{
		Method: http.MethodDelete,
		Path:   c.cf.AccountPath("/hyperdrive/configs/", configID),
	}, nil)
}

// maxListPages caps how far [Client.List] will walk.
const maxListPages = 200

// List returns the account's Hyperdrive configurations.
func (c *Client) List(ctx context.Context) ([]Configuration, error) {
	var out []Configuration
	for page := 1; page <= maxListPages; page++ {
		q := url.Values{}
		q.Set("page", strconv.Itoa(page))
		q.Set("per_page", "100")
		env, err := c.cf.DoEnvelope(ctx, cloudflare.Request{
			Method: http.MethodGet,
			Path:   c.cf.AccountPath("/hyperdrive/configs"),
			Query:  q,
		})
		if err != nil {
			return nil, err
		}
		var batch []wireConfig
		if err := json.Unmarshal(env.Result, &batch); err != nil {
			return nil, fmt.Errorf("drops/cloudflare/hyperdrive: decode result: %w", err)
		}
		for _, w := range batch {
			out = append(out, w.toConfiguration())
		}
		if len(batch) < 100 {
			return out, nil
		}
	}
	return out, fmt.Errorf("drops/cloudflare/hyperdrive: stopped listing configurations after %d pages", maxListPages)
}

// FindByName returns the configuration with the given name, or
// [cloudflare.ErrNotFound].
func (c *Client) FindByName(ctx context.Context, name string) (*Configuration, error) {
	if name == "" {
		return nil, ErrNoConfigName
	}
	all, err := c.List(ctx)
	if err != nil {
		return nil, err
	}
	for i := range all {
		if all[i].Name == name {
			return &all[i], nil
		}
	}
	return nil, fmt.Errorf("drops/cloudflare/hyperdrive: no configuration named %q: %w", name, cloudflare.ErrNotFound)
}

// DirectConfig returns a [Config] addressing this configuration's
// origin database directly, past Hyperdrive.
//
// That is the connection a migration runner, a logical-replication
// consumer or anything in [Unsupported] needs: those do not work
// through a pooler, and the whole point of [Check] is to find that
// out at boot rather than at the first NOTIFY nobody receives.
//
// The password is the caller's to supply, because Cloudflare does not
// return it.
//
// sslmode is left to the caller and matters here in a way it does not
// for the hop to Hyperdrive: this connection leaves your network for
// the origin, so set [Config.SSLMode] on the result.
func (c Configuration) DirectConfig(password string) Config {
	return Config{
		Engine:   c.Origin.Engine,
		Host:     c.Origin.Host,
		Port:     c.Origin.Port,
		Database: c.Origin.Database,
		User:     c.Origin.User,
		Password: password,
	}
}
