package dist

import (
	"math"
	"math/rand/v2"
	"sort"
	"testing"
)

const draws = 20000

func rng(stream uint64) *rand.Rand { return rand.New(rand.NewPCG(1, stream)) }

// logNormal returns n draws from a log-normal with the given median and σ.
func logNormal(r *rand.Rand, n int, median, sigma float64) []float64 {
	xs := make([]float64, n)
	for i := range xs {
		xs[i] = median * math.Exp(sigma*r.NormFloat64())
	}
	return xs
}

func quantiles(d *Dist, r *rand.Rand, qs ...float64) []float64 {
	return Quantiles(draws, func() float64 { return d.Sample(r) }, qs...)
}

func within(t *testing.T, name string, got, want, rel float64) {
	t.Helper()
	if math.Abs(got-want) > rel*want {
		t.Errorf("%s = %.2f, want %.2f ± %.0f%%", name, got, want, rel*100)
	}
}

func TestPriorQuantile(t *testing.T) {
	p := NewPrior(60)
	within(t, "P50", p.Quantile(0.5), 60, 1e-9)
	within(t, "P80", p.Quantile(0.8), 60*math.Exp(0.8416212335729143), 1e-9)
	within(t, "P99", p.Quantile(0.99), 60*math.Exp(2.3263478740408408), 1e-9)
}

// With no history a chain is its prior, capped at the prior's P99.
func TestNoHistoryIsThePrior(t *testing.T) {
	prior := NewPrior(60)
	d := NewRoot(prior, nil, 3).Child(nil, 10).Child(nil, 10)
	q := quantiles(d, rng(1), 0.5, 0.8, 1)
	within(t, "P50", q[0], prior.Quantile(0.5), 0.05)
	within(t, "P80", q[1], prior.Quantile(0.8), 0.05)
	if cap99 := prior.Quantile(0.99); q[2] != cap99 {
		t.Errorf("max draw = %v, want exactly the prior P99 %v (clamped)", q[2], cap99)
	}
}

// Plenty of data from a known log-normal: the draws reproduce its quantiles,
// widened only slightly by the smoothing. k = 0 isolates the own-data draw
// (TestPoolingWeight covers the pooling).
func TestRecoversKnownLogNormal(t *testing.T) {
	const median, sigma = 30.0, 0.5
	obs := logNormal(rng(2), 1000, median, sigma)
	d := NewRoot(NewPrior(600), obs, 3).Child(obs, 0)
	q := quantiles(d, rng(3), 0.5, 0.8, 0.95)
	within(t, "P50", q[0], median, 0.08)
	within(t, "P80", q[1], median*math.Exp(sigma*0.8416), 0.08)
	within(t, "P95", q[2], median*math.Exp(sigma*1.6449), 0.10)
}

// A class with n observations draws from them with probability n/(n+k).
func TestPoolingWeight(t *testing.T) {
	far := Prior{Median: 10000, Sigma: 0.1}
	for _, tt := range []struct {
		n    int
		k    float64
		want float64
	}{{0, 10, 0}, {1, 10, 1.0 / 11}, {10, 10, 0.5}, {100, 10, 100.0 / 110}, {5, 0, 1}} {
		obs := make([]float64, tt.n)
		for i := range obs {
			obs[i] = 10
		}
		d := NewRoot(far, nil, 3).Child(obs, tt.k)
		r := rng(4)
		own := 0
		for i := 0; i < draws; i++ {
			if d.Sample(r) < 1000 {
				own++
			}
		}
		if got := float64(own) / draws; math.Abs(got-tt.want) > 0.015 {
			t.Errorf("n=%d k=%v: own-data share = %.3f, want %.3f", tt.n, tt.k, got, tt.want)
		}
	}
}

// A bimodal class (clean pass vs rejection-and-retry) stays bimodal. A
// log-normal fitted to the same data (μ ≈ ln 55, σ ≈ 1.7) would put about 35%
// of its mass between the modes; Silverman's rule oversmooths a little, so
// allow up to 10%.
func TestKeepsBimodality(t *testing.T) {
	r := rng(5)
	obs := append(logNormal(r, 200, 10, 0.2), logNormal(r, 200, 300, 0.2)...)
	d := NewRoot(NewPrior(60), obs, 3).Child(obs, 10)
	var low, middle, high int
	for i := 0; i < draws; i++ {
		switch x := d.Sample(r); {
		case x < 25:
			low++
		case x < 120:
			middle++
		default:
			high++
		}
	}
	if share := float64(middle) / draws; share > 0.10 {
		t.Errorf("%.1f%% of draws between the modes, want < 10%%", share*100)
	}
	if lo, hi := float64(low)/draws, float64(high)/draws; math.Abs(lo-hi) > 0.05 {
		t.Errorf("mode shares %.2f / %.2f, want about equal", lo, hi)
	}
}

