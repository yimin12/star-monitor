package source

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"testing"
	"time"

	"github.com/yimin12/star-monitor/internal/store"
)

// fakeSearch serves GitHub search over n repositories with stars 300..300+n-1.
func fakeSearch(t *testing.T, n int) *httptest.Server {
	re := regexp.MustCompile(`stars:(?:>=(\d+)|(\d+)\.\.(\d+))`)
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m := re.FindStringSubmatch(r.URL.Query().Get("q"))
		lo, hi := 0, 1<<30
		if m[1] != "" {
			lo, _ = strconv.Atoi(m[1])
		} else {
			lo, _ = strconv.Atoi(m[2])
			hi, _ = strconv.Atoi(m[3])
		}
		var all []ghRepo
		for i := n - 1; i >= 0; i-- { // sorted by stars, descending
			s := 300 + i
			if s >= lo && s <= hi {
				all = append(all, ghRepo{ID: int64(i), FullName: fmt.Sprintf("o/r%d", i), Stars: s})
			}
		}
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		start, end := (page-1)*100, page*100
		if start > len(all) {
			start = len(all)
		}
		if end > len(all) {
			end = len(all)
		}
		_ = json.NewEncoder(w).Encode(ghSearch{Total: len(all), Items: all[start:end]})
	}))
}

func TestCollectSplitsRangesAndSeesEveryRepo(t *testing.T) {
	srv := fakeSearch(t, 2500)
	defer srv.Close()
	g := &GitHub{MinStars: 300, HTTP: srv.Client(), API: srv.URL, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	g.lastQuery = time.Now().Add(-time.Hour)
	seen := map[int64]bool{}
	// Skip the real pacing in tests.
	orig := searchPace
	searchPace = 0
	defer func() { searchPace = orig }()
	n, err := g.Collect(context.Background(), func(b []store.Repo) error {
		for _, r := range b {
			if seen[r.ExtID] {
				t.Fatalf("repo %d saved twice", r.ExtID)
			}
			seen[r.ExtID] = true
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if n != 2500 || len(seen) != 2500 {
		t.Fatalf("saw %d repos (%d unique), want 2500", n, len(seen))
	}
}

func TestStarsSinceRebuildsDailyTotals(t *testing.T) {
	now := time.Now().UTC()
	today := store.Day(now)
	// 250 stars: 50 three days ago, 100 yesterday, 100 today; oldest first.
	var stars []time.Time
	for i := 0; i < 50; i++ {
		stars = append(stars, now.AddDate(0, 0, -3))
	}
	for i := 0; i < 100; i++ {
		stars = append(stars, now.AddDate(0, 0, -1))
	}
	for i := 0; i < 100; i++ {
		stars = append(stars, now)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		w.Header().Set("Link", fmt.Sprintf(`<%s/x?per_page=100&page=3>; rel="last"`, "http://h"))
		var out []map[string]time.Time
		for i := (page - 1) * 100; i < page*100 && i < len(stars); i++ {
			out = append(out, map[string]time.Time{"starred_at": stars[i]})
		}
		_ = json.NewEncoder(w).Encode(out)
	}))
	defer srv.Close()
	g := &GitHub{HTTP: srv.Client(), API: srv.URL, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	days, err := g.StarsSince(context.Background(), "o/r", 1250, now.AddDate(0, 0, -5), 10)
	if err != nil {
		t.Fatal(err)
	}
	want := map[int64]int{today: 1250, today - 1: 1150, today - 2: 1050, today - 3: 1050, today - 4: 1000}
	for d, v := range want {
		if days[d] != v {
			t.Fatalf("day %d: got %d want %d (all %v)", today-d, days[d], v, days)
		}
	}
}
