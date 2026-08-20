package pgwire

import "testing"

func TestParseDSNURL(t *testing.T) {
	t.Setenv("PGPASSWORD", "")
	cfg, err := parseDSN("postgres://alice:s%2Fcret@db.internal:6432/shop?sslmode=require&application_name=drops")
	if err != nil {
		t.Fatalf("parseDSN: %v", err)
	}
	if cfg.User != "alice" || cfg.Password != "s/cret" {
		t.Errorf("credentials = %q / %q", cfg.User, cfg.Password)
	}
	if cfg.Host != "db.internal" || cfg.Port != "6432" {
		t.Errorf("address = %s:%s", cfg.Host, cfg.Port)
	}
	if cfg.Database != "shop" {
		t.Errorf("database = %q", cfg.Database)
	}
	if cfg.SSLMode != "require" {
		t.Errorf("sslmode = %q", cfg.SSLMode)
	}
	if cfg.Options["application_name"] != "drops" {
		t.Errorf("options = %v", cfg.Options)
	}
}

func TestParseDSNKeywords(t *testing.T) {
	cfg, err := parseDSN(`host=/var/run/postgresql user=drops dbname=drops password='p a\'ss' sslmode=disable`)
	if err != nil {
		t.Fatalf("parseDSN: %v", err)
	}
	if cfg.Password != `p a'ss` {
		t.Errorf("password = %q", cfg.Password)
	}
	network, address := cfg.address()
	if network != "unix" || address != "/var/run/postgresql/.s.PGSQL.5432" {
		t.Errorf("address = %s %s", network, address)
	}
}

// libpq's defaults, which a user who has PGHOST set expects to hold.
func TestParseDSNFallsBackToEnvironment(t *testing.T) {
	t.Setenv("PGHOST", "elsewhere")
	t.Setenv("PGUSER", "envuser")
	t.Setenv("PGPASSWORD", "envpass")
	t.Setenv("PGDATABASE", "")
	cfg, err := parseDSN("")
	if err != nil {
		t.Fatalf("parseDSN: %v", err)
	}
	if cfg.Host != "elsewhere" || cfg.User != "envuser" || cfg.Password != "envpass" {
		t.Fatalf("config = %+v", cfg)
	}
	// libpq names the database after the user when neither says.
	if cfg.Database != "envuser" {
		t.Errorf("database = %q, want the user name", cfg.Database)
	}
	if cfg.SSLMode != "prefer" {
		t.Errorf("sslmode = %q, want libpq's default", cfg.SSLMode)
	}
}

func TestParseDSNRejectsNonsense(t *testing.T) {
	for _, dsn := range []string{"mysql://root@localhost/db", "just some words"} {
		if _, err := parseDSN(dsn); err == nil {
			t.Errorf("parseDSN(%q) accepted a connection string it cannot honour", dsn)
		}
	}
}

func TestParseDSNRequiresAUser(t *testing.T) {
	t.Setenv("PGUSER", "")
	t.Setenv("USER", "")
	if _, err := parseDSN("postgres://localhost/shop"); err == nil {
		t.Error("parseDSN accepted a connection string with no user anywhere")
	}
}