// Fat tails are kept: draws can modestly exceed the largest observation but
// never the tail cap.
func TestTailCap(t *testing.T) {
	obs := []float64{5, 10, 20, 40, 100}
	// A far prior makes the parent draws land on the cap.
	d := NewRoot(Prior{Median: 1e6, Sigma: 1}, obs, 3).Child(obs, 10)
	r := rng(6)
	var beyond, capped int
	for i := 0; i < draws; i++ {
		x := d.Sample(r)
		if x > 300 {
			t.Fatalf("draw %v exceeds the cap 3 × 100", x)
		}
		if x > 100 && x < 300 {
			beyond++
		}
		if x == 300 {
			capped++
		}
	}
	if beyond == 0 {
		t.Error("no draw exceeded the largest observation; smoothing should allow it")
	}
	if capped == 0 {
		t.Error("no draw reached the cap, but the far prior should be clamped to it")
	}
}

func TestObservationsFlooredAtOneMinute(t *testing.T) {
	d := NewRoot(NewPrior(60), []float64{0, 0, 0}, 3).Child([]float64{0, 0, 0}, 0)
	if d.cap != 3 {
		t.Errorf("cap = %v, want 3 × the 1-minute floor", d.cap)
	}
	for _, x := range d.logs {
		if x != 0 {
			t.Errorf("log observation %v, want log(1) = 0", x)
		}
	}
}

func TestSampleBeyond(t *testing.T) {
	obs := logNormal(rng(7), 500, 60, 0.6)
	d := NewRoot(NewPrior(60), obs, 3).Child(obs, 10)

	t.Run("conditional", func(t *testing.T) {
		r := rng(8)
		const elapsed = 90.0
		var total []float64
		for i := 0; i < draws; i++ {
			rem := d.SampleBeyond(r, elapsed)
			if rem <= 0 {
				t.Fatalf("remaining %v, want > 0", rem)
			}
			total = append(total, elapsed+rem)
		}
		// Survivors of 90 minutes: their total median is the conditional
		// median, above both the elapsed time and the unconditioned median.
		want := Quantiles(draws, func() float64 {
			for {
				if x := d.Sample(r); x > elapsed {
					return x
				}
			}
		}, 0.5)[0]
		sort.Float64s(total)
		within(t, "conditional P50", Quantile(total, 0.5), want, 0.05)
	})

	t.Run("beyond all history", func(t *testing.T) {
		// Past the cap no draw can exceed elapsed: Lindy's draw is returned.
		r1, r2 := rng(9), rng(9)
		for i := 0; i < 100; i++ {
			if got, want := d.SampleBeyond(r1, 1e9), Lindy(r2, 1e9); got != want {
				t.Fatalf("SampleBeyond past the cap = %v, want Lindy's %v", got, want)
			}
		}
		// Below the cap but past every observation, the tries fail too.
		r := rng(13)
		elapsed := 0.9 * d.cap
		rem := Quantiles(draws, func() float64 { return d.SampleBeyond(r, elapsed) }, 0.5)[0]
		within(t, "remaining P50", rem, elapsed, 0.05)
	})

	t.Run("not started", func(t *testing.T) {
		r1, r2 := rng(10), rng(10)
		if got, want := d.SampleBeyond(r1, 0), d.Sample(r2); got != want {
			t.Errorf("SampleBeyond(0) = %v, want Sample %v", got, want)
		}
	})
}

// Lindy: a duration that has lasted a while lasts about as long again, and
// is not clamped at the tail cap.
func TestLindy(t *testing.T) {
	r := rng(14)
	const elapsed = 1e6
	q := Quantiles(draws, func() float64 { return Lindy(r, elapsed) }, 0.5, 0.8)
	within(t, "P50", q[0], elapsed, 0.03)
	within(t, "P80", q[1], elapsed*math.Exp(0.8416), 0.04)
	if got := Lindy(r, 0); got != MinMinutes {
		t.Errorf("Lindy(0) = %v, want the 1-minute floor", got)
	}
}

// Without censored durations a Kaplan–Meier node is a plain one.
func TestCensoredWithoutCensoring(t *testing.T) {
	obs := logNormal(rng(15), 200, 30, 0.5)
	root := NewRoot(NewPrior(60), obs, 3)
	a, b := root.Child(obs, 10), root.Censored(obs, nil, 10)
	r1, r2 := rng(16), rng(16)
	for i := 0; i < 1000; i++ {
		if x, y := a.Sample(r1), b.Sample(r2); x != y {
			t.Fatalf("draw %d: Censored %v != Child %v", i, y, x)
		}
	}
}

