package mysql

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math/rand/v2"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// GenerateOptions configures a single migration-generation run.
//
// The default behaviour reads from and writes to a single Dir on disk.
// Every FS-touching field can be overridden for tests, or to embed the
// generator in tooling that captures the output in memory.
type GenerateOptions struct {
	// Schema is the current desired schema. Required.
	Schema *Schema

	// Dir is the migration directory — the root for reads (when FS is
	// nil) and the destination for writes (when Write is nil).
	// Required.
	Dir string

	// Name is the suffix appended to the migration tag: "init"
	// produces "0000_init". Empty generates a random two-word name.
	Name string

	// FS overrides reads. When set, the journal and the previous
	// snapshot are read through it. When nil, os.DirFS(".") is used.
	FS fs.FS

	// Write overrides file writes, receiving a path relative to Dir.
	// When nil, files are written under Dir with os.WriteFile.
	Write func(relPath string, data []byte) error

	// Now overrides the timestamp written into journal entries (unix
	// milliseconds). When nil, time.Now().UnixMilli() is used.
	Now func() int64

	// NameFn overrides the random-name generator used when Name is
	// empty.
	NameFn func() string

	// Safe adds IF [NOT] EXISTS where MySQL has it. See
	// DiffOptions.Safe, which on this dialect is much weaker than on
	// PostgreSQL's.
	Safe bool

	// Server is the server the migration is being generated for. It
	// decides the few renderings that are not a property of the schema;
	// leaving it zero generates the conservative reading of both.
	Server ServerInfo

	// SplitAlters emits one ALTER TABLE per column change instead of
	// batching a table's changes into one statement. See
	// DiffOptions.SplitAlters — on a generated migration the trade is
	// sharper than on a push, because the file is what a human reads
	// when the migration stops half-way.
	SplitAlters bool

	// WithDown emits a paired <tag>.down.sql containing
	// DiffDown(prev, cur).
	//
	// The down direction is best-effort and applies cleanly only when
	// the up direction's inverse is well-defined. DROP COLUMN can never
	// be reversed losslessly because the data is gone, and on MySQL
	// nothing rolls back on its own: review a generated down script
	// before relying on it.
	WithDown bool
}

// GenerateResult describes what a run produced.
type GenerateResult struct {
	Tag      string // e.g. "0003_warm_harbor"; empty when NoOp
	Idx      int    // sequence index for the new migration
	SQL      string // statement-breakpoint-joined migration SQL (up)
	DownSQL  string // rollback SQL; empty unless WithDown was set
	NoOp     bool   // true when prev and cur are equivalent
	Snapshot []byte // bytes written to meta/<idx>_snapshot.json
	Journal  []byte // bytes written to meta/_journal.json
}

// GenerateMigration computes the schema diff and writes a new migration
// set: <tag>.sql, meta/<idx>_snapshot.json and an updated
// meta/_journal.json. The layout is drizzle-kit's, so the same
// directory can be read by other tooling that knows it.
//
// It is a no-op when the Go schema and the latest snapshot agree; no
// files are written in that case.
//
// Generating beats pushing on this dialect for a reason that is
// sharper here than elsewhere: a migration that fails half-way cannot
// be rolled back, so the statements have to be readable by whoever
// works out what state the database is in. That is a file, not a diff
// computed at deploy time.
func GenerateMigration(opts GenerateOptions) (*GenerateResult, error) {
	if opts.Schema == nil {
		return nil, errors.New("drops/mysql: Schema is required")
	}
	if opts.Dir == "" {
		return nil, errors.New("drops/mysql: Dir is required")
	}
	if opts.Now == nil {
		opts.Now = func() int64 { return time.Now().UnixMilli() }
	}
	if opts.NameFn == nil {
		opts.NameFn = randomMigrationName
	}
	if opts.FS == nil {
		// Read from the same tree we write to, rooted at the working
		// directory so Dir means the same thing on both sides.
		opts.FS = os.DirFS(".")
	}
	if opts.Write == nil {
		opts.Write = func(rel string, data []byte) error {
			full := filepath.Join(opts.Dir, rel)
			if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
				return err
			}
			return os.WriteFile(full, data, 0o644)
		}
	}

	prev, idx, err := loadPrevSnapshot(opts.FS, opts.Dir)
	if err != nil {
		return nil, err
	}
	cur := BuildSnapshot(opts.Schema)
	cur.PrevID = prev.ID

	diffOpts := DiffOptions{Safe: opts.Safe, Server: opts.Server, SplitAlters: opts.SplitAlters}
	statements := Diff(prev, cur, diffOpts)
	if len(statements) == 0 {
		return &GenerateResult{NoOp: true}, nil
	}

	name := opts.Name
	if name == "" {
		name = opts.NameFn()
	}
	tag := fmt.Sprintf("%04d_%s", idx, name)

	sql := strings.Join(statements, "\n--> statement-breakpoint\n") + "\n"
	snapshotBytes, err := cur.Marshal()
	if err != nil {
		return nil, err
	}

	journal, err := loadJournalForWrite(opts.FS, opts.Dir)
	if err != nil {
		return nil, err
	}
	journal.Entries = append(journal.Entries, migrationJournalEntry{
		Idx:         idx,
		Version:     "5",
		When:        opts.Now(),
		Tag:         tag,
		Breakpoints: true,
	})
	journal.Version = "5"
	journal.Dialect = "mysql"
	journalBytes, err := marshalJournal(journal)
	if err != nil {
		return nil, err
	}

	var downSQL string
	if opts.WithDown {
		if down := DiffDown(prev, cur, diffOpts); len(down) > 0 {
			downSQL = strings.Join(down, "\n--> statement-breakpoint\n") + "\n"
		}
	}

	if err := opts.Write(tag+".sql", []byte(sql)); err != nil {
		return nil, fmt.Errorf("drops/mysql: write migration SQL: %w", err)
	}
	if downSQL != "" {
		if err := opts.Write(tag+".down.sql", []byte(downSQL)); err != nil {
			return nil, fmt.Errorf("drops/mysql: write down SQL: %w", err)
		}
	}
	if err := opts.Write(fmt.Sprintf("meta/%04d_snapshot.json", idx), snapshotBytes); err != nil {
		return nil, fmt.Errorf("drops/mysql: write snapshot: %w", err)
	}
	if err := opts.Write("meta/_journal.json", journalBytes); err != nil {
		return nil, fmt.Errorf("drops/mysql: write journal: %w", err)
	}

	return &GenerateResult{
		Tag:      tag,
		Idx:      idx,
		SQL:      sql,
		DownSQL:  downSQL,
		Snapshot: snapshotBytes,
		Journal:  journalBytes,
	}, nil
}

