// Package web serves the ranking page. htmx swaps the table on filter changes;
// without JavaScript the same form reloads the page.
package web

import (
	"embed"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/yimin12/star-monitor/internal/store"
)

//go:embed templates/*.html static/*
var assets embed.FS

type Server struct {
	Store    *store.Store
	Log      *slog.Logger
	Window   int
	Interval time.Duration
	Running  func() bool

	tmpl *template.Template
}

func (s *Server) Handler() http.Handler {
	s.tmpl = template.Must(template.New("").Funcs(template.FuncMap{
		"num":   humanNum,
		"spark": sparkline,
		"fixed": func(f float64) string { return strconv.FormatFloat(f, 'f', 1, 64) },
		"pct": func(f float64) string {
			if f < 0 {
				return "—"
			}
			return strconv.FormatFloat(f, 'f', 0, 64) + "%"
		},
		"ago":   ago,
		"inc":   func(i int) int { return i + 1 },
		"list":  func(v ...string) []string { return v },
		"short": func(t time.Time) string { return t.Local().Format("01-02 15:04") },
	}).ParseFS(assets, "templates/*.html"))

	static, _ := fs.Sub(assets, "static")
	mux := http.NewServeMux()
	mux.Handle("GET /static/", http.StripPrefix("/static/", cacheFor(http.FileServer(http.FS(static)), 24*time.Hour)))
	mux.HandleFunc("GET /{$}", s.page)
	mux.HandleFunc("GET /rows", s.rows)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok\n")) })
	return securityHeaders(mux)
}

type view struct {
	Q         store.Query
	Rows      []store.Row
	Languages []string
	Runs      []store.LastRun
	Coverage  int
	Window    int
	Running   bool
	NextRun   time.Time
	Err       string
}

func (s *Server) query(r *http.Request) store.Query {
	q := store.Query{
		Source:     r.URL.Query().Get("source"),
		Language:   r.URL.Query().Get("lang"),
		Sort:       r.URL.Query().Get("sort"),
		HideSpikes: r.URL.Query().Get("nospike") == "1",
		Limit:      100,
	}
	if q.Source != "gitlab" {
		q.Source = "github"
	}
	q.MinStars, _ = strconv.Atoi(r.URL.Query().Get("min"))
	return q
}

func (s *Server) load(r *http.Request) view {
	ctx := r.Context()
	v := view{Q: s.query(r), Window: s.Window, Running: s.Running()}
	var err error
	if v.Rows, err = s.Store.Top(ctx, v.Q); err != nil {
		s.Log.Error("top", "err", err)
		v.Err = "读取榜单失败"
	}
	v.Languages, _ = s.Store.Languages(ctx, v.Q.Source, 30)
	v.Runs, _ = s.Store.LastRuns(ctx)
	v.Coverage, _ = s.Store.Coverage(ctx, v.Q.Source)
	if last, _ := s.Store.LastSuccess(ctx, "github"); !last.IsZero() {
		v.NextRun = last.Add(s.Interval)
	}
	return v
}

func (s *Server) page(w http.ResponseWriter, r *http.Request) { s.render(w, "page", s.load(r)) }
func (s *Server) rows(w http.ResponseWriter, r *http.Request) { s.render(w, "rows", s.load(r)) }

func (s *Server) render(w http.ResponseWriter, name string, v view) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tmpl.ExecuteTemplate(w, name, v); err != nil {
		s.Log.Error("render", "template", name, "err", err)
	}
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'self' https://home.dev.topymh.org")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}

func cacheFor(next http.Handler, d time.Duration) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", fmt.Sprintf("public, max-age=%d", int(d.Seconds())))
		next.ServeHTTP(w, r)
	})
}

func humanNum(n int) string {
	switch {
	case n >= 1_000_000:
		return strconv.FormatFloat(float64(n)/1e6, 'f', 1, 64) + "M"
	case n >= 10_000:
		return strconv.FormatFloat(float64(n)/1e3, 'f', 1, 64) + "k"
	default:
		return strconv.Itoa(n)
	}
}

func ago(t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	d := time.Since(t)
	switch {
	case d < 0:
		return "约 " + (-d).Round(time.Minute).String() + " 后"
	case d < time.Hour:
		return strconv.Itoa(int(d.Minutes())) + " 分钟前"
	case d < 48*time.Hour:
		return strconv.Itoa(int(d.Hours())) + " 小时前"
	default:
		return strconv.Itoa(int(d.Hours()/24)) + " 天前"
	}
}

// sparkline draws daily gains as bars. Missing days are left empty.
func sparkline(series []int) template.HTML {
	const w, h, gap = 90.0, 22.0, 1.0
	n := len(series) - 1
	if n < 1 {
		return ""
	}
	inc := make([]float64, n)
	known := make([]bool, n)
	var max float64
	for i := 1; i <= n; i++ {
		if series[i] >= 0 && series[i-1] >= 0 {
			inc[i-1] = math.Max(0, float64(series[i]-series[i-1]))
			known[i-1] = true
			max = math.Max(max, inc[i-1])
		}
	}
	bw := (w - gap*float64(n-1)) / float64(n)
	var b strings.Builder
	fmt.Fprintf(&b, `<svg class="spark" viewBox="0 0 %.0f %.0f" width="%.0f" height="%.0f" role="img" aria-label="每日增星">`, w, h, w, h)
	for i := 0; i < n; i++ {
		x := float64(i) * (bw + gap)
		if !known[i] {
			fmt.Fprintf(&b, `<rect class="none" x="%.1f" y="%.1f" width="%.1f" height="1"/>`, x, h-1, bw)
			continue
		}
		bh := 1.0
		if max > 0 {
			bh = math.Max(1, inc[i]/max*h)
		}
		fmt.Fprintf(&b, `<rect x="%.1f" y="%.1f" width="%.1f" height="%.1f"><title>+%d</title></rect>`, x, h-bh, bw, bh, int(inc[i]))
	}
	b.WriteString(`</svg>`)
	return template.HTML(b.String())
}
