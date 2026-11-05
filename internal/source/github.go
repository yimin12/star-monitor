// Package source collects star counts from GitHub and GitLab.
package source

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"time"

	"github.com/yimin12/star-monitor/internal/store"
)

// GitHub searches repositories by star range. Search returns at most 1000
// results per query, so ranges are split until each fits.
type GitHub struct {
	Token    string
	MinStars int
	HTTP     *http.Client
	Log      *slog.Logger
	API      string // https://api.github.com, overridable in tests

	Requests  int
	lastQuery time.Time
}

const searchCap = 1000

// searchPace spaces search requests: authenticated search allows 30 a minute.
var searchPace = 2100 * time.Millisecond

type ghRepo struct {
	ID          int64  `json:"id"`
	FullName    string `json:"full_name"`
	HTMLURL     string `json:"html_url"`
	Description string `json:"description"`
	Language    string `json:"language"`
	CreatedAt   string `json:"created_at"`
	Stars       int    `json:"stargazers_count"`
}

type ghSearch struct {
	Total      int      `json:"total_count"`
	Incomplete bool     `json:"incomplete_results"`
	Items      []ghRepo `json:"items"`
}

// Collect walks every star range from MinStars up and passes each page to save.
func (g *GitHub) Collect(ctx context.Context, save func([]store.Repo) error) (int, error) {
	seen := 0
	// Ranges are [lo, hi]; hi < 0 means unbounded.
	queue := [][2]int{{g.MinStars, -1}}
	for len(queue) > 0 {
		lo, hi := queue[0][0], queue[0][1]
		queue = queue[1:]
		n, split, err := g.collectRange(ctx, lo, hi, "", save)
		if err != nil {
			return seen, err
		}
		seen += n
		if !split {
			continue
		}
		if hi >= 0 && lo == hi {
			// One star value with over 1000 repositories: split by creation year.
			for y := 2008; y <= time.Now().Year(); y++ {
				m, _, err := g.collectRange(ctx, lo, hi, fmt.Sprintf("created:%d-01-01..%d-12-31", y, y), save)
				if err != nil {
					return seen, err
				}
				seen += m
			}
			continue
		}
		top := hi
		if top < 0 {
			top = lo * 4
		}
		mid := lo + (top-lo)/2
		queue = append(queue, [2]int{lo, mid}, [2]int{mid + 1, hi})
	}
	return seen, nil
}

// collectRange fetches one range. It reports split=true without saving when the
// range holds more than the search cap.
func (g *GitHub) collectRange(ctx context.Context, lo, hi int, extra string, save func([]store.Repo) error) (int, bool, error) {
	stars := fmt.Sprintf("stars:>=%d", lo)
	if hi >= 0 {
		stars = fmt.Sprintf("stars:%d..%d", lo, hi)
	}
	q := stars + " fork:false archived:false"
	if extra != "" {
		q += " " + extra
	}
	n := 0
	for page := 1; page <= searchCap/100; page++ {
		var res ghSearch
		if err := g.search(ctx, q, page, &res); err != nil {
			return n, false, err
		}
		if page == 1 && res.Total > searchCap && extra == "" {
			return 0, true, nil
		}
		if res.Incomplete {
			g.Log.Warn("incomplete search results", "query", q, "page", page)
		}
		batch := make([]store.Repo, 0, len(res.Items))
		for _, it := range res.Items {
			batch = append(batch, store.Repo{Source: "github", ExtID: it.ID, FullName: it.FullName, URL: it.HTMLURL,
				Description: it.Description, Language: it.Language, CreatedAt: it.CreatedAt, Stars: it.Stars})
		}
		if err := save(batch); err != nil {
			return n, false, err
		}
		n += len(batch)
		if len(res.Items) < 100 || page*100 >= res.Total {
			break
		}
	}
	return n, false, nil
}

func (g *GitHub) search(ctx context.Context, q string, page int, out *ghSearch) error {
	if wait := searchPace - time.Since(g.lastQuery); wait > 0 {
		if err := sleep(ctx, wait); err != nil {
			return err
		}
	}
	g.lastQuery = time.Now()
	v := url.Values{"q": {q}, "sort": {"stars"}, "order": {"desc"}, "per_page": {"100"}, "page": {strconv.Itoa(page)}}
	_, err := g.get(ctx, "/search/repositories?"+v.Encode(), "application/vnd.github+json", out)
	return err
}

