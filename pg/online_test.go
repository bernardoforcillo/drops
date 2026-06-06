package pg_test

import (
	"strings"
	"testing"

	"github.com/bernardoforcillo/drops"
	"github.com/bernardoforcillo/drops/pg"
)

func TestAnalyzeSpotsDropTable(t *testing.T) {
	risks := pg.AnalyzeMigrationRisks([]string{`DROP TABLE "users";`})
	if len(risks) != 1 {
		t.Fatalf("expected 1 risk, got %d", len(risks))
	}
	if risks[0].Level != pg.RiskDanger {
		t.Errorf("DROP TABLE should be danger, got %s", risks[0].Level)
	}
	if !strings.Contains(risks[0].Reason, "irreversible") {
		t.Errorf("reason missing context: %s", risks[0].Reason)
	}
}

func TestAnalyzeSpotsAlterColumnTYPE(t *testing.T) {
	risks := pg.AnalyzeMigrationRisks([]string{
		`ALTER TABLE "users" ALTER COLUMN "name" TYPE varchar(64);`,
	})
	if len(risks) != 1 || risks[0].Level != pg.RiskDanger {
		t.Errorf("ALTER COLUMN TYPE: %+v", risks)
	}
}

func TestAnalyzeSuggestsConcurrentlyForCreateIndex(t *testing.T) {
	risks := pg.AnalyzeMigrationRisks([]string{
		`CREATE INDEX users_email_idx ON users (email);`,
	})
	if len(risks) != 1 {
		t.Fatalf("expected 1 risk, got %d", len(risks))
	}
	if !strings.Contains(risks[0].Suggestion, "CONCURRENTLY") {
		t.Errorf("suggestion should mention CONCURRENTLY: %s", risks[0].Suggestion)
	}
}

func TestAnalyzeAllowsConcurrentlyIndex(t *testing.T) {
	risks := pg.AnalyzeMigrationRisks([]string{
		`CREATE INDEX CONCURRENTLY users_email_idx ON users (email);`,
	})
	if len(risks) != 0 {
		t.Errorf("CONCURRENTLY should not flag, got %+v", risks)
	}
}

func TestAnalyzeFKWithoutNotValid(t *testing.T) {
	risks := pg.AnalyzeMigrationRisks([]string{
		`ALTER TABLE posts ADD CONSTRAINT fk FOREIGN KEY (user_id) REFERENCES users(id);`,
	})
	if len(risks) != 1 || risks[0].Level != pg.RiskWarn {
		t.Errorf("FK without NOT VALID: %+v", risks)
	}
}

func TestAnalyzeAllowsFKWithNotValid(t *testing.T) {
	risks := pg.AnalyzeMigrationRisks([]string{
		`ALTER TABLE posts ADD CONSTRAINT fk FOREIGN KEY (user_id) REFERENCES users(id) NOT VALID;`,
	})
	if len(risks) != 0 {
		t.Errorf("FK NOT VALID should be allowed, got %+v", risks)
	}
}

func TestAnalyzeAddColumnNotNull(t *testing.T) {
	risks := pg.AnalyzeMigrationRisks([]string{
		`ALTER TABLE users ADD COLUMN locked boolean NOT NULL;`,
	})
	if len(risks) != 1 || risks[0].Level != pg.RiskWarn {
		t.Errorf("ADD COLUMN NOT NULL should warn, got %+v", risks)
	}
}

func TestHasDangerousMigration(t *testing.T) {
	if !pg.HasDangerousMigration([]string{`DROP TABLE users;`}) {
		t.Error("DROP TABLE should be flagged dangerous")
	}
	if pg.HasDangerousMigration([]string{`CREATE INDEX CONCURRENTLY i ON t (c);`}) {
		t.Error("safe statement should not flag")
	}
}

func TestCreateIndexOnlineEmitsConcurrently(t *testing.T) {
	tbl := pg.NewTable("widgets")
	id := pg.Add(tbl, pg.BigSerial("id").PrimaryKey())
	idx := pg.NewIndex("widgets_id_idx", tbl, id)
	expr := pg.CreateIndexOnline(idx)
	sql, _ := drops.String(expr)
	if !strings.Contains(sql, "CONCURRENTLY") {
		t.Errorf("CreateIndexOnline must emit CONCURRENTLY, got: %s", sql)
	}
}
