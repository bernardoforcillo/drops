package hyperdrive_test

import (
	"context"
	"fmt"
	"log"
	"os"

	"github.com/bernardoforcillo/drops/cloudflare"
	"github.com/bernardoforcillo/drops/cloudflare/hyperdrive"
)

// Inside a Worker the binding hands you the connection string
// already. This is for everywhere else, where the parts arrive as
// separate environment variables and have to be put back together.
func ExampleConfig_DSN() {
	dsn, err := hyperdrive.Config{
		Engine:   hyperdrive.PostgreSQL,
		Host:     "hyperdrive.internal",
		Database: "tenders",
		User:     "app",
		Password: "p@ss/word",
		Params:   map[string]string{"application_name": "tendersbay"},
	}.DSN()
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(dsn)
	// Output: postgresql://app:p%40ss%2Fword@hyperdrive.internal:5432/tenders?application_name=tendersbay
}

// The check that belongs in a startup path: list what the service
// uses, and find out at boot rather than at the first NOTIFY nobody
// receives.
func ExampleCheck() {
	err := hyperdrive.Check(
		hyperdrive.FeatureListenNotify,
		hyperdrive.FeatureLogicalReplication,
	)
	if err != nil {
		// The message names the drops symbols that stop working and
		// the mechanism that breaks them, so it is greppable.
		fmt.Println("this service cannot run behind Hyperdrive")
	}
	// Output: this service cannot run behind Hyperdrive
}

// Most of drops is unaffected — a list of what breaks invites the
// assumption that everything else is suspect.
func ExampleWorks() {
	for _, w := range hyperdrive.Works()[:2] {
		fmt.Println(w)
	}
	// Output:
	// query builders, entities, relations and eager loading
	// DDL and file-based migrations
}

// Provisioning the pooler from the code that owns the database, so
// the configuration ID is not something a human writes down.
func ExampleClient_Create() {
	cf, err := cloudflare.New("your-account-id", cloudflare.WithAPIToken("your-api-token"))
	if err != nil {
		log.Fatal(err)
	}
	hc := hyperdrive.NewClient(cf)
	ctx := context.Background()

	cfg, err := hc.Create(ctx, hyperdrive.NewConfig{
		Name: "app-primary",
		Origin: hyperdrive.Origin{
			Engine:   hyperdrive.PostgreSQL,
			Host:     "db.internal.example.com",
			Database: "app",
			User:     "hyperdrive",
		},
		// Cloudflare stores this and never returns it, so it has to
		// be supplied again on every Replace.
		Password: os.Getenv("ORIGIN_PASSWORD"),
		Caching:  hyperdrive.Caching{MaxAge: 30, StaleWhileRevalidate: 5},
	})
	if err != nil {
		log.Fatal(err)
	}

	// The pooler existing changes nothing about what breaks behind
	// it: this is still the check that belongs at boot.
	if err := hyperdrive.Check(hyperdrive.FeatureListenNotify); err != nil {
		log.Fatal(err)
	}
	fmt.Println("bind this in wrangler.toml:", cfg.ID)
}

// Logical replication wants a direct connection to the origin, past
// the pooler — which is the one thing a Hyperdrive binding cannot
// give you.
func ExampleConfiguration_DirectConfig() {
	var hc *hyperdrive.Client
	ctx := context.Background()

	cfg, err := hc.FindByName(ctx, "app-primary")
	if err != nil {
		log.Fatal(err)
	}
	direct := cfg.DirectConfig(os.Getenv("ORIGIN_PASSWORD"))
	direct.SSLMode = "require" // this hop leaves your network

	dsn, err := direct.DSN()
	if err != nil {
		log.Fatal(err)
	}
	// Open pg.Stream or mirror.LogicalSource on this, not on the
	// Hyperdrive connection string.
	_ = dsn
}