// get sends one request, waiting out rate limits. It retries a few times on
// rate limits and server errors.
func (g *GitHub) get(ctx context.Context, path, accept string, out any) (http.Header, error) {
	base := g.API
	if base == "" {
		base = "https://api.github.com"
	}
	var lastErr error
	for attempt := 0; attempt < 6; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+path, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Accept", accept)
		req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
		req.Header.Set("User-Agent", "star-monitor")
		if g.Token != "" {
			req.Header.Set("Authorization", "Bearer "+g.Token)
		}
		g.Requests++
		res, err := g.HTTP.Do(req)
		if err != nil {
			lastErr = err
			if err := sleep(ctx, time.Duration(attempt+1)*5*time.Second); err != nil {
				return nil, err
			}
			continue
		}
		body, err := io.ReadAll(io.LimitReader(res.Body, 32<<20))
		res.Body.Close()
		if err != nil {
			lastErr = err
			continue
		}
		switch {
		case res.StatusCode == http.StatusOK:
			return res.Header, json.Unmarshal(body, out)
		case res.StatusCode == http.StatusForbidden || res.StatusCode == http.StatusTooManyRequests:
			wait := rateWait(res.Header)
			g.Log.Warn("rate limited", "status", res.StatusCode, "wait", wait.Round(time.Second))
			lastErr = fmt.Errorf("rate limited: %s", res.Status)
			if err := sleep(ctx, wait); err != nil {
				return nil, err
			}
		case res.StatusCode >= 500:
			lastErr = fmt.Errorf("github: %s", res.Status)
			if err := sleep(ctx, time.Duration(attempt+1)*10*time.Second); err != nil {
				return nil, err
			}
		default:
			return res.Header, &StatusError{Code: res.StatusCode, Body: string(truncate(body, 200))}
		}
	}
	return nil, lastErr
}

// StatusError is a non-retryable HTTP error.
type StatusError struct {
	Code int
	Body string
}

func (e *StatusError) Error() string { return fmt.Sprintf("HTTP %d: %s", e.Code, e.Body) }

func rateWait(h http.Header) time.Duration {
	if s, err := strconv.Atoi(h.Get("Retry-After")); err == nil {
		return time.Duration(s)*time.Second + time.Second
	}
	if h.Get("X-RateLimit-Remaining") == "0" {
		if reset, err := strconv.ParseInt(h.Get("X-RateLimit-Reset"), 10, 64); err == nil {
			if d := time.Until(time.Unix(reset, 0)); d > 0 {
				return d + 2*time.Second
			}
		}
	}
	return time.Minute
}

var lastPageRE = regexp.MustCompile(`[?&]page=(\d+)[^>]*>;\s*rel="last"`)

// ErrTooManyPages means the stargazer list is longer than GitHub will page through.
var ErrTooManyPages = errors.New("stargazer list too long to page")

// MaxStargazerPages is how far GitHub lets the stargazer list be paged.
var MaxStargazerPages = 400

// StarsSince reconstructs daily star counts after since from the stargazer list,
// newest pages first. total is the current star count. Unstars are invisible,
// so values are approximate.
func (g *GitHub) StarsSince(ctx context.Context, fullName string, total int, since time.Time, budget int) (map[int64]int, error) {
	const accept = "application/vnd.github.star+json"
	path := "/repos/" + fullName + "/stargazers?per_page=100"
	var first []struct {
		StarredAt time.Time `json:"starred_at"`
	}
	h, err := g.get(ctx, path+"&page=1", accept, &first)
	if err != nil {
		return nil, err
	}
	last := 1
	if m := lastPageRE.FindStringSubmatch(h.Get("Link")); m != nil {
		last, _ = strconv.Atoi(m[1])
	}
	if last > MaxStargazerPages {
		return nil, ErrTooManyPages
	}
	// Count stars per day, walking back from the newest page.
	perDay := map[int64]int{}
	done := false
	for page := last; page >= 1 && !done; page-- {
		if budget--; budget < 0 {
			return nil, errors.New("request budget exhausted")
		}
		items := first
		if page != 1 {
			items = nil
			if _, err := g.get(ctx, fmt.Sprintf("%s&page=%d", path, page), accept, &items); err != nil {
				return nil, err
			}
		}
		for i := len(items) - 1; i >= 0; i-- {
			if items[i].StarredAt.Before(since) {
				done = true
				break
			}
			perDay[store.Day(items[i].StarredAt)]++
		}
	}
	// End-of-day totals: today's total minus stars added after each day.
	out := map[int64]int{}
	today := store.Day(time.Now())
	running := total
	for d := today; d >= store.Day(since); d-- {
		out[d] = running
		running -= perDay[d]
	}
	return out, nil
}

func truncate(b []byte, n int) []byte {
	if len(b) > n {
		return b[:n]
	}
	return b
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
