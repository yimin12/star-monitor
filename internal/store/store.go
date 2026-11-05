// Package store keeps repositories, one star count per repository per UTC day,
// collection runs and computed scores in SQLite.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

const schema = `
CREATE TABLE IF NOT EXISTS repos (
  id INTEGER PRIMARY KEY,
  source TEXT NOT NULL,
  ext_id INTEGER NOT NULL,
  full_name TEXT NOT NULL,
  url TEXT NOT NULL,
  description TEXT NOT NULL DEFAULT '',
  language TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL DEFAULT '',
  stars INTEGER NOT NULL,
  first_seen INTEGER NOT NULL,
  last_seen INTEGER NOT NULL,
  UNIQUE (source, ext_id)
);
CREATE INDEX IF NOT EXISTS repos_seen ON repos (source, last_seen);
-- The last observed count of each UTC day. Backfilled days never overwrite observed ones.
CREATE TABLE IF NOT EXISTS daily (
  repo_id INTEGER NOT NULL,
  day INTEGER NOT NULL,
  stars INTEGER NOT NULL,
  observed INTEGER NOT NULL DEFAULT 1,
  PRIMARY KEY (repo_id, day)
) WITHOUT ROWID;
CREATE TABLE IF NOT EXISTS runs (
  id INTEGER PRIMARY KEY,
  source TEXT NOT NULL,
  started INTEGER NOT NULL,
  finished INTEGER,
  ok INTEGER NOT NULL DEFAULT 0,
  repos INTEGER NOT NULL DEFAULT 0,
  requests INTEGER NOT NULL DEFAULT 0,
  note TEXT NOT NULL DEFAULT ''
);
-- Query ranges finished within a run, so an interrupted run can resume.
CREATE TABLE IF NOT EXISTS run_ranges (
  run_id INTEGER NOT NULL,
  key TEXT NOT NULL,
  PRIMARY KEY (run_id, key)
) WITHOUT ROWID;
CREATE TABLE IF NOT EXISTS scores (
  repo_id INTEGER PRIMARY KEY,
  s0 INTEGER, now INTEGER, delta INTEGER, recent INTEGER,
  a REAL, b REAL, m REAL, p REAL, score REAL,
  days INTEGER, spike INTEGER, series TEXT
);
CREATE INDEX IF NOT EXISTS scores_score ON scores (score DESC);
CREATE TABLE IF NOT EXISTS backfill (
  repo_id INTEGER PRIMARY KEY,
  at INTEGER NOT NULL,
  ok INTEGER NOT NULL,
  note TEXT NOT NULL DEFAULT ''
);
`

// Store wraps the database. SQLite allows one writer, so writes are serialized
// by keeping a single connection.
type Store struct{ DB *sql.DB }

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(10000)&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(ON)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		return nil, fmt.Errorf("schema: %w", err)
	}
	return &Store{DB: db}, nil
}

// Day is the UTC day number used as the key of the daily table.
func Day(t time.Time) int64 { return t.UTC().Unix() / 86400 }

// Repo is one observation from a source.
type Repo struct {
	Source, FullName, URL, Description, Language, CreatedAt string
	ExtID                                                   int64
	Stars                                                   int
}

