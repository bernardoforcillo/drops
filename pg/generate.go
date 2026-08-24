package pg

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
// All FS-touching fields can be overridden for tests or for embedding
// the generator in tooling that wants to capture output in memory.
type GenerateOptions struct {
	// Schema is the current desired schema. Required.
	Schema *Schema

	// Dir is the migration directory — both the root for FS reads (if FS
	// is nil) and the destination for Write (if Write is nil). Required.
	Dir string

	// Name is the suffix appended to the migration tag — e.g. "init"
	// produces "0000_init". If empty, a random two-word name is generated.
	Name string

	// FS is an optional override for reads. When set, the journal and
	// previous snapshot are read from FS (paths relative to Dir become
	// paths relative to the FS root). When nil, os.DirFS is used.
	FS fs.FS

	// Write is an optional override for file writes. When set, it is
	// called for each output file (relative path within Dir + bytes).
	// When nil, files are written to disk under Dir with os.WriteFile.
	Write func(relPath string, data []byte) error

	// Now overrides the timestamp written into journal entries (unix
	// milliseconds). When nil, time.Now().UnixMilli() is used.
	Now func() int64

	// NameFn overrides the random-name generator used when Name is empty.
	NameFn func() string

	// Safe wraps every destructive or creative DDL in IF [NOT] EXISTS
	// so the migration can be re-run idempotently. See DiffOptions.Safe.
	Safe bool

	// WithDown enables auto-generated rollback SQL. When true, the
	// generator emits a paired <tag>.down.sql file alongside the
	// up SQL containing DiffDown(prev, cur). The down direction is
	// best-effort and applies cleanly only when the up direction's
	// inverse is itself well-defined (column ADD ↔ DROP, type ↔
	// type swap, etc.); DROP COLUMN can never be reversed
	// losslessly because the data is gone — review generated down
	// scripts before relying on them.
	WithDown bool

	// Renames answers the rename questions this run raises, for a caller
	// that already knows the answer — a command line, a prompt, a test.
	// Answers are merged over the ones already recorded in
	// RenameLogFile, with these winning, and the merged set is written
	// back so the next run does not ask again.
	//
	// A candidate left unanswered by both is not guessed at: the run
	// returns *RenameAmbiguityError and writes nothing. See rename.go.
	Renames []RenameDecision
}

// GenerateResult describes what a Run produced.
type GenerateResult struct {
	Tag      string   // e.g. "0003_warm_iron_man"; empty when NoOp
	Idx      int      // sequence index for the new migration
	SQL      string   // statement-breakpoint-joined migration SQL (up)
	DownSQL  string   // rollback SQL; empty unless WithDown was set
	NoOp     bool     // true when prev and cur snapshots are equivalent
	Snapshot []byte   // bytes written to meta/<idx>_snapshot.json
	Journal  []byte   // bytes written to meta/_journal.json
	Renames  []Rename // the renames this migration performs, if any

	// RenameLog is what was written to RenameLogFile, or nil when
	// there was no rename decision to record.
	RenameLog []byte
}

