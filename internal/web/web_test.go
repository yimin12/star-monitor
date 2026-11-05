package web

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yimin12/star-monitor/internal/score"
	"github.com/yimin12/star-monitor/internal/store"
)

func TestPageRanksAndEscapes(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	now := time.Now()
	// Two repositories over 16 days: one gains steadily from 5000, one from 0.
	for d := 15; d >= 0; d-- {
		at := now.AddDate(0, 0, -d)
		g := 15 - d
		if err := st.SaveBatch(ctx, []store.Repo{
			{Source: "github", ExtID: 1, FullName: "a/steady", URL: "https://github.com/a/steady", Language: "Go",
				Description: "<script>alert(1)</script>", Stars: 5000 + 200*g},
			{Source: "github", ExtID: 2, FullName: "b/newborn", URL: "https://github.com/b/newborn", Stars: 400 * g},
		}, at); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.Recompute(ctx, 15, map[string]score.Params{"github": score.Default}, now); err != nil {
		t.Fatal(err)
	}
	s := &Server{Store: st, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Window: 15, Interval: 6 * time.Hour, Running: func() bool { return false }}
	h := s.Handler()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	body := rec.Body.String()
	if rec.Code != 200 {
		t.Fatalf("status %d", rec.Code)
	}
	if strings.Contains(body, "<script>alert") {
		t.Fatal("description not escaped")
	}
	a, b := strings.Index(body, "a/steady"), strings.Index(body, "b/newborn")
	if a < 0 || b < 0 || a > b {
		t.Fatalf("steady repo should rank above the newborn one (a=%d b=%d)", a, b)
	}
	if rec.Header().Get("Content-Security-Policy") == "" {
		t.Fatal("missing CSP")
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/rows?sort=delta&lang=Go", nil))
	if !strings.Contains(rec.Body.String(), "a/steady") || strings.Contains(rec.Body.String(), "b/newborn") {
		t.Fatal("language filter not applied")
	}
}
