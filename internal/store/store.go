// Package store persists corpus identity, per-cycle deltas and their rollups.
//
// The division of labour with internal/model is deliberate: the model owns identity
// and id assignment, this package only records what the model decided. That keeps
// ids dense and stable across restarts, which in turn lets stored deltas reference
// entities by array index rather than by a translated key.
package store

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
	"time"

	"folding/internal/model"

	_ "modernc.org/sqlite"
)

// Store is a handle on the history database.
//
// Reads and writes use separate connection pools. WAL mode allows any number of
// concurrent readers alongside a single writer, but database/sql cannot express that
// through one handle: capping a shared pool at one connection to serialise writes
// also serialises every read behind them. With history queries arriving at thousands
// per second that queueing dominates — measured at p99 82 ms against sub-millisecond
// for every in-memory endpoint.
type Store struct {
	w *sql.DB // single connection; all mutations
	r *sql.DB // pooled; read-only queries

	// Prepared read statements, cached by SQL text.
	//
	// database/sql re-prepares on every Query call, and this driver is pure Go, so
	// parsing the statement costs more than executing it against these small
	// tables. Caching turned the history endpoints from the slowest thing the API
	// does into the ordinary case.
	stmtMu sync.RWMutex
	stmts  map[string]*sql.Stmt
}

// readPoolSize bounds concurrent readers. Beyond a handful, SQLite page-cache
// contention costs more than the added parallelism returns.
const readPoolSize = 8

// Open opens or creates the database at path and applies the schema.
func Open(path string) (*Store, error) {
	w, err := openPool(path, 1)
	if err != nil {
		return nil, err
	}
	if _, err := w.Exec(schema); err != nil {
		w.Close()
		return nil, fmt.Errorf("store: applying schema: %w", err)
	}
	if err := backfillProjectRollups(w); err != nil {
		w.Close()
		return nil, err
	}
	r, err := openPool(path, readPoolSize)
	if err != nil {
		w.Close()
		return nil, err
	}
	return &Store{w: w, r: r, stmts: map[string]*sql.Stmt{}}, nil
}

func openPool(path string, conns int) (*sql.DB, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(conns)
	db.SetMaxIdleConns(conns)
	// Connections are cheap to keep and expensive to re-establish, since every new
	// one must re-apply the pragmas below.
	db.SetConnMaxLifetime(0)
	for _, p := range pragmas {
		if _, err := db.Exec(p); err != nil {
			db.Close()
			return nil, fmt.Errorf("store: %s: %w", p, err)
		}
	}
	return db, nil
}

// Close releases both pools.
func (s *Store) Close() error {
	s.stmtMu.Lock()
	for _, st := range s.stmts {
		st.Close()
	}
	s.stmts = nil
	s.stmtMu.Unlock()

	err := s.r.Close()
	if werr := s.w.Close(); err == nil {
		err = werr
	}
	return err
}

// DB exposes the write handle for maintenance tasks. Prefer the typed methods.
func (s *Store) DB() *sql.DB { return s.w }

// query runs a cached prepared statement against the read pool.
func (s *Store) query(ctx context.Context, sqlText string, args ...any) (*sql.Rows, error) {
	s.stmtMu.RLock()
	st, ok := s.stmts[sqlText]
	s.stmtMu.RUnlock()
	if ok {
		return st.QueryContext(ctx, args...)
	}

	prepared, err := s.r.PrepareContext(ctx, sqlText)
	if err != nil {
		return nil, err
	}
	s.stmtMu.Lock()
	// Another goroutine may have prepared the same text meanwhile; keep one.
	if existing, ok := s.stmts[sqlText]; ok {
		s.stmtMu.Unlock()
		prepared.Close()
		return existing.QueryContext(ctx, args...)
	}
	s.stmts[sqlText] = prepared
	s.stmtMu.Unlock()
	return prepared.QueryContext(ctx, args...)
}

// CycleMeta is the audit record for one ingested snapshot pair.
type CycleMeta struct {
	TeamSnapshotAt time.Time
	UserSnapshotAt time.Time
	TeamRows       int
	UserRows       int
	Malformed      int
	Duration       time.Duration
}

