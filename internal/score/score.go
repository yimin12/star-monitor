// Package score ranks repositories by how much attention they are gaining now,
// relative to an audience they already had.
//
//	Score = A × B × M × P
//	A attention   = Δ / √(S0 + K)          between absolute and relative growth
//	B base        = S0 / (S0 + K)          zero for repositories that started from nothing
//	M momentum    = recent share / expected share, clamped to [MinM, MaxM]
//	P persistence = √(Neff / days), Neff = 1 / Σ(daily share²)
package score

import "math"

// Params are the tunable constants. K is in stars.
type Params struct {
	K            float64
	MomentumDays int
	MinM, MaxM   float64
	// A day share at or above SpikeShare marks the result as a one-day spike.
	SpikeShare float64
}

// Default is the starting point agreed on 2026-11-05; tune it against real data.
var Default = Params{K: 1000, MomentumDays: 5, MinM: 0.5, MaxM: 1.5, SpikeShare: 0.5}

// Result explains a score so the page can show why a repository ranks where it does.
type Result struct {
	S0, Now, Delta, Recent int
	A, B, M, P, Neff       float64
	Score                  float64
	Days                   int  // days of history covered by the window
	Spike                  bool // one day holds most of the gain
}

// Compute scores daily star counts, oldest first. The first value is the start
// of the window. Missing days (value < 0) carry the previous value forward;
// leading missing days shorten the window.
func Compute(daily []int, p Params) Result {
	vals := make([]int, 0, len(daily))
	for _, v := range daily {
		switch {
		case v >= 0:
			vals = append(vals, v)
		case len(vals) > 0:
			vals = append(vals, vals[len(vals)-1])
		}
	}
	r := Result{}
	if len(vals) < 2 {
		if len(vals) == 1 {
			r.S0, r.Now = vals[0], vals[0]
		}
		return r
	}
	days := len(vals) - 1
	r.Days, r.S0, r.Now = days, vals[0], vals[days]
	r.Delta = r.Now - r.S0
	if r.Delta <= 0 {
		return r
	}

	inc := make([]float64, days)
	var total float64
	for i := 1; i <= days; i++ {
		if d := vals[i] - vals[i-1]; d > 0 {
			inc[i-1] = float64(d)
			total += float64(d)
		}
	}
	var sumSq, maxShare float64
	for _, d := range inc {
		s := d / total
		sumSq += s * s
		maxShare = math.Max(maxShare, s)
	}
	r.Neff = 1 / sumSq
	r.Spike = maxShare >= p.SpikeShare

	recentDays := min(p.MomentumDays, days)
	r.Recent = r.Now - vals[days-recentDays]
	expected := float64(r.Delta) * float64(recentDays) / float64(days)
	r.M = clamp(float64(r.Recent)/expected, p.MinM, p.MaxM)

	s0 := float64(r.S0)
	r.A = float64(r.Delta) / math.Sqrt(s0+p.K)
	r.B = s0 / (s0 + p.K)
	r.P = math.Sqrt(r.Neff / float64(days))
	r.Score = r.A * r.B * r.M * r.P
	return r
}

func clamp(v, lo, hi float64) float64 { return math.Max(lo, math.Min(hi, v)) }
