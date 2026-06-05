package pg

import (
	"database/sql/driver"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/bernardoforcillo/drops"
)

// PostGIS support without forcing the extension into drops core.
// Point is a value type drops marshals as PostGIS-friendly text
// (SRID=4326;POINT(lon lat)) and parses from both WKT and the
// EWKB hex form most drivers return on read. AutoTable maps it
// to `geography(Point, 4326)` so an entity that holds a Point
// field gets a spatial column without manual DDL.
//
//	type Driver struct {
//	    ID       int64    `drop:"id,primaryKey,autoIncrement"`
//	    Position pg.Point `drop:"position,notNull"`
//	}
//
//	// Bolt-style dispatch: closest drivers inside a bounding box
//	nearby, _ := DriverEntity.Query(db).
//	    Where(pg.Within(DriversTable.Col("position"), pg.Box{
//	        SW: pg.Point{Lat: 41.85, Lon: 12.40},
//	        NE: pg.Point{Lat: 41.95, Lon: 12.55},
//	    })).
//	    OrderBy(pg.NearestFrom(DriversTable.Col("position"), userLoc)).
//	    Limit(10).
//	    All(ctx)
//	// emits ORDER BY position <-> 'SRID=4326;POINT(lon lat)'::geography
//	// which uses the KNN index on position when present.
//
// The helpers below cover the 80%: containment, distance,
// nearest-neighbour ordering. For ST_Intersects / ST_Buffer /
// fancier shapes, drop down to drops.Raw — the geography column
// type means PostGIS resolves the expression naturally.

// Point is a (latitude, longitude) pair in WGS84 (SRID 4326).
type Point struct {
	Lat float64
	Lon float64
}

// Value implements driver.Valuer — emits SRID-tagged WKT so the
// PostgreSQL geography type accepts it directly.
func (p Point) Value() (driver.Value, error) {
	return fmt.Sprintf("SRID=4326;POINT(%s %s)",
		strconv.FormatFloat(p.Lon, 'f', -1, 64),
		strconv.FormatFloat(p.Lat, 'f', -1, 64)), nil
}

// Scan implements sql.Scanner. Accepts:
//
//   - WKT text: "POINT(lon lat)" or "SRID=4326;POINT(lon lat)"
//   - EWKB hex: the default form PostGIS returns from a
//     geography column.
//
// Drivers that pre-parse the geometry into a custom type are
// not supported; force text output via ST_AsText if needed.
func (p *Point) Scan(src any) error {
	switch v := src.(type) {
	case nil:
		*p = Point{}
		return nil
	case string:
		return p.parseWKTOrHex(v)
	case []byte:
		return p.parseWKTOrHex(string(v))
	}
	return fmt.Errorf("drops/pg: Point.Scan unsupported src %T", src)
}

func (p *Point) parseWKTOrHex(s string) error {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(strings.ToUpper(s), "SRID=") {
		if i := strings.IndexByte(s, ';'); i >= 0 {
			s = s[i+1:]
		}
	}
	upper := strings.ToUpper(s)
	if strings.HasPrefix(upper, "POINT(") {
		body := s[len("POINT("):]
		if !strings.HasSuffix(body, ")") {
			return errors.New("drops/pg: Point.Scan malformed WKT")
		}
		body = body[:len(body)-1]
		parts := strings.Fields(body)
		if len(parts) != 2 {
			return errors.New("drops/pg: Point.Scan expected two coordinates")
		}
		lon, err := strconv.ParseFloat(parts[0], 64)
		if err != nil {
			return fmt.Errorf("drops/pg: Point.Scan lon: %w", err)
		}
		lat, err := strconv.ParseFloat(parts[1], 64)
		if err != nil {
			return fmt.Errorf("drops/pg: Point.Scan lat: %w", err)
		}
		p.Lat = lat
		p.Lon = lon
		return nil
	}
	// EWKB hex — PostGIS default. Parse the minimum needed for
	// a Point: byte-order, type+SRID, x, y.
	return p.parseEWKBHex(s)
}

