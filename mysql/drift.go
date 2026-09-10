package mysql

// Schema drift detection — what's in production that the repo
// doesn't know about, and what's in the repo that production hasn't
// applied yet. Both questions matter:
//
//	report := mysql.DetectDrift(repoSnap, liveSnap)
//	if !report.InSync {
//	    if len(report.PendingMigrations) > 0 {
//	        log.Printf("prod is behind by %d statements", len(report.PendingMigrations))
//	    }
//	    if len(report.UnauthorizedChanges) > 0 {
//	        log.Printf("prod has %d unrecorded changes — investigate",
//	            len(report.UnauthorizedChanges))
//	    }
//	}
//
// Pair Introspect (live server → Snapshot) with the repo Snapshot
// committed alongside migrations; run DetectDrift in CI to gate
// merges on a clean diff.

// DriftReport summarises the gap between two snapshots — typically
// the repo's canonical schema and a live introspection of
// production.
type DriftReport struct {
	// PendingMigrations is the statement list drops would emit to
	// bring live UP TO repo — empty means production has every
	// schema change in the repo applied.
	PendingMigrations []string

	// UnauthorizedChanges is the statement list drops would emit to
	// bring repo UP TO live — empty means production matches the
	// repo with no unrecorded manual edits.
	UnauthorizedChanges []string

	// InSync is true when both PendingMigrations and
	// UnauthorizedChanges are empty.
	InSync bool
}

// DetectDrift computes the two-way diff between the repo's
// canonical snapshot and a live introspection. Both arguments
// must be non-nil — use EmptySnapshot when one side is genuinely
// empty (e.g. fresh database).
//
// What it can see is exactly what Diff compares, which here includes
// indexes and foreign keys — drops/sqlite's cannot, because its schema
// DSL declares no index, so this reports a hand-added index as drift
// where that one cannot.
//
// What it still cannot see is the list under "What Push cannot see" in
// [Push]'s doc comment, and the reason is worth stating rather than
// leaving to be discovered: a difference Diff does not compare produces
// no statement in EITHER direction, so it reads here as agreement. A
// table whose ENGINE was changed by hand, a functional index, a CHECK
// on 5.7 — none of them makes InSync false. Push reports each as a
// notice, so a CI gate that wants them should read Push's notices on a
// dry run rather than expect drift to carry them.
func DetectDrift(repo, live *Snapshot) DriftReport {
	if repo == nil {
		repo = EmptySnapshot()
	}
	if live == nil {
		live = EmptySnapshot()
	}
	pending := Diff(live, repo)
	unauthorized := Diff(repo, live)
	return DriftReport{
		PendingMigrations:   pending,
		UnauthorizedChanges: unauthorized,
		InSync:              len(pending) == 0 && len(unauthorized) == 0,
	}
}

// HasPendingMigrations reports whether production is behind the
// repo.
func (r DriftReport) HasPendingMigrations() bool {
	return len(r.PendingMigrations) > 0
}

// HasUnauthorizedChanges reports whether production has changes the
// repo doesn't know about — typically the headline CI alert.
func (r DriftReport) HasUnauthorizedChanges() bool {
	return len(r.UnauthorizedChanges) > 0
}