// migrationJournal mirrors meta/_journal.json.
type migrationJournal struct {
	Version string                  `json:"version"`
	Dialect string                  `json:"dialect"`
	Entries []migrationJournalEntry `json:"entries"`
}

type migrationJournalEntry struct {
	Idx         int    `json:"idx"`
	Version     string `json:"version"`
	When        int64  `json:"when"`
	Tag         string `json:"tag"`
	Breakpoints bool   `json:"breakpoints"`
}

// loadPrevSnapshot reads the snapshot with the highest index in meta/
// and returns it with the next index to use. A directory with no
// snapshots yields the empty snapshot and index 0.
func loadPrevSnapshot(fsys fs.FS, dir string) (*Snapshot, int, error) {
	metaDir := path.Join(dir, "meta")
	entries, err := fs.ReadDir(fsys, metaDir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return EmptySnapshot(), 0, nil
		}
		return nil, 0, fmt.Errorf("drops/mysql: read meta dir: %w", err)
	}

	type snap struct {
		idx  int
		name string
	}
	var snaps []snap
	for _, e := range entries {
		n := e.Name()
		if !strings.HasSuffix(n, "_snapshot.json") {
			continue
		}
		idx, perr := atoiPadded(strings.TrimSuffix(n, "_snapshot.json"))
		if perr != nil {
			continue
		}
		snaps = append(snaps, snap{idx: idx, name: n})
	}
	if len(snaps) == 0 {
		return EmptySnapshot(), 0, nil
	}
	sort.Slice(snaps, func(i, j int) bool { return snaps[i].idx < snaps[j].idx })
	latest := snaps[len(snaps)-1]
	body, err := fs.ReadFile(fsys, path.Join(metaDir, latest.name))
	if err != nil {
		return nil, 0, fmt.Errorf("drops/mysql: read snapshot %s: %w", latest.name, err)
	}
	prev, err := UnmarshalSnapshot(body)
	if err != nil {
		return nil, 0, err
	}
	return prev, latest.idx + 1, nil
}

// loadJournalForWrite reads the existing journal or returns a fresh one.
func loadJournalForWrite(fsys fs.FS, dir string) (*migrationJournal, error) {
	body, err := fs.ReadFile(fsys, path.Join(dir, "meta", "_journal.json"))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return &migrationJournal{Version: "5", Dialect: "mysql"}, nil
		}
		return nil, fmt.Errorf("drops/mysql: read journal: %w", err)
	}
	var j migrationJournal
	if err := json.Unmarshal(body, &j); err != nil {
		return nil, fmt.Errorf("drops/mysql: parse journal: %w", err)
	}
	if j.Dialect != "" && j.Dialect != "mysql" {
		return nil, fmt.Errorf("drops/mysql: journal dialect is %q; this generator writes mysql", j.Dialect)
	}
	return &j, nil
}

func marshalJournal(j *migrationJournal) ([]byte, error) {
	body, err := json.MarshalIndent(j, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(body, '\n'), nil
}

// atoiPadded converts a zero-padded numeric string to an int.
func atoiPadded(s string) (int, error) {
	if s == "" {
		return 0, errors.New("empty")
	}
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("non-digit in %q", s)
		}
		n = n*10 + int(c-'0')
	}
	return n, nil
}

// migrationAdjectives / migrationNouns produce friendly tags like
// "ancient_forest". A custom NameFn replaces them entirely.
var (
	migrationAdjectives = []string{
		"ancient", "bold", "calm", "deep", "eager", "free", "gentle",
		"happy", "icy", "jolly", "keen", "lucky", "mighty", "noble",
		"olive", "proud", "quiet", "rapid", "silent", "tender", "urban",
		"vivid", "warm", "young", "zealous",
	}
	migrationNouns = []string{
		"forest", "mountain", "river", "ocean", "valley", "summit",
		"meadow", "harbor", "lagoon", "canyon", "glacier", "savanna",
		"plateau", "tundra", "delta", "fjord", "isthmus", "atoll",
		"prairie", "estuary", "ridge", "vale", "bay", "cape", "isle",
	}
)

func randomMigrationName() string {
	return migrationAdjectives[rand.IntN(len(migrationAdjectives))] +
		"_" + migrationNouns[rand.IntN(len(migrationNouns))]
}