// LoadIdentity rebuilds the model's identity tables from disk.
//
// Rows are read in id order and appended, so each entity lands back on the slot it
// was originally assigned. Stored deltas reference those slots by number, so a
// mismatch would silently reattribute history to the wrong donor — hence the
// explicit assertions rather than trusting the ordering.
func (s *Store) LoadIdentity(ctx context.Context, st *model.State) error {
	rows, err := s.r.QueryContext(ctx, `SELECT name_id, name FROM names ORDER BY name_id`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id int32
		var name string
		if err := rows.Scan(&id, &name); err != nil {
			return err
		}
		if got := st.Names.Intern(name); got != id {
			return fmt.Errorf("store: name %q loaded as id %d, expected %d", name, got, id)
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}

	trows, err := s.r.QueryContext(ctx,
		`SELECT slot, team_id, name_id, score, wu FROM teams ORDER BY slot`)
	if err != nil {
		return err
	}
	defer trows.Close()
	for trows.Next() {
		var slot, teamID, nameID int32
		var score, wu int64
		if err := trows.Scan(&slot, &teamID, &nameID, &score, &wu); err != nil {
			return err
		}
		if got := st.AppendTeam(teamID, nameID); got != slot {
			return fmt.Errorf("store: team %d loaded at slot %d, expected %d", teamID, got, slot)
		}
		st.Teams[slot].Score, st.Teams[slot].WUs = score, wu
	}
	if err := trows.Err(); err != nil {
		return err
	}

	mrows, err := s.r.QueryContext(ctx,
		`SELECT member_id, name_id, team_id, score, wu FROM members ORDER BY member_id`)
	if err != nil {
		return err
	}
	defer mrows.Close()
	for mrows.Next() {
		var id, nameID, teamID int32
		var score, wu int64
		if err := mrows.Scan(&id, &nameID, &teamID, &score, &wu); err != nil {
			return err
		}
		if got := st.AppendMember(nameID, teamID); got != id {
			return fmt.Errorf("store: member (%d,%d) loaded at slot %d, expected %d",
				nameID, teamID, got, id)
		}
		st.Members[id].Score, st.Members[id].WUs = score, wu
	}
	return mrows.Err()
}

// WriteCycle persists one cycle: newly interned identity, the deltas, and the audit
// row — all in a single transaction.
//
// Atomicity matters more than it might appear. Identity written without its deltas
// would leave a donor whose history silently begins late; deltas written without
// their identity would reference a slot that does not exist on the next restart.
func (s *Store) WriteCycle(ctx context.Context, st *model.State, c *model.Cycle, meta CycleMeta) error {
	tx, err := s.w.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	ts := c.At.UTC().Unix()

	// Names are interned before slots are assigned, so persist any that are new.
	// The arena only grows, so "everything past the stored high-water mark" is
	// exactly the set of new names.
	var storedNames int64
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM names`).Scan(&storedNames); err != nil {
		return err
	}
	if int(storedNames) < st.Names.Len() {
		stmt, err := tx.PrepareContext(ctx, `INSERT INTO names(name_id, name) VALUES(?, ?)`)
		if err != nil {
			return err
		}
		for id := int(storedNames); id < st.Names.Len(); id++ {
			if _, err := stmt.ExecContext(ctx, id, st.Names.Name(int32(id))); err != nil {
				stmt.Close()
				return fmt.Errorf("store: inserting name %d: %w", id, err)
			}
		}
		stmt.Close()
	}

	if len(c.NewTeams) > 0 {
		stmt, err := tx.PrepareContext(ctx,
			`INSERT OR IGNORE INTO teams(slot, team_id, name_id, first_seen, score, wu) VALUES(?,?,?,?,?,?)`)
		if err != nil {
			return err
		}
		for _, slot := range c.NewTeams {
			t := st.Teams[slot]
			if _, err := stmt.ExecContext(ctx, slot, t.ID, t.NameID, ts, t.Score, t.WUs); err != nil {
				stmt.Close()
				return fmt.Errorf("store: inserting team slot %d: %w", slot, err)
			}
		}
		stmt.Close()
	}

	if len(c.NewMembers) > 0 {
		stmt, err := tx.PrepareContext(ctx,
			`INSERT OR IGNORE INTO members(member_id, name_id, team_id, first_seen, score, wu) VALUES(?,?,?,?,?,?)`)
		if err != nil {
			return err
		}
		for _, slot := range c.NewMembers {
			m := st.Members[slot]
			if _, err := stmt.ExecContext(ctx, slot, m.NameID, m.TeamID, ts, m.Score, m.WUs); err != nil {
				stmt.Close()
				return fmt.Errorf("store: inserting member slot %d: %w", slot, err)
			}
		}
		stmt.Close()
	}

	if err := insertDeltas(ctx, tx,
		`INSERT OR REPLACE INTO member_deltas(member_id, ts, d_score, d_wu) VALUES(?, ?, ?, ?)`,
		ts, c.MemberDeltas); err != nil {
		return err
	}
	if err := insertDeltas(ctx, tx,
		`INSERT OR REPLACE INTO team_deltas(slot, ts, d_score, d_wu) VALUES(?, ?, ?, ?)`,
		ts, c.TeamDeltas); err != nil {
		return err
	}

	// Refresh cumulative totals only for entities that moved. At ~1k changed of
	// 2.7M this is trivial, and it is what lets a restart restore state directly
	// instead of replaying a snapshot.
	if err := updateTotals(ctx, tx,
		`UPDATE members SET score = ?, wu = ? WHERE member_id = ?`,
		c.MemberDeltas, func(id int32) (int64, int64) {
			return st.Members[id].Score, st.Members[id].WUs
		}); err != nil {
		return err
	}
	if err := updateTotals(ctx, tx,
		`UPDATE teams SET score = ?, wu = ? WHERE slot = ?`,
		c.TeamDeltas, func(id int32) (int64, int64) {
			return st.Teams[id].Score, st.Teams[id].WUs
		}); err != nil {
		return err
	}

	// Rollups are refreshed inside the same transaction as the deltas they
	// summarise, so the two can never disagree.
	if err := rollupCycle(ctx, tx, c.At); err != nil {
		return err
	}

	_, err = tx.ExecContext(ctx, `
        INSERT OR REPLACE INTO cycles(ts, team_snapshot_at, user_snapshot_at,
            team_rows, user_rows, malformed, new_members, new_teams,
            member_deltas, team_deltas, regressions, duration_ms)
        VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`,
		ts, meta.TeamSnapshotAt.UTC().Unix(), meta.UserSnapshotAt.UTC().Unix(),
		meta.TeamRows, meta.UserRows, meta.Malformed,
		len(c.NewMembers), len(c.NewTeams),
		len(c.MemberDeltas), len(c.TeamDeltas), c.Regressions,
		meta.Duration.Milliseconds())
	if err != nil {
		return err
	}
	return tx.Commit()
}

func updateTotals(ctx context.Context, tx *sql.Tx, query string,
	ds []model.Delta, cur func(int32) (int64, int64)) error {
	if len(ds) == 0 {
		return nil
	}
	stmt, err := tx.PrepareContext(ctx, query)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, d := range ds {
		score, wu := cur(d.ID)
		if _, err := stmt.ExecContext(ctx, score, wu, d.ID); err != nil {
			return fmt.Errorf("store: updating totals for %d: %w", d.ID, err)
		}
	}
	return nil
}

func insertDeltas(ctx context.Context, tx *sql.Tx, query string, ts int64, ds []model.Delta) error {
	if len(ds) == 0 {
		return nil
	}
	stmt, err := tx.PrepareContext(ctx, query)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, d := range ds {
		if _, err := stmt.ExecContext(ctx, d.ID, ts, d.DScore, d.DWUs); err != nil {
			return fmt.Errorf("store: inserting delta for %d: %w", d.ID, err)
		}
	}
	return nil
}

// AppliedCycles returns the timestamps of every cycle already ingested, so replay
// can skip work it has already done.
func (s *Store) AppliedCycles(ctx context.Context) (map[int64]bool, error) {
	rows, err := s.r.QueryContext(ctx, `SELECT ts FROM cycles`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]bool{}
	for rows.Next() {
		var ts int64
		if err := rows.Scan(&ts); err != nil {
			return nil, err
		}
		out[ts] = true
	}
	return out, rows.Err()
}

// LatestCycle returns the most recent ingested cycle time, or zero if none.
func (s *Store) LatestCycle(ctx context.Context) (time.Time, error) {
	var ts sql.NullInt64
	err := s.r.QueryRowContext(ctx, `SELECT MAX(ts) FROM cycles`).Scan(&ts)
	if err != nil || !ts.Valid {
		return time.Time{}, err
	}
	return time.Unix(ts.Int64, 0).UTC(), nil
}

// RecentCycles returns the newest n snapshot instants, oldest first.
//
// Upstream's cadence is a measurement, not a constant: intervals run 3606–3613s and
// creep later every cycle, so predicting the next publish from a hardcoded hour is
// wrong by a growing margin. These are the observations that prediction is built on.
func (s *Store) RecentCycles(ctx context.Context, n int) ([]time.Time, error) {
	rows, err := s.query(ctx, `SELECT ts FROM (SELECT ts FROM cycles ORDER BY ts DESC LIMIT ?) ORDER BY ts`, n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []time.Time
	for rows.Next() {
		var ts int64
		if err := rows.Scan(&ts); err != nil {
			return nil, err
		}
		out = append(out, time.Unix(ts, 0).UTC())
	}
	return out, rows.Err()
}
