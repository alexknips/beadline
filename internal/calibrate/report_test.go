package calibrate

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

// leaves makes n leaf pairs, one bead each, of which held close by their
// P80 and the rest after their P95.
func leaves(n, held int) []Pair {
	grid := NewGrid(Levels, spread(10, 1.2))
	var out []Pair
	for k := 0; k < n; k++ {
		actual := 5.0
		if k >= held {
			actual = 500
		}
		p := resolved(fmt.Sprintf("b%d", k), grid, actual)
		p.Level, p.Method, p.Source = LevelLeaf, MethodModel, "backtest"
		out = append(out, p)
	}
	return out
}

func TestGate(t *testing.T) {
	for _, tt := range []struct {
		name   string
		pairs  []Pair
		pass   bool
		reason string
	}{
		{"held 80%", leaves(40, 32), true, ""},
		{"held 70%", leaves(40, 28), true, ""},
		{"held 50%", leaves(40, 20), false, "too narrow"},
		{"held 95%", leaves(40, 38), false, "too wide"},
		{"too few beads", leaves(10, 8), false, "only 10 leaf beads"},
		{"nothing known", nil, false, "no leaf forecast"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			g := NewReport("backtest", now, tt.pairs).Gate
			if g == nil || g.Pass != tt.pass || !strings.Contains(g.Reason, tt.reason) || (tt.reason == "" && g.Reason != "") {
				t.Errorf("gate = %+v, want pass %v and a reason with %q", g, tt.pass, tt.reason)
			}
		})
	}
	if g := NewReport("snapshots", now, leaves(40, 32)).Gate; g != nil {
		t.Errorf("snapshot grading has a release gate: %+v", g)
	}
}

func TestReportText(t *testing.T) {
	pairs := leaves(30, 24)
	for _, p := range leaves(30, 10) {
		p.Method = MethodBaseline
		pairs = append(pairs, p)
	}
	hl := func(id string, added int, actual float64) Pair {
		p := resolved(id, NewGrid(Levels, spread(100, 1.2)), actual)
		p.Level, p.Method, p.ScopeAdded = LevelHighLevel, MethodModel, added
		return p
	}
	pairs = append(pairs, hl("E1", 0, 50), hl("E2", 3, 900), Pair{ID: "E3", Level: LevelHighLevel, Method: MethodModel, Outcome: OutcomeVoid, Void: VoidVanished})
	r := NewReport("backtest", now, pairs)
	r.Origins, r.Runs = []time.Time{now.Add(-30 * 24 * time.Hour), now.Add(-10 * 24 * time.Hour)}, 500
	since := now.Add(-90 * 24 * time.Hour)
	r.DependencySkewHours, r.SkewSince = 2, &since

	var out bytes.Buffer
	r.WriteText(&out)
	text := out.String()
	for _, want := range []string{
		"backtest: 2 origins from 2026-08-20 12:00 to 2026-09-09 12:00, 500 runs each; outcomes known to 2026-09-19 12:00 UTC\n",
		"dependency timestamps corrected by -2h from 2026-06-21 12:00 UTC",
		"LEAF BEADS (the honest sample)\n  30 forecasts: 30 resolved, 0 still open (right-censored); 30 distinct beads scored\n",
		"METHOD    QUANTILE  HELD", "model     P80       24/30  80%", "baseline  P80       10/30  33%",
		"model error: bias", "baseline error: bias", "model PIT by fifths",
		"HIGH-LEVEL (milestones, epics, goals)\n  3 forecasts: 2 resolved, 0 still open (right-censored), 1 void (1 vanished); 2 distinct items scored\n",
		"! only 2 distinct items (< 20)",
		"  fixed scope: 1 forecast, P80 held 1/1 (100%)\n  scope changed: 1 forecast, P80 held 0/1 (0%)\n",
		"release gate (leaf P80 held 70%-90%): PASS 80%\n",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("report does not contain %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "distinct beads (<") {
		t.Errorf("30 leaf beads flagged as few:\n%s", text)
	}

	data, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	var back map[string]any
	if err := json.Unmarshal(data, &back); err != nil || back["mode"] != "backtest" || len(back["pairs"].([]any)) != len(pairs) {
		t.Errorf("JSON report = %.300s (%v)", data, err)
	}

	empty := NewReport("snapshots", now, nil)
	empty.Snapshots = []string{"x.json"}
	out.Reset()
	empty.WriteText(&out)
	if !strings.Contains(out.String(), "1 snapshot graded against data as of 2026-09-19 12:00 UTC") || !strings.Contains(out.String(), "no forecasts to grade") ||
		strings.Contains(out.String(), "release gate") {
		t.Errorf("empty report:\n%s", out.String())
	}
}