// SaveBatch upserts repositories and records today's count for each.
func (s *Store) SaveBatch(ctx context.Context, repos []Repo, now time.Time) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	up, err := tx.PrepareContext(ctx, `
INSERT INTO repos (source, ext_id, full_name, url, description, language, created_at, stars, first_seen, last_seen)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (source, ext_id) DO UPDATE SET full_name = excluded.full_name, url = excluded.url,
  description = excluded.description, language = excluded.language, stars = excluded.stars,
  last_seen = excluded.last_seen
RETURNING id`)
	if err != nil {
		return err
	}
	day, err := tx.PrepareContext(ctx, `
INSERT INTO daily (repo_id, day, stars, observed) VALUES (?, ?, ?, 1)
ON CONFLICT (repo_id, day) DO UPDATE SET stars = excluded.stars, observed = 1`)
	if err != nil {
		return err
	}
	ts, d := now.Unix(), Day(now)
	for _, r := range repos {
		var id int64
		desc := r.Description
		if len([]rune(desc)) > 300 {
			desc = string([]rune(desc)[:300])
		}
		if err := up.QueryRowContext(ctx, r.Source, r.ExtID, r.FullName, r.URL, desc, r.Language, r.CreatedAt, r.Stars, ts, ts).Scan(&id); err != nil {
			return err
		}
		if _, err := day.ExecContext(ctx, id, d, r.Stars); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// SaveBackfill stores reconstructed counts for days that have no observation.
func (s *Store) SaveBackfill(ctx context.Context, repoID int64, days map[int64]int, ok bool, note string, now time.Time) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for d, v := range days {
		if _, err := tx.ExecContext(ctx, `INSERT INTO daily (repo_id, day, stars, observed) VALUES (?, ?, ?, 0)
ON CONFLICT (repo_id, day) DO NOTHING`, repoID, d, v); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO backfill (repo_id, at, ok, note) VALUES (?, ?, ?, ?)
ON CONFLICT (repo_id) DO UPDATE SET at = excluded.at, ok = excluded.ok, note = excluded.note`,
		repoID, now.Unix(), ok, note); err != nil {
		return err
	}
	return tx.Commit()
}

// StartRun and FinishRun record one collection pass for the status bar.
func (s *Store) StartRun(ctx context.Context, source string, now time.Time) (int64, error) {
	var id int64
	err := s.DB.QueryRowContext(ctx, `INSERT INTO runs (source, started) VALUES (?, ?) RETURNING id`, source, now.Unix()).Scan(&id)
	return id, err
}

// FinishRun closes a run. repos counts every repository of the source seen since
// the run started, so a resumed run reports its whole pass.
func (s *Store) FinishRun(ctx context.Context, id int64, ok bool, requests int, note string, now time.Time) error {
	_, err := s.DB.ExecContext(ctx, `UPDATE runs SET finished = ?, ok = ?, requests = requests + ?, note = ?,
  repos = (SELECT COUNT(*) FROM repos r WHERE r.source = runs.source AND r.last_seen >= runs.started)
WHERE id = ?`, now.Unix(), ok, requests, note, id)
	return err
}

// AddRequests records requests made by a run segment that was interrupted.
func (s *Store) AddRequests(ctx context.Context, id int64, requests int) error {
	_, err := s.DB.ExecContext(ctx, `UPDATE runs SET requests = requests + ? WHERE id = ?`, requests, id)
	return err
}

// ResumableRun returns the latest unfinished run of a source started after
// since, or 0. A run is left unfinished when the process stops mid-pass.
func (s *Store) ResumableRun(ctx context.Context, source string, since time.Time) (int64, error) {
	var id int64
	err := s.DB.QueryRowContext(ctx, `SELECT id FROM runs WHERE source = ? AND finished IS NULL AND started >= ?
  AND id = (SELECT MAX(id) FROM runs WHERE source = ?)`, source, since.Unix(), source).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return id, err
}

// Progress tracks finished query ranges of one run.
type Progress struct {
	store *Store
	run   int64
	done  map[string]bool
}

func (s *Store) Progress(ctx context.Context, run int64) (*Progress, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT key FROM run_ranges WHERE run_id = ?`, run)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	p := &Progress{store: s, run: run, done: map[string]bool{}}
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			return nil, err
		}
		p.done[k] = true
	}
	return p, rows.Err()
}

// Done reports whether a range finished earlier in this run.
func (p *Progress) Done(key string) bool { return p != nil && p.done[key] }

// Mark records a finished range.
func (p *Progress) Mark(ctx context.Context, key string) error {
	if p == nil {
		return nil
	}
	p.done[key] = true
	_, err := p.store.DB.ExecContext(ctx, `INSERT OR IGNORE INTO run_ranges (run_id, key) VALUES (?, ?)`, p.run, key)
	return err
}

// Len is the number of finished ranges.
func (p *Progress) Len() int {
	if p == nil {
		return 0
	}
	return len(p.done)
}

// LastRun is the latest run of a source, finished or not.
type LastRun struct {
	Source            string
	Started, Finished time.Time
	Running, OK       bool
	Repos, Requests   int
	Note              string
}

func (s *Store) LastRuns(ctx context.Context) ([]LastRun, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT source, started, COALESCE(finished, 0), ok, repos, requests, note
FROM runs WHERE id IN (SELECT MAX(id) FROM runs GROUP BY source) ORDER BY source`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []LastRun
	for rows.Next() {
		var r LastRun
		var started, finished int64
		if err := rows.Scan(&r.Source, &started, &finished, &r.OK, &r.Repos, &r.Requests, &r.Note); err != nil {
			return nil, err
		}
		r.Started = time.Unix(started, 0)
		r.Running = finished == 0
		if !r.Running {
			r.Finished = time.Unix(finished, 0)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// LastSuccess is when a source last finished a run successfully, or zero.
func (s *Store) LastSuccess(ctx context.Context, source string) (time.Time, error) {
	var ts sql.NullInt64
	err := s.DB.QueryRowContext(ctx, `SELECT MAX(finished) FROM runs WHERE source = ? AND ok = 1`, source).Scan(&ts)
	if err != nil || !ts.Valid {
		return time.Time{}, err
	}
	return time.Unix(ts.Int64, 0), nil
}

// Prune drops daily rows older than keepDays and repositories not seen for that long.
func (s *Store) Prune(ctx context.Context, keepDays int, now time.Time) error {
	cut := Day(now) - int64(keepDays)
	if _, err := s.DB.ExecContext(ctx, `DELETE FROM daily WHERE day < ?`, cut); err != nil {
		return err
	}
	if _, err := s.DB.ExecContext(ctx, `DELETE FROM run_ranges WHERE run_id IN (SELECT id FROM runs WHERE finished IS NOT NULL)`); err != nil {
		return err
	}
	old := `SELECT id FROM repos WHERE last_seen < ?`
	ts := now.Add(-time.Duration(keepDays) * 24 * time.Hour).Unix()
	for _, q := range []string{
		`DELETE FROM daily WHERE repo_id IN (` + old + `)`,
		`DELETE FROM scores WHERE repo_id IN (` + old + `)`,
		`DELETE FROM backfill WHERE repo_id IN (` + old + `)`,
		`DELETE FROM repos WHERE last_seen < ?`,
	} {
		if _, err := s.DB.ExecContext(ctx, q, ts); err != nil {
			return err
		}
	}
	return nil
}
