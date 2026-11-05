package store

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/yimin12/star-monitor/internal/score"
)

// Recompute scores every repository seen in the last two days over the window,
// with the parameters of its source.
func (s *Store) Recompute(ctx context.Context, window int, params map[string]score.Params, now time.Time) (int, error) {
	end := Day(now)
	start := end - int64(window)
	rows, err := s.DB.QueryContext(ctx, `SELECT d.repo_id, r.source, d.day, d.stars FROM daily d
JOIN repos r ON r.id = d.repo_id
WHERE d.day >= ? AND d.day <= ? AND r.last_seen >= ?
ORDER BY d.repo_id, d.day`, start, end, now.Add(-48*time.Hour).Unix())
	if err != nil {
		return 0, err
	}
	series := map[int64][]int{}
	sources := map[int64]string{}
	for rows.Next() {
		var id, day int64
		var stars int
		var src string
		if err := rows.Scan(&id, &src, &day, &stars); err != nil {
			rows.Close()
			return 0, err
		}
		v, ok := series[id]
		if !ok {
			v = make([]int, window+1)
			for i := range v {
				v[i] = -1
			}
			series[id] = v
			sources[id] = src
		}
		v[day-start] = stars
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM scores`); err != nil {
		return 0, err
	}
	ins, err := tx.PrepareContext(ctx, `INSERT INTO scores (repo_id, s0, now, delta, recent, a, b, m, p, score, days, spike, series)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return 0, err
	}
	for id, v := range series {
		p, ok := params[sources[id]]
		if !ok {
			p = score.Default
		}
		r := score.Compute(v, p)
		js, _ := json.Marshal(v)
		if _, err := ins.ExecContext(ctx, id, r.S0, r.Now, r.Delta, r.Recent, r.A, r.B, r.M, r.P, r.Score, r.Days, r.Spike, string(js)); err != nil {
			return 0, err
		}
	}
	return len(series), tx.Commit()
}

// Row is one line of the ranking.
type Row struct {
	ID                                 int64
	Source, FullName, URL, Description string
	Language, CreatedAt                string
	S0, Now, Delta, Recent, Days       int
	A, B, M, P, Score                  float64
	Spike, Backfilled                  bool
	Series                             []int
}

// Growth is the relative gain in percent, or -1 when the start was zero.
func (r Row) Growth() float64 {
	if r.S0 == 0 {
		return -1
	}
	return float64(r.Delta) * 100 / float64(r.S0)
}

// Query filters and sorts the ranking.
type Query struct {
	Source, Language, Sort string
	MinStars, Limit        int
	HideSpikes             bool
}

func (s *Store) Top(ctx context.Context, q Query) ([]Row, error) {
	order := map[string]string{
		"score":  "s.score DESC",
		"delta":  "s.delta DESC",
		"growth": "CAST(s.delta AS REAL) / MAX(s.s0, 1) DESC",
	}[q.Sort]
	if order == "" {
		order = "s.score DESC"
	}
	where := []string{"r.source = ?", "s.delta > 0", "s.now >= ?"}
	args := []any{q.Source, q.MinStars}
	if q.Language != "" {
		where = append(where, "r.language = ?")
		args = append(args, q.Language)
	}
	if q.HideSpikes {
		where = append(where, "s.spike = 0")
	}
	if q.Sort == "growth" {
		// Growth from a tiny base is what the score avoids; keep that true here.
		where = append(where, "s.s0 >= 100")
	}
	limit := q.Limit
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	args = append(args, limit)
	rows, err := s.DB.QueryContext(ctx, fmt.Sprintf(`SELECT r.id, r.source, r.full_name, r.url, r.description, r.language, r.created_at,
  s.s0, s.now, s.delta, s.recent, s.days, s.a, s.b, s.m, s.p, s.score, s.spike, s.series,
  EXISTS (SELECT 1 FROM backfill b WHERE b.repo_id = r.id AND b.ok = 1)
FROM scores s JOIN repos r ON r.id = s.repo_id
WHERE %s ORDER BY %s LIMIT ?`, strings.Join(where, " AND "), order), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Row
	for rows.Next() {
		var r Row
		var series string
		if err := rows.Scan(&r.ID, &r.Source, &r.FullName, &r.URL, &r.Description, &r.Language, &r.CreatedAt,
			&r.S0, &r.Now, &r.Delta, &r.Recent, &r.Days, &r.A, &r.B, &r.M, &r.P, &r.Score, &r.Spike, &series, &r.Backfilled); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(series), &r.Series)
		out = append(out, r)
	}
	return out, rows.Err()
}

// Languages lists the most common languages among scored repositories of a source.
func (s *Store) Languages(ctx context.Context, source string, limit int) ([]string, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT r.language FROM scores s JOIN repos r ON r.id = s.repo_id
WHERE r.source = ? AND r.language != '' AND s.delta > 0 GROUP BY r.language ORDER BY COUNT(*) DESC LIMIT ?`, source, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var l string
		if err := rows.Scan(&l); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// Coverage is how many days of observed history the database holds.
func (s *Store) Coverage(ctx context.Context, source string) (int, error) {
	var n int
	err := s.DB.QueryRowContext(ctx, `SELECT COUNT(DISTINCT d.day) FROM daily d JOIN repos r ON r.id = d.repo_id
WHERE r.source = ? AND d.observed = 1`, source).Scan(&n)
	return n, err
}

// BackfillCandidates are repositories whose window start is unknown or recent,
// ranked by partial gain, that have not been backfilled in the last week.
func (s *Store) BackfillCandidates(ctx context.Context, source string, window, maxStars, limit int, now time.Time) ([]BackfillCandidate, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT r.id, r.full_name, r.stars FROM scores s JOIN repos r ON r.id = s.repo_id
LEFT JOIN backfill b ON b.repo_id = r.id
WHERE r.source = ? AND s.days < ? AND r.stars <= ? AND s.delta > 0 AND (b.repo_id IS NULL OR b.at < ?)
ORDER BY s.delta DESC LIMIT ?`, source, window, maxStars, now.Add(-7*24*time.Hour).Unix(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []BackfillCandidate
	for rows.Next() {
		var c BackfillCandidate
		if err := rows.Scan(&c.ID, &c.FullName, &c.Stars); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

type BackfillCandidate struct {
	ID       int64
	FullName string
	Stars    int
}
