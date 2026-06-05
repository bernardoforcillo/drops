package pg_test

import (
	"strings"
	"testing"

	"github.com/bernardoforcillo/drops"
	"github.com/bernardoforcillo/drops/pg"
)

func TestPointValueEmitsSRIDTaggedWKT(t *testing.T) {
	p := pg.Point{Lat: 41.9028, Lon: 12.4964} // Rome
	v, err := p.Value()
	if err != nil {
		t.Fatal(err)
	}
	got := v.(string)
	if !strings.HasPrefix(got, "SRID=4326;POINT(") {
		t.Errorf("Value: %q", got)
	}
	if !strings.Contains(got, "12.4964") || !strings.Contains(got, "41.9028") {
		t.Errorf("Value missing lat/lon: %q", got)
	}
}

func TestPointScanWKT(t *testing.T) {
	var p pg.Point
	if err := p.Scan("POINT(12.4964 41.9028)"); err != nil {
		t.Fatal(err)
	}
	if p.Lat != 41.9028 || p.Lon != 12.4964 {
		t.Errorf("Scan WKT: %+v", p)
	}
}

func TestPointScanSRIDWKT(t *testing.T) {
	var p pg.Point
	if err := p.Scan("SRID=4326;POINT(12.4964 41.9028)"); err != nil {
		t.Fatal(err)
	}
	if p.Lat != 41.9028 || p.Lon != 12.4964 {
		t.Errorf("Scan SRID WKT: %+v", p)
	}
}

func TestPointScanEWKBHexRoundTrip(t *testing.T) {
	// EWKB for SRID=4326 POINT(12 41), little-endian, SRID flag.
	// Built by PostgreSQL: 0101000020E6100000000000000000284000000000008044400
	// Use a known fixture from PostGIS docs for this LON/LAT.
	hexFixture := "0101000020E61000000000000000002840000000000080444"
	hexFixture += "0" // pad to even hex
	var p pg.Point
	if err := p.Scan(hexFixture); err != nil {
		t.Fatalf("Scan EWKB: %v", err)
	}
	if p.Lon < 11.99 || p.Lon > 12.01 {
		t.Errorf("EWKB lon: %v", p.Lon)
	}
	if p.Lat < 40.99 || p.Lat > 41.01 {
		t.Errorf("EWKB lat: %v", p.Lat)
	}
}

func TestPointScanNil(t *testing.T) {
	var p pg.Point
	if err := p.Scan(nil); err != nil {
		t.Fatal(err)
	}
}

func TestPointScanUnsupportedType(t *testing.T) {
	var p pg.Point
	if err := p.Scan(42); err == nil {
		t.Error("unsupported src type should error")
	}
}

func TestWithinEmitsEnvelope(t *testing.T) {
	tbl := pg.NewTable("drivers")
	pos := pg.Add(tbl, pg.BigSerial("id").PrimaryKey())
	_ = pos
	posCol := pg.Add(tbl, pg.Custom[pg.Point]("position", "geography(Point,4326)"))
	expr := pg.Within(posCol, pg.Box{
		SW: pg.Point{Lat: 41.85, Lon: 12.40},
		NE: pg.Point{Lat: 41.95, Lon: 12.55},
	})
	sql, args := drops.String(expr)
	if !strings.Contains(sql, "ST_Within") {
		t.Errorf("Within must use ST_Within: %s", sql)
	}
	if !strings.Contains(sql, "ST_MakeEnvelope") {
		t.Errorf("Within must use ST_MakeEnvelope: %s", sql)
	}
	if len(args) != 4 {
		t.Errorf("expected 4 args (sw lon/lat, ne lon/lat), got %d", len(args))
	}
}

func TestDistanceFromAndNearestFrom(t *testing.T) {
	tbl := pg.NewTable("drivers")
	posCol := pg.Add(tbl, pg.Custom[pg.Point]("position", "geography(Point,4326)"))

	d := pg.DistanceFrom(posCol, pg.Point{Lat: 41.9, Lon: 12.5})
	sql, _ := drops.String(d)
	if !strings.Contains(sql, "ST_Distance(") {
		t.Errorf("DistanceFrom must use ST_Distance: %s", sql)
	}

	n := pg.NearestFrom(posCol, pg.Point{Lat: 41.9, Lon: 12.5})
	sql, _ = drops.String(n)
	if !strings.Contains(sql, "<->") {
		t.Errorf("NearestFrom must use <-> operator: %s", sql)
	}
}

func TestWithinRadius(t *testing.T) {
	tbl := pg.NewTable("drivers")
	posCol := pg.Add(tbl, pg.Custom[pg.Point]("position", "geography(Point,4326)"))
	expr := pg.WithinRadius(posCol, pg.Point{Lat: 41.9, Lon: 12.5}, 1500)
	sql, args := drops.String(expr)
	if !strings.Contains(sql, "ST_DWithin") {
		t.Errorf("WithinRadius must use ST_DWithin: %s", sql)
	}
	if len(args) != 2 {
		t.Errorf("expected 2 args (point, metres), got %d: %v", len(args), args)
	}
}

func TestPointAutoTableMapsToGeography(t *testing.T) {
	type driverRow struct {
		ID       int64    `drop:"id,primaryKey,autoIncrement"`
		Position pg.Point `drop:"position,notNull"`
	}
	tbl := pg.AutoTable[driverRow]("drivers")
	col := tbl.Col("position")
	if col == nil {
		t.Fatal("position column missing")
	}
	if !strings.Contains(col.Type().TypeSQL(), "geography(Point") {
		t.Errorf("Point should map to geography(Point,4326), got %q", col.Type().TypeSQL())
	}
}

func TestPointStringIsCanonical(t *testing.T) {
	p := pg.Point{Lat: 1, Lon: 2}
	if p.String() != "SRID=4326;POINT(2 1)" {
		t.Errorf("String: %q", p.String())
	}
}