// parseEWKBHex decodes the standard PostGIS EWKB-hex form of a
// Point. The layout is:
//
//	1 byte  byte order (00=BE, 01=LE)
//	4 bytes geometry type (lower 16 bits = type; high bits =
//	         optional flags including SRID)
//	[4 bytes SRID]  when the SRID flag is set
//	8 bytes X (float64)
//	8 bytes Y (float64)
//
// Most PostGIS deployments emit little-endian + SRID, so that
// branch is the well-trodden path; big-endian is supported but
// rare.
func (p *Point) parseEWKBHex(s string) error {
	raw, err := hex.DecodeString(s)
	if err != nil {
		return fmt.Errorf("drops/pg: Point.Scan hex: %w", err)
	}
	if len(raw) < 21 {
		return errors.New("drops/pg: Point.Scan EWKB too short")
	}
	le := raw[0] == 0x01
	readU32 := func(b []byte) uint32 {
		if le {
			return uint32(b[0]) | uint32(b[1])<<8 | uint32(b[2])<<16 | uint32(b[3])<<24
		}
		return uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
	}
	typ := readU32(raw[1:5])
	offset := 5
	if typ&0x20000000 != 0 { // SRID flag
		if len(raw) < 25 {
			return errors.New("drops/pg: Point.Scan EWKB SRID truncated")
		}
		offset += 4
	}
	if len(raw) < offset+16 {
		return errors.New("drops/pg: Point.Scan EWKB coords truncated")
	}
	readF64 := func(b []byte) float64 {
		var bits uint64
		if le {
			bits = uint64(b[0]) | uint64(b[1])<<8 | uint64(b[2])<<16 | uint64(b[3])<<24 |
				uint64(b[4])<<32 | uint64(b[5])<<40 | uint64(b[6])<<48 | uint64(b[7])<<56
		} else {
			bits = uint64(b[0])<<56 | uint64(b[1])<<48 | uint64(b[2])<<40 | uint64(b[3])<<32 |
				uint64(b[4])<<24 | uint64(b[5])<<16 | uint64(b[6])<<8 | uint64(b[7])
		}
		return math.Float64frombits(bits)
	}
	p.Lon = readF64(raw[offset : offset+8])
	p.Lat = readF64(raw[offset+8 : offset+16])
	return nil
}

// String renders the canonical SRID-tagged WKT used by Value.
func (p Point) String() string {
	v, _ := p.Value()
	return v.(string)
}

// Box is an axis-aligned bounding box defined by its
// south-west / north-east corners.
type Box struct {
	SW Point
	NE Point
}

// ----------------------------------------------------------------------
// SQL helpers
// ----------------------------------------------------------------------

// Within renders ST_Within(col, ST_MakeEnvelope(...)::geography),
// the canonical "is point inside box" predicate. The box is
// implicit-cast to geography so the result respects the SRID
// declared on col.
func Within(col ColRef, box Box) drops.Expression {
	return drops.ExprFunc(func(b *drops.Builder) {
		b.WriteString("ST_Within(")
		col.col().WriteSQL(b)
		b.WriteString("::geometry, ST_MakeEnvelope($1, $2, $3, $4, 4326))")
		b.AddArg(box.SW.Lon)
		b.AddArg(box.SW.Lat)
		b.AddArg(box.NE.Lon)
		b.AddArg(box.NE.Lat)
	})
}

// DistanceFrom renders ST_Distance(col, point) — distance in
// metres when col is a geography column. Suitable for SELECT
// projections and ORDER BY:
//
//	q.OrderBy(pg.DistanceFrom(DriversTable.Col("position"), userLoc))
//
// For nearest-N queries that should use the spatial KNN index,
// prefer NearestFrom (which emits the <-> operator).
func DistanceFrom(col ColRef, p Point) drops.Expression {
	return drops.ExprFunc(func(b *drops.Builder) {
		b.WriteString("ST_Distance(")
		col.col().WriteSQL(b)
		b.WriteString(", $1::geography)")
		b.AddArg(p.String())
	})
}

// NearestFrom emits the `<->` operator (KNN distance) which
// PostgreSQL can plan against a GiST or SP-GiST index on col.
// Use as an ORDER BY term; the result is ordered by
// closest-first.
func NearestFrom(col ColRef, p Point) drops.Expression {
	return drops.ExprFunc(func(b *drops.Builder) {
		col.col().WriteSQL(b)
		b.WriteString(" <-> $1::geography")
		b.AddArg(p.String())
	})
}

// WithinRadius renders ST_DWithin(col, point, metres) — true
// when col is within metres of point. Uses the spherical /
// spheroidal distance for geography columns, so the result
// matches the canonical "X km radius" semantics.
func WithinRadius(col ColRef, p Point, metres float64) drops.Expression {
	return drops.ExprFunc(func(b *drops.Builder) {
		b.WriteString("ST_DWithin(")
		col.col().WriteSQL(b)
		b.WriteString(", $1::geography, $2)")
		b.AddArg(p.String())
		b.AddArg(metres)
	})
}