// GenerateMigration computes the schema diff and writes a new drizzle-kit
// migration set: <tag>.sql, meta/<idx>_snapshot.json and an updated
// meta/_journal.json.
//
// It is a no-op when there are no differences between the current Go
// schema and the latest snapshot; no migration files are written in
// that case. The one thing a no-op run still writes is RenameLogFile,
// and only when this run was given an answer to record — a decision
// that settles a question without moving the schema is still a
// decision, and leaving it unrecorded means being asked again.
//
// It refuses, returning *RenameAmbiguityError and writing nothing, when
// the change could be a rename and nothing on disk or in the options
// says whether it is. Answer with GenerateOptions.Renames; the answer
// is recorded in RenameLogFile and replayed from then on. See rename.go
// for why guessing is not on the list of options.
func GenerateMigration(opts GenerateOptions) (*GenerateResult, error) {
	if opts.Schema == nil {
		return nil, errors.New("drops/pg: Schema is required")
	}
	if opts.Dir == "" {
		return nil, errors.New("drops/pg: Dir is required")
	}
	if opts.Now == nil {
		opts.Now = func() int64 { return time.Now().UnixMilli() }
	}
	if opts.NameFn == nil {
		opts.NameFn = randomName
	}
	if opts.FS == nil {
		// Read from the same directory we write to. We use the parent
		// because os.DirFS rooted at Dir would make "meta/_journal.json"
		// and ".sql" paths relative to Dir; we want to use a unified
		// path scheme keyed on Dir.
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

	recorded, err := loadRenameLog(opts.FS, opts.Dir)
	if err != nil {
		return nil, err
	}
	decisions := mergeDecisions(recorded, opts.Renames)
	// The schema can state a rename of its own — see DeclaredRenames.
	// That is a fact the schema carries, not an answer somebody gave
	// to a question, so it settles the question without being written
	// into the log, and a recorded answer wins where the two disagree.
	resolving := mergeDecisions(DeclaredRenames(opts.Schema), decisions)
	renames, unresolved := ResolveRenames(prev, cur, resolving)
	if len(unresolved) > 0 {
		// Nothing is written. A migration that turns a rename into a
		// DROP COLUMN is worse than no migration at all, and this is the
		// only point at which drops can still say so.
		return nil, &RenameAmbiguityError{Candidates: unresolved}
	}
	if err := validateRenames(prev, cur, renames); err != nil {
		return nil, err
	}

	diffOpts := DiffOptions{Safe: opts.Safe, Renames: renames}
	statements := Diff(prev, cur, diffOpts)

	var renameLogBytes []byte
	if len(decisions) > 0 {
		if renameLogBytes, err = marshalRenameLog(decisions); err != nil {
			return nil, err
		}
	}
	// A no-op run still records an answer it was just given. Not every
	// decision moves the schema: saying "no, that column really is
	// going" about a column the diff no longer proposes anything for
	// settles the question and produces no statement, and returning
	// here without writing it meant the same question came back on the
	// next run — with the same nobody to answer it. Nothing is written
	// when no answer was given this run, so a settled schema stays
	// silent.
	if len(statements) == 0 {
		if len(opts.Renames) == 0 {
			return &GenerateResult{NoOp: true}, nil
		}
		if err := opts.Write(RenameLogFile, renameLogBytes); err != nil {
			return nil, fmt.Errorf("drops/pg: write rename log: %w", err)
		}
		return &GenerateResult{NoOp: true, RenameLog: renameLogBytes}, nil
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

	// Update journal.
	journal, err := loadJournalForWrite(opts.FS, opts.Dir)
	if err != nil {
		return nil, err
	}
	journal.Entries = append(journal.Entries, drizzleJournalEntry{
		Idx:         idx,
		Version:     "7",
		When:        opts.Now(),
		Tag:         tag,
		Breakpoints: true,
	})
	journal.Version = "7"
	journal.Dialect = "postgresql"
	journalBytes, err := marshalJournal(journal)
	if err != nil {
		return nil, err
	}

	var downSQL string
	if opts.WithDown {
		downStmts := DiffDown(prev, cur, diffOpts)
		if len(downStmts) > 0 {
			downSQL = strings.Join(downStmts, "\n--> statement-breakpoint\n") + "\n"
		}
	}

	if err := opts.Write(tag+".sql", []byte(sql)); err != nil {
		return nil, fmt.Errorf("drops/pg: write migration SQL: %w", err)
	}
	if downSQL != "" {
		if err := opts.Write(tag+".down.sql", []byte(downSQL)); err != nil {
			return nil, fmt.Errorf("drops/pg: write down SQL: %w", err)
		}
	}
	if err := opts.Write(fmt.Sprintf("meta/%04d_snapshot.json", idx), snapshotBytes); err != nil {
		return nil, fmt.Errorf("drops/pg: write snapshot: %w", err)
	}
	if err := opts.Write("meta/_journal.json", journalBytes); err != nil {
		return nil, fmt.Errorf("drops/pg: write journal: %w", err)
	}
	if renameLogBytes != nil {
		if err := opts.Write(RenameLogFile, renameLogBytes); err != nil {
			return nil, fmt.Errorf("drops/pg: write rename log: %w", err)
		}
	}

	return &GenerateResult{
		Tag:       tag,
		Idx:       idx,
		SQL:       sql,
		DownSQL:   downSQL,
		Snapshot:  snapshotBytes,
		Journal:   journalBytes,
		Renames:   renames,
		RenameLog: renameLogBytes,
	}, nil
}

// loadPrevSnapshot reads the previous snapshot (the one with the highest
// idx in meta/) and returns it together with the next idx to use.
//
// Returns EmptySnapshot and idx 0 if no journal or snapshot exists.
func loadPrevSnapshot(fsys fs.FS, dir string) (*Snapshot, int, error) {
	metaDir := path.Join(dir, "meta")
	entries, err := fs.ReadDir(fsys, metaDir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return EmptySnapshot(), 0, nil
		}
		return nil, 0, fmt.Errorf("drops/pg: read meta dir: %w", err)
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
		prefix := strings.TrimSuffix(n, "_snapshot.json")
		idx, perr := atoiPadded(prefix)
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
		return nil, 0, fmt.Errorf("drops/pg: read snapshot %s: %w", latest.name, err)
	}
	prev, err := UnmarshalSnapshot(body)
	if err != nil {
		return nil, 0, err
	}
	return prev, latest.idx + 1, nil
}

// loadJournalForWrite reads the existing journal or returns a fresh one.
func loadJournalForWrite(fsys fs.FS, dir string) (*drizzleJournal, error) {
	body, err := fs.ReadFile(fsys, path.Join(dir, "meta", "_journal.json"))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return &drizzleJournal{Version: "7", Dialect: "postgresql"}, nil
		}
		return nil, fmt.Errorf("drops/pg: read journal: %w", err)
	}
	var j drizzleJournal
	if err := json.Unmarshal(body, &j); err != nil {
		return nil, fmt.Errorf("drops/pg: parse journal: %w", err)
	}
	return &j, nil
}

func marshalJournal(j *drizzleJournal) ([]byte, error) {
	body, err := json.MarshalIndent(j, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(body, '\n'), nil
}

// atoiPadded converts a zero-padded numeric string to an int. Returns
// an error for non-numeric input.
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

// Random-name generation -----------------------------------------------

// nameAdjectives / nameNouns are short word lists used to produce
// friendly migration tags like "ancient_forest". The full drizzle-kit
// generator uses a larger pool of Marvel-themed words; we keep ours
// small to avoid bloat, and a custom NameFn can replace it entirely.
var (
	nameAdjectives = []string{
		"ancient", "bold", "calm", "deep", "eager", "free", "gentle",
		"happy", "icy", "jolly", "keen", "lucky", "mighty", "noble",
		"olive", "proud", "quiet", "rapid", "silent", "tender", "urban",
		"vivid", "warm", "young", "zealous",
	}
	nameNouns = []string{
		"forest", "mountain", "river", "ocean", "valley", "summit",
		"meadow", "harbor", "lagoon", "canyon", "glacier", "savanna",
		"plateau", "tundra", "delta", "fjord", "isthmus", "atoll",
		"prairie", "estuary", "ridge", "vale", "bay", "cape", "isle",
	}
)

func randomName() string {
	return nameAdjectives[rand.IntN(len(nameAdjectives))] + "_" + nameNouns[rand.IntN(len(nameNouns))]
}