// Kaplan–Meier recovers a distribution from data censored at random, where
// the completed durations alone are biased short.
func TestCensoredRecoversKnownLogNormal(t *testing.T) {
	const median, sigma = 60.0, 0.8
	r := rng(17)
	var events, censored []float64
	for i := 0; i < 4000; i++ {
		d := median * math.Exp(sigma*r.NormFloat64())
		// Observed for a random time: beads opened at random moments.
		if c := 400 * r.Float64(); c < d {
			censored = append(censored, c)
		} else {
			events = append(events, d)
		}
	}
	root := NewRoot(NewPrior(600), append(append([]float64(nil), events...), censored...), 3)
	km := root.Censored(events, censored, 0)
	naive := root.Child(events, 0)
	q := quantiles(km, rng(18), 0.5, 0.8)
	within(t, "P50", q[0], median, 0.08)
	within(t, "P80", q[1], median*math.Exp(sigma*0.8416), 0.10)
	if n := quantiles(naive, rng(18), 0.8)[0]; n >= 0.9*q[1] {
		t.Errorf("completed durations alone give P80 %.1f, want clearly below the Kaplan–Meier %.1f", n, q[1])
	}
}

// Survival mass left beyond the last completed duration is drawn past it.
func TestCensoredTail(t *testing.T) {
	events := []float64{10, 20, 30, 40}
	censored := []float64{100, 200, 300, 400} // half the class outlasts every completed bead
	d := NewRoot(Prior{Median: 1e6, Sigma: 1}, append(append([]float64(nil), events...), censored...), 3).Censored(events, censored, 0)
	if d.N() != 4 || d.own != 1 {
		t.Errorf("N = %d, own = %v; want 4 events and all draws own with k = 0", d.N(), d.own)
	}
	r := rng(19)
	beyond := 0
	for i := 0; i < draws; i++ {
		x := d.Sample(r)
		if x > 1200 {
			t.Fatalf("draw %v exceeds the cap 3 × 400", x)
		}
		if x > 40 {
			beyond++
		}
	}
	// Half the mass is past the last event (40). Smoothing adds about half
	// of that event's own mass of 1/8, and a little of the others'.
	if share := float64(beyond) / draws; share < 0.53 || share > 0.62 {
		t.Errorf("%.2f of draws beyond the last completed duration, want about half", share)
	}

	// Nothing completed yet: everything is tail, past the longest running.
	all := NewRoot(NewPrior(60), censored, 3).Censored(nil, censored, 0)
	for i := 0; i < 1000; i++ {
		if x := all.Sample(r); x < 400 {
			t.Fatalf("draw %v with no completed duration, want past the longest running 400", x)
		}
	}
	// Pooling counts censored durations too.
	if pooled := NewRoot(NewPrior(60), censored, 3).Censored(events, censored, 8); pooled.own != 0.5 {
		t.Errorf("own = %v, want 8 / (8 + 8)", pooled.own)
	}
}

func TestSeededDrawsAreReproducible(t *testing.T) {
	obs := logNormal(rng(11), 50, 30, 1)
	d := NewRoot(NewPrior(60), obs, 3).Child(obs[:10], 10)
	r1, r2 := rng(12), rng(12)
	for i := 0; i < 1000; i++ {
		if a, b := d.Sample(r1), d.Sample(r2); a != b {
			t.Fatalf("draw %d: %v != %v with the same seed", i, a, b)
		}
	}
}

func TestBandwidth(t *testing.T) {
	if h := bandwidth(nil); h != minBandwidth {
		t.Errorf("no observations: h = %v, want %v", h, minBandwidth)
	}
	if h := bandwidth([]float64{3}); h != minBandwidth {
		t.Errorf("one observation: h = %v, want %v", h, minBandwidth)
	}
	if h := bandwidth([]float64{2, 2, 2, 2}); h != minBandwidth {
		t.Errorf("identical observations: h = %v, want %v", h, minBandwidth)
	}
	// 0..10: sd = 3.3166, IQR = 5 → min(3.3166, 3.7313) = sd.
	xs := []float64{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10}
	want := 0.9 * math.Sqrt(11) * math.Pow(11, -0.2)
	within(t, "h", bandwidth(xs), want, 1e-9)
	// One outlier inflates sd but not the IQR: the IQR term wins.
	ys := []float64{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 100}
	within(t, "h", bandwidth(ys), 0.9*(5/1.34)*math.Pow(11, -0.2), 1e-9)
}

func TestQuantile(t *testing.T) {
	xs := []float64{10, 20, 30, 40}
	for _, tt := range []struct{ q, want float64 }{{0, 10}, {0.5, 25}, {1, 40}, {1.0 / 3, 20}} {
		if got := Quantile(xs, tt.q); math.Abs(got-tt.want) > 1e-9 {
			t.Errorf("Quantile(%v) = %v, want %v", tt.q, got, tt.want)
		}
	}
	if got := Quantile(nil, 0.5); !math.IsNaN(got) {
		t.Errorf("Quantile of nothing = %v, want NaN", got)
	}
}
