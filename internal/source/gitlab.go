package source

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/yimin12/star-monitor/internal/store"
)

// GitLab pages public gitlab.com projects by star count, highest first, until
// star counts drop below MinStars. No token is needed.
type GitLab struct {
	MinStars int
	HTTP     *http.Client
	Log      *slog.Logger
	API      string // https://gitlab.com/api/v4

	Requests int
}

type glProject struct {
	ID          int64  `json:"id"`
	Path        string `json:"path_with_namespace"`
	WebURL      string `json:"web_url"`
	Description string `json:"description"`
	CreatedAt   string `json:"created_at"`
	Stars       int    `json:"star_count"`
	ForkedFrom  any    `json:"forked_from_project"`
	Archived    bool   `json:"archived"`
}

// Collect pages GitLab from the top. It is short, so it does not resume.
func (g *GitLab) Collect(ctx context.Context, save func([]store.Repo) error, _ *store.Progress) (int, error) {
	base := g.API
	if base == "" {
		base = "https://gitlab.com/api/v4"
	}
	seen := 0
	for page := 1; ; page++ {
		v := url.Values{"order_by": {"star_count"}, "sort": {"desc"}, "per_page": {"100"},
			"page": {strconv.Itoa(page)}, "visibility": {"public"}, "archived": {"false"}}
		var items []glProject
		if err := g.get(ctx, base+"/projects?"+v.Encode(), &items); err != nil {
			return seen, err
		}
		batch := make([]store.Repo, 0, len(items))
		done := len(items) < 100
		for _, p := range items {
			if p.Stars < g.MinStars {
				done = true
				break
			}
			if p.ForkedFrom != nil || p.Archived {
				continue
			}
			batch = append(batch, store.Repo{Source: "gitlab", ExtID: p.ID, FullName: p.Path, URL: p.WebURL,
				Description: p.Description, CreatedAt: p.CreatedAt, Stars: p.Stars})
		}
		if err := save(batch); err != nil {
			return seen, err
		}
		seen += len(batch)
		if done {
			return seen, nil
		}
		if err := sleep(ctx, 500*time.Millisecond); err != nil {
			return seen, err
		}
	}
}

func (g *GitLab) get(ctx context.Context, u string, out any) error {
	var lastErr error
	for attempt := 0; attempt < 5; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return err
		}
		req.Header.Set("User-Agent", "star-monitor")
		g.Requests++
		res, err := g.HTTP.Do(req)
		if err != nil {
			lastErr = err
		} else {
			body, rerr := io.ReadAll(io.LimitReader(res.Body, 32<<20))
			res.Body.Close()
			switch {
			case rerr != nil:
				lastErr = rerr
			case res.StatusCode == http.StatusOK:
				return json.Unmarshal(body, out)
			case res.StatusCode == http.StatusTooManyRequests || res.StatusCode >= 500:
				lastErr = fmt.Errorf("gitlab: %s", res.Status)
				g.Log.Warn("gitlab retry", "status", res.StatusCode)
			default:
				return &StatusError{Code: res.StatusCode, Body: string(truncate(body, 200))}
			}
		}
		if err := sleep(ctx, time.Duration(attempt+1)*15*time.Second); err != nil {
			return err
		}
	}
	return lastErr
}
