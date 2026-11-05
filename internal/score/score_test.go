package score

import (
	"math"
	"sort"
	"testing"
)

// curve builds 16 daily totals (15 days) from a start value and per-day gains.
func curve(start int, gains []float64) []int {
	out := []int{start}
	acc := float64(start)
	for _, g := range gains {
		acc += g
		out = append(out, int(math.Round(acc)))
	}
	return out
}

func steady(total float64) []float64 {
	g := make([]float64, 15)
	for i := range g {
		g[i] = total / 15
	}
	return g
}

func weighted(total, ratio float64) []float64 {
	g := make([]float64, 15)
	var sum float64
	for i := range g {
		g[i] = math.Pow(ratio, float64(i))
		sum += g[i]
	}
	for i := range g {
		g[i] *= total / sum
	}
	return g
}

func spike(total float64, day int) []float64 {
	g := steady(total * 0.2)
	g[day] += total * 0.8
	return g
}

func TestRankingMatchesAgreedCases(t *testing.T) {
	cases := []struct {
		name  string
		daily []int
	}{
		{"20k accelerating", curve(20000, weighted(6000, 1.15))},
		{"5k accelerating", curve(5000, weighted(3000, 1.15))},
		{"3k spike yesterday", curve(3000, spike(6000, 13))},
		{"5k steady", curve(5000, steady(3000))},
		{"200k steady big gain", curve(200000, steady(12000))},
		{"1.5k steady", curve(1500, steady(2000))},
		{"5k fading", curve(5000, weighted(3000, 0.8))},
		{"3k spike 12 days ago", curve(3000, spike(6000, 2))},
		{"300 small", curve(300, steady(1200))},
		{"200k routine", curve(200000, steady(3000))},
		{"new 0 to 5000", curve(0, steady(5000))},
	}
	got := make([]string, len(cases))
	scores := map[string]float64{}
	for i, c := range cases {
		got[i] = c.name
		scores[c.name] = Compute(c.daily, Default).Score
	}
	sort.SliceStable(got, func(i, j int) bool { return scores[got[i]] > scores[got[j]] })
	for i, c := range cases {
		if got[i] != c.name {
			t.Fatalf("rank %d: got %q, want %q (scores %v)", i+1, got[i], c.name, scores)
		}
	}
	if scores["new 0 to 5000"] != 0 {
		t.Fatalf("a repository with no stars at the window start must score 0")
	}
}

func TestSpikeFlag(t *testing.T) {
	if !Compute(curve(3000, spike(6000, 13)), Default).Spike {
		t.Fatal("one-day spike not flagged")
	}
	if Compute(curve(5000, steady(3000)), Default).Spike {
		t.Fatal("steady growth flagged as spike")
	}
}

func TestMissingDays(t *testing.T) {
	// Leading gaps shorten the window; inner gaps carry the last value forward.
	r := Compute([]int{-1, -1, 1000, -1, 1100, 1200}, Default)
	if r.Days != 3 || r.S0 != 1000 || r.Delta != 200 {
		t.Fatalf("got %+v", r)
	}
	if Compute([]int{-1, 500}, Default).Score != 0 {
		t.Fatal("one data point must not score")
	}
	if Compute([]int{5000, 4900}, Default).Score != 0 {
		t.Fatal("losing stars must not score")
	}
}
