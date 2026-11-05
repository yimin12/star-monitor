// Command star-monitor collects GitHub and GitLab star counts every few hours
// and serves a ranking of repositories that are gaining attention.
package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/yimin12/star-monitor/internal/collector"
	"github.com/yimin12/star-monitor/internal/score"
	"github.com/yimin12/star-monitor/internal/source"
	"github.com/yimin12/star-monitor/internal/store"
	"github.com/yimin12/star-monitor/internal/web"
)

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	if v, err := strconv.Atoi(os.Getenv(key)); err == nil {
		return v
	}
	return def
}

func main() {
	once := flag.Bool("once", false, "run one collection pass and exit")
	flag.Parse()
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	st, err := store.Open(env("DB_PATH", "star-monitor.db"))
	if err != nil {
		log.Error("open database", "err", err)
		os.Exit(1)
	}
	interval, err := time.ParseDuration(env("INTERVAL", "6h"))
	if err != nil {
		log.Error("INTERVAL", "err", err)
		os.Exit(1)
	}
	window := envInt("WINDOW_DAYS", 15)
	hc := &http.Client{Timeout: 60 * time.Second}
	gitlabParams := score.Default
	gitlabParams.K = float64(envInt("GITLAB_K", 100))
	githubParams := score.Default
	githubParams.K = float64(envInt("GITHUB_K", 1000))

	c := &collector.Collector{
		Store:    st,
		Log:      log,
		Interval: interval,
		Window:   window,
		KeepDays: envInt("KEEP_DAYS", 45),
		GitHub:   &source.GitHub{Token: os.Getenv("GITHUB_TOKEN"), MinStars: envInt("GITHUB_MIN_STARS", 300), HTTP: hc, Log: log},
		Params:   map[string]score.Params{"github": githubParams, "gitlab": gitlabParams},
		// Off by default: since July 2026 GitHub lists stargazers only to repo
		// admins and collaborators, so other repositories cannot be backfilled.
		BackfillRepos:    envInt("BACKFILL_REPOS", 0),
		BackfillRequests: envInt("BACKFILL_REQUESTS", 2000),
		BackfillMaxStars: envInt("BACKFILL_MAX_STARS", 40000),
	}
	if envInt("GITLAB_ENABLED", 1) == 1 {
		c.GitLab = &source.GitLab{MinStars: envInt("GITLAB_MIN_STARS", 50), HTTP: hc, Log: log}
	}
	if c.GitHub.Token == "" {
		log.Warn("GITHUB_TOKEN is empty: search is limited to 10 requests a minute")
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if *once {
		c.RunOnce(ctx)
		return
	}

	srv := &http.Server{
		Addr:              env("LISTEN", ":8080"),
		Handler:           (&web.Server{Store: st, Log: log, Window: window, Interval: interval, Running: c.Running}).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	loopDone := make(chan struct{})
	go func() {
		defer close(loopDone)
		c.Loop(ctx)
	}()
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()
	log.Info("listening", "addr", srv.Addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Error("serve", "err", err)
		os.Exit(1)
	}
	// Let the collector record where it stopped, within the pod's 30-second grace period.
	select {
	case <-loopDone:
	case <-time.After(20 * time.Second):
		log.Warn("collector did not stop in time")
	}
	log.Info("stopped")
}
