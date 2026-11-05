// Package collector runs the sources on a schedule and refreshes scores.
package collector

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/yimin12/star-monitor/internal/score"
	"github.com/yimin12/star-monitor/internal/source"
	"github.com/yimin12/star-monitor/internal/store"
)

type Collector struct {
	Store    *store.Store
	GitHub   *source.GitHub
	GitLab   *source.GitLab
	Log      *slog.Logger
	Interval time.Duration
	Window   int
	KeepDays int
	Params   map[string]score.Params // per source
	// Backfill limits per run: repositories and GitHub requests.
	BackfillRepos, BackfillRequests, BackfillMaxStars int

	mu      sync.Mutex
	running bool
}

// Loop runs a pass whenever the last successful GitHub pass is older than
// Interval, so a restart resumes the schedule instead of resetting it. A failed
// pass is retried after 30 minutes.
func (c *Collector) Loop(ctx context.Context) {
	for {
		last, err := c.Store.LastSuccess(ctx, "github")
		if err != nil {
			c.Log.Error("read last run", "err", err)
		}
		wait := time.Until(last.Add(c.Interval))
		if wait <= 0 {
			c.RunOnce(ctx)
			if now, _ := c.Store.LastSuccess(ctx, "github"); now.After(last) {
				continue
			}
			wait = 30 * time.Minute
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

// Running reports whether a pass is in progress.
func (c *Collector) Running() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.running
}

// RunOnce collects every source, backfills candidates and recomputes scores.
func (c *Collector) RunOnce(ctx context.Context) {
	c.mu.Lock()
	if c.running {
		c.mu.Unlock()
		return
	}
	c.running = true
	c.mu.Unlock()
	defer func() { c.mu.Lock(); c.running = false; c.mu.Unlock() }()

	if c.GitLab != nil {
		c.collect(ctx, "gitlab", c.GitLab.Collect, func() int { return c.GitLab.Requests })
	}
	c.collect(ctx, "github", c.GitHub.Collect, func() int { return c.GitHub.Requests })
	c.recompute(ctx)
	if c.GitHub.Token != "" && c.BackfillRepos > 0 {
		c.backfill(ctx)
		c.recompute(ctx)
	}
	if err := c.Store.Prune(ctx, c.KeepDays, time.Now()); err != nil {
		c.Log.Error("prune", "err", err)
	}
}

func (c *Collector) collect(ctx context.Context, name string, fn func(context.Context, func([]store.Repo) error, *store.Progress) (int, error), requests func() int) {
	start := time.Now()
	before := requests()
	id, err := c.Store.ResumableRun(ctx, name, start.Add(-c.Interval))
	if err != nil {
		c.Log.Error("find resumable run", "source", name, "err", err)
		return
	}
	resumed := id != 0
	if !resumed {
		if id, err = c.Store.StartRun(ctx, name, start); err != nil {
			c.Log.Error("start run", "source", name, "err", err)
			return
		}
	}
	prog, err := c.Store.Progress(ctx, id)
	if err != nil {
		c.Log.Error("load progress", "source", name, "err", err)
		return
	}
	c.Log.Info("collect start", "source", name, "run", id, "resumed", resumed, "cursor", prog.Cursor())
	n, err := fn(ctx, func(batch []store.Repo) error { return c.Store.SaveBatch(ctx, batch, time.Now()) }, prog)
	bg := context.WithoutCancel(ctx)
	if errors.Is(err, context.Canceled) {
		// Shutting down: leave the run open so the next start resumes it.
		if aerr := c.Store.AddRequests(bg, id, requests()-before); aerr != nil {
			c.Log.Error("record requests", "err", aerr)
		}
		c.Log.Info("collect interrupted", "source", name, "run", id, "cursor", prog.Cursor())
		return
	}
	note := ""
	if err != nil {
		note = err.Error()
		c.Log.Error("collect failed", "source", name, "repos", n, "err", err)
	}
	if ferr := c.Store.FinishRun(bg, id, err == nil, requests()-before, note, time.Now()); ferr != nil {
		c.Log.Error("finish run", "err", ferr)
	}
	c.Log.Info("collect done", "source", name, "run", id, "repos", n, "requests", requests()-before, "took", time.Since(start).Round(time.Second).String())
}

func (c *Collector) recompute(ctx context.Context) {
	n, err := c.Store.Recompute(ctx, c.Window, c.Params, time.Now())
	if err != nil {
		c.Log.Error("recompute", "err", err)
		return
	}
	c.Log.Info("scores recomputed", "repos", n)
}

// backfill reconstructs the window start for repositories that lack it.
func (c *Collector) backfill(ctx context.Context) {
	cands, err := c.Store.BackfillCandidates(ctx, "github", c.Window, c.BackfillMaxStars, c.BackfillRepos, time.Now())
	if err != nil {
		c.Log.Error("backfill candidates", "err", err)
		return
	}
	budget := c.BackfillRequests
	since := time.Unix(store.Day(time.Now())*86400, 0).AddDate(0, 0, -c.Window)
	done := 0
	for _, cand := range cands {
		if budget <= 0 || ctx.Err() != nil {
			break
		}
		before := c.GitHub.Requests
		days, err := c.GitHub.StarsSince(ctx, cand.FullName, cand.Stars, since, budget)
		budget -= c.GitHub.Requests - before
		note := ""
		if err != nil {
			note = err.Error()
			if !errors.Is(err, source.ErrTooManyPages) {
				c.Log.Warn("backfill failed", "repo", cand.FullName, "err", err)
			}
		}
		if serr := c.Store.SaveBackfill(ctx, cand.ID, days, err == nil, note, time.Now()); serr != nil {
			c.Log.Error("save backfill", "err", serr)
			return
		}
		if err == nil {
			done++
		}
	}
	c.Log.Info("backfill done", "repos", done, "candidates", len(cands), "requests", c.BackfillRequests-budget)
}
