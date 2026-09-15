package hyperdrive_test

import (
	"fmt"
	"log"

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
