package hyperdrive_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/bernardoforcillo/drops/cloudflare/hyperdrive"
)

func TestDSNDefaultsToPostgres(t *testing.T) {
	dsn, err := hyperdrive.Config{
		Host:     "hyperdrive.internal",
		Database: "tenders",
		User:     "app",
		Password: "secret",
	}.DSN()
	if err != nil {
		t.Fatalf("DSN: %v", err)
	}
	want := "postgresql://app:secret@hyperdrive.internal:5432/tenders"
	if dsn != want {
		t.Errorf("DSN = %q, want %q", dsn, want)
	}
}

func TestDSNMySQLPort(t *testing.T) {
	dsn, err := hyperdrive.Config{
		Engine:   hyperdrive.MySQL,
		Host:     "h",
		Database: "d",
		User:     "u",
	}.DSN()
	if err != nil {
		t.Fatalf("DSN: %v", err)
	}
	if !strings.HasPrefix(dsn, "mysql://") || !strings.Contains(dsn, ":3306/") {
		t.Errorf("DSN = %q, want a mysql:// URL on 3306", dsn)
	}
}

// A generated password containing a "/" or an "@" makes a URL that
// parses as a different host, and the failure that follows names
// neither the password nor the escaping.
func TestPasswordIsEscaped(t *testing.T) {
	dsn, err := hyperdrive.Config{
		Host:     "h",
		Database: "d",
		User:     "u",
		Password: "p@ss/w:rd?",
	}.DSN()
	if err != nil {
		t.Fatalf("DSN: %v", err)
	}
	if strings.Contains(dsn, "p@ss/w:rd?") {
		t.Errorf("DSN = %q — the password went in unescaped", dsn)
	}
	if !strings.Contains(dsn, "@h:5432/d") {
		t.Errorf("DSN = %q, want the host intact after the credentials", dsn)
	}
}

func TestDSNNamesEveryMissingField(t *testing.T) {
	_, err := hyperdrive.Config{}.DSN()
	if !errors.Is(err, hyperdrive.ErrIncompleteConfig) {
		t.Fatalf("err = %v, want ErrIncompleteConfig", err)
	}
	for _, field := range []string{"Host", "Database", "User"} {
		if !strings.Contains(err.Error(), field) {
			t.Errorf("err = %v, want it to name %s", err, field)
		}
	}
}

func TestDSNRejectsAnUnknownEngine(t *testing.T) {
	_, err := hyperdrive.Config{Engine: "sqlite", Host: "h", Database: "d", User: "u"}.DSN()
	if !errors.Is(err, hyperdrive.ErrIncompleteConfig) {
		t.Errorf("err = %v, want ErrIncompleteConfig", err)
	}
}

// A DSN that reorders between runs is not comparable in a test and
// not diffable in a log.
func TestDSNParamsAreStable(t *testing.T) {
	cfg := hyperdrive.Config{
		Host: "h", Database: "d", User: "u",
		SSLMode: "require",
		Params: map[string]string{
			"application_name":     "tendersbay",
			"connect_timeout":      "5",
			"target_session_attrs": "read-write",
		},
	}
	first, err := cfg.DSN()
	if err != nil {
		t.Fatalf("DSN: %v", err)
	}
	for i := 0; i < 20; i++ {
		again, err := cfg.DSN()
		if err != nil {
			t.Fatalf("DSN: %v", err)
		}
		if again != first {
			t.Fatalf("DSN is not stable:\n %s\n %s", first, again)
		}
	}
	if !strings.Contains(first, "sslmode=require") {
		t.Errorf("DSN = %q, want sslmode", first)
	}
}

// Check --------------------------------------------------------------

func TestCheckPassesForNothing(t *testing.T) {
	if err := hyperdrive.Check(); err != nil {
		t.Errorf("Check() = %v, want nil", err)
	}
}

func TestCheckNamesTheSymbolsThatStopWorking(t *testing.T) {
	err := hyperdrive.Check(hyperdrive.FeatureListenNotify)
	if !errors.Is(err, hyperdrive.ErrUnsupported) {
		t.Fatalf("err = %v, want ErrUnsupported", err)
	}
	// The symbol is what a reader greps for, so the message has to
	// carry it rather than only the feature name.
	if !strings.Contains(err.Error(), "pg.Listen") {
		t.Errorf("err = %v, want it to name pg.Listen", err)
	}
	if !strings.Contains(err.Error(), "pooler returns the connection") {
		t.Errorf("err = %v, want it to say why", err)
	}
}

func TestCheckReportsEveryFeatureAtOnce(t *testing.T) {
	err := hyperdrive.Check(hyperdrive.Everything()...)
	if err == nil {
		t.Fatal("Check(Everything()) should fail")
	}
	for _, f := range hyperdrive.Everything() {
		if !strings.Contains(err.Error(), string(f)) {
			t.Errorf("err does not mention %s", f)
		}
	}
}

func TestCheckIgnoresAnUnknownFeature(t *testing.T) {
	if err := hyperdrive.Check(hyperdrive.Feature("something-else")); err != nil {
		t.Errorf("Check of an unknown feature = %v, want nil", err)
	}
}

// Every listed feature has to carry a symbol and a mechanism, or the
// list is just a set of names.
func TestEveryFeatureIsDescribed(t *testing.T) {
	features := hyperdrive.Everything()
	if len(features) == 0 {
		t.Fatal("Everything() is empty")
	}
	for _, f := range features {
		u, ok := hyperdrive.Describe(f)
		if !ok {
			t.Errorf("%s has no description", f)
			continue
		}
		if len(u.Symbols) == 0 {
			t.Errorf("%s names no symbols", f)
		}
		if u.Why == "" {
			t.Errorf("%s says no why", f)
		}
		if !strings.HasSuffix(u.Why, ".") {
			t.Errorf("%s: why should be a sentence, got %q", f, u.Why)
		}
	}
}

func TestReportMarksTheSilentFailures(t *testing.T) {
	r := hyperdrive.Report()
	if !strings.Contains(r, "fails silently") {
		t.Error("Report does not mark the silent failures, which are the dangerous ones")
	}
	for _, f := range hyperdrive.Everything() {
		if !strings.Contains(r, string(f)) {
			t.Errorf("Report omits %s", f)
		}
	}
}

// A list of what breaks invites the assumption that everything else
// is suspect, so the package says what keeps working too.
func TestWorksIsNotEmpty(t *testing.T) {
	w := hyperdrive.Works()
	if len(w) < 5 {
		t.Errorf("Works() lists %d things; the answer is most of drops", len(w))
	}
	joined := strings.Join(w, "\n")
	for _, want := range []string{"migrations", "transactions", "outbox"} {
		if !strings.Contains(joined, want) {
			t.Errorf("Works() does not mention %s", want)
		}
	}
}
