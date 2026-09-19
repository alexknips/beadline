package render

import (
	"bytes"
	"flag"
	"html"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/alexknips/beadline/internal/config"
	"github.com/alexknips/beadline/internal/load"
	"github.com/alexknips/beadline/internal/roadmap"
)

var update = flag.Bool("update", false, "rewrite the golden files in testdata")

var now = time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)

// built is the multirepo fixture as Build sees it: no forecast.
func built(t *testing.T) *roadmap.Roadmap {
	t.Helper()
	cfg, err := config.Load(filepath.Join("..", "..", "testdata", "multirepo", "beadline.toml"))
	if err != nil {
		t.Fatal(err)
	}
	g, rep, err := load.Load(cfg)
	if err != nil {
		t.Fatal(err)
	}
	r := roadmap.Build(g, rep, cfg, now)
	r.BeadlineVersion = "test"
	if r.Inputs, err = roadmap.Fingerprint(cfg); err != nil {
		t.Fatal(err)
	}
	return r
}

// forecast is the fixture with made-up forecasts standing in for the
// forecaster, plus rows for every status and a hostile title.
func forecast(t *testing.T) *roadmap.Roadmap {
	t.Helper()
	r := built(t)
	r.ModelVersion = "adr-1"
	day := func(n float64) *time.Time { d := now.Add(time.Duration(n * 24 * float64(time.Hour))); return &d }
	f := func(v float64) *float64 { return &v }
	set := func(o *roadmap.Outlook, p50, p80, p95 float64) {
		o.P50, o.P80, o.P95 = day(p50), day(p80), day(p95)
		o.AgentHours, o.HumanHours, o.RatePerDay = f(p50*6), f(p50*2), f(3.5)
	}
	for n := range r.Milestones {
		m := &r.Milestones[n]
		switch m.ID {
		case "api-m1": // target 2026-10-15: on track
			set(&m.Outlook, 6, 12, 25)
			m.CriticalChain = []string{"api-3", "api-4", "api-5"}
		case "api-e1":
			set(&m.Outlook, 1, 2.5, 4)
		case "web-e1":
			set(&m.Outlook, 14, 30, 55)
			m.TargetDueAt = day(20) // at risk
		}
	}
	g := &r.Goals[0] // hq-g1
	set(&g.Outlook, 15, 31, 80)
	g.TargetDueAt = day(10) // late

	outlook := func(status string, target *time.Time) roadmap.Outlook {
		return roadmap.Outlook{Status: status, TargetDueAt: target, Total: 2, Done: 1, Remaining: 1, DonePct: 50,
			RemainingIDs: []string{"x-1"}}
	}
	stalled := outlook(roadmap.StatusStalled, nil)
	outside := outlook(roadmap.StatusBlockedOutside, nil)
	outside.WaitsOnUnloaded = []string{"ops-7"}
	set(&outside, 3, 5, 9)
	ready := outlook(roadmap.StatusReadyToClose, nil)
	ready.Total, ready.Done, ready.Remaining, ready.DonePct, ready.RemainingIDs = 2, 2, 0, 100, []string{}
	unplanned := roadmap.Outlook{Status: roadmap.StatusNotPlanned, TargetDueAt: day(-3), RemainingIDs: []string{}}
	r.Milestones = append(r.Milestones,
		roadmap.Milestone{ID: "web-e3", Title: `<script>alert("x")</script> & </svg> "quoted" titles stay text`, Repo: "web", Type: "epic", Outlook: stalled},
		roadmap.Milestone{ID: "web-e4", Title: "Partner SSO, waiting on the ops repo", Repo: "web", Type: "epic", Outlook: outside},
		roadmap.Milestone{ID: "api-e2", Title: "Deprecate v0 endpoints", Repo: "api", Type: "epic", Outlook: ready},
		roadmap.Milestone{ID: "hq-m9", Title: "A milestone with a very long title that has to be cut before it runs into the bars", Repo: "hq", Type: "milestone", Outlook: unplanned},
	)
	r.Calibration = &roadmap.Calibration{Samples: 12, P50Coverage: 0.5, P80Coverage: 0.75}
	r.Assess()
	return r
}

func renderString(t *testing.T, r *roadmap.Roadmap) string {
	t.Helper()
	var buf bytes.Buffer
	if err := Render(&buf, r, Options{Title: "Fixture roadmap"}); err != nil {
		t.Fatal(err)
	}
	return buf.String()
}

func TestRenderGolden(t *testing.T) {
	for _, tt := range []struct {
		name string
		r    func(*testing.T) *roadmap.Roadmap
	}{{"forecast.html", forecast}, {"built.html", built}} {
		t.Run(tt.name, func(t *testing.T) {
			got := renderString(t, tt.r(t))
			path := filepath.Join("testdata", tt.name)
			if *update {
				if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("%v (run go test -update to create it)", err)
			}
			if got != string(want) {
				t.Errorf("page differs from %s; run go test ./internal/render -update and review the diff", path)
			}
		})
	}
}

// TestSelfContained checks the page can be served from anywhere: nothing it
// loads or links to, and no script that could reach the network.
func TestSelfContained(t *testing.T) {
	for _, page := range []string{renderString(t, forecast(t)), renderString(t, built(t))} {
		selfContained(t, page)
	}
	if m := regexp.MustCompile(`(?i)\b(https?|ftp):|url\(|@import`).FindString(styleCSS + scriptJS); m != "" {
		t.Errorf("inline style or script refers to %q", m)
	}
	if m := regexp.MustCompile(`\b(fetch|XMLHttpRequest|WebSocket|EventSource|sendBeacon|import)\b\s*\(?`).FindString(scriptJS); m != "" {
		t.Errorf("script can reach the network: %q", m)
	}
	if len(scriptJS) > 10*1024 {
		t.Errorf("script is %d bytes; ADR-1 budgets 10 KB", len(scriptJS))
	}
}

func selfContained(t *testing.T, page string) {
	t.Helper()
	if m := regexp.MustCompile(`(?i)<[a-z][^>]*\s(src|href|xlink:href|action|formaction|srcset|poster|background|ping)\s*=`).FindString(page); m != "" {
		t.Errorf("page references a resource: %s", m)
	}
	if m := regexp.MustCompile(`(?i)<(link|iframe|img|object|embed|base|form|a)\b`).FindString(page); m != "" {
		t.Errorf("page has a %s element", m)
	}
	if !strings.Contains(page, `<meta http-equiv="Content-Security-Policy" content="default-src &#39;none&#39;;`) {
		t.Error("page has no default-src 'none' Content-Security-Policy")
	}
}

// TestCSPHashes checks the policy allows exactly the inline style and script
// that the page carries, byte for byte.
func TestCSPHashes(t *testing.T) {
	page := renderString(t, forecast(t))
	style := regexp.MustCompile(`(?s)<style>(.*?)</style>`).FindAllStringSubmatch(page, -1)
	script := regexp.MustCompile(`(?s)<script>(.*?)</script>`).FindAllStringSubmatch(page, -1)
	if len(style) != 1 || len(script) != 1 {
		t.Fatalf("want one <style> and one <script>, got %d and %d", len(style), len(script))
	}
	csp := regexp.MustCompile(`http-equiv="Content-Security-Policy" content="([^"]*)"`).FindStringSubmatch(page)
	if csp == nil {
		t.Fatal("no Content-Security-Policy")
	}
	policy := html.UnescapeString(csp[1])
	for _, want := range []string{"style-src '" + cspHash(style[0][1]) + "'", "script-src '" + cspHash(script[0][1]) + "'"} {
		if !strings.Contains(policy, want) {
			t.Errorf("policy %q lacks %q", policy, want)
		}
	}
	if strings.Index(page, "Content-Security-Policy") > strings.Index(page, "<style>") {
		t.Error("the policy must come before the style it covers")
	}
}

func TestEmbeddedRoadmap(t *testing.T) {
	r := forecast(t)
	page := renderString(t, r)
	m := regexp.MustCompile(`(?s)<script type="application/json" id="roadmap-data">(.*?)</script>`).FindStringSubmatch(page)
	if m == nil {
		t.Fatal("no embedded roadmap")
	}
	back, err := roadmap.Read(strings.NewReader(m[1]))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(back, r) {
		t.Error("the embedded roadmap differs from the one rendered")
	}
}

func TestEscaping(t *testing.T) {
	page := renderString(t, forecast(t))
	if strings.Count(page, "<script") != 2 || strings.Contains(page, "alert(\"x\")</script>") {
		t.Error("a bead title injected markup")
	}
	if strings.Count(page, "</svg>") != strings.Count(page, "<svg") {
		t.Error("a bead title closed an <svg>")
	}
	if !strings.Contains(page, `&lt;script&gt;alert(&#34;x&#34;)&lt;/script&gt; &amp; &lt;/svg&gt;`) {
		t.Error("the hostile title is not shown as text")
	}
}

func TestPageContent(t *testing.T) {
	page := renderString(t, forecast(t))
	for _, want := range []string{
		`<title>Fixture roadmap</title>`,
		`<g class="lane" data-repo="*goals"`, `<g class="lane" data-repo="api"`, `<g class="lane" data-repo="web"`, `<g class="lane" data-repo="hq"`,
		`data-id="hq-g1" data-repos="[&#34;api&#34;,&#34;hq&#34;,&#34;web&#34;]"`, // goal rows filter by their members' repos
		`<rect class="bar s-on_track"`, `<rect class="bar s-at_risk"`, `<rect class="bar s-late"`, `<rect class="bar s-none"`,
		`class="target past"`, // hq-m9's target has passed
		`ready to close`, `not planned`, `stalled`, `waits on an unloaded repo`,
		`Plan for 2026-10-01 (80% chance) · 50/50: 2026-09-25`, `P50 2026-09-25 · P80 2026-10-01 · P95 2026-10-14`, `Target 2026-10-15: on track`,
		`Remaining 4 of 6 (33.3% done) · 1 human gate`, `Pace 3.5 beads/day · agents 3`,
		`Critical chain: 3 beads`, `Waits on unloaded: ops-7`, `Members: Public API v1; Usage dashboard`,
		`A milestone with a very long title that has…`,
		`1 on track`, `1 at risk`, `2 late`, `Track record: 80% dates held 9 of 12.`,
		`<th scope="col">Plan for (80% chance)</th><th scope="col">50/50</th>`,
		`<p>Settings: agents.api = 3 · agents.hq = 1 · human_gate.metadata = [&#34;awaiting_signoff&#34;, &#34;hold_reason&#34;].</p>`,
		`model adr-1 · seed 1 · 2000 runs · inputs 320d4943330f`,
		`done 2026-08-20`, // web-e2 in the table, not the timeline
	} {
		if !strings.Contains(page, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	if strings.Contains(page, `<g class="row" data-id="web-e2"`) {
		t.Error("a done epic is on the timeline")
	}
	if strings.Contains(page, "No forecast in this roadmap yet") || strings.Contains(page, "could not be read") {
		t.Error("forecast roadmap shows a banner")
	}
	if strings.Contains(strings.ToLower(page), "almost surely") {
		t.Error(`the page says "almost surely"`)
	}

	plain := renderString(t, built(t))
	for _, want := range []string{"No forecast in this roadmap yet", "no forecast yet", "0 on track", "4 without a target or forecast"} {
		if !strings.Contains(plain, want) {
			t.Errorf("roadmap without a forecast lacks %q", want)
		}
	}
	if strings.Contains(timelineSVG(t, plain), `class="bar`) {
		t.Error("roadmap without a forecast draws bars")
	}
}

func TestFailedRepo(t *testing.T) {
	r := built(t)
	// A repo that could not be read has no beads.
	r.Repos[1].Error = "repo web: bd export: exit status 1: database locked"
	var kept []roadmap.Milestone
	for _, m := range r.Milestones {
		if m.Repo != "web" {
			kept = append(kept, m)
		}
	}
	r.Milestones = kept
	r.Config.Settings = nil
	page := renderString(t, r)
	for _, want := range []string{
		`<div class="banner error" role="alert"><p>Some repos could not be read, so this page shows the others only:</p><ul><li>repo web: bd export: exit status 1: database locked</li></ul></div>`,
		`>could not be read</text>`, `could not be read: see the note at the top`,
		`<p>Default settings.</p>`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("page lacks %q", want)
		}
	}
}

func timelineSVG(t *testing.T, page string) string {
	t.Helper()
	m := regexp.MustCompile(`(?s)<svg class="tl".*?</svg>`).FindString(page)
	if m == "" {
		t.Fatal("no timeline")
	}
	return m
}

func TestEmptyRoadmap(t *testing.T) {
	r := &roadmap.Roadmap{SchemaVersion: roadmap.SchemaVersion, GeneratedAt: now,
		Repos: []roadmap.Repo{{Name: "solo", ConcurrencySource: "measured"}}}
	page := renderString(t, r)
	for _, want := range []string{"no open goals", "no open milestones or epics", "No goals.", "No open or recently closed milestones or epics.", "No targets (due_at) set"} {
		if !strings.Contains(page, want) {
			t.Errorf("empty roadmap lacks %q", want)
		}
	}
	if err := Render(&bytes.Buffer{}, &roadmap.Roadmap{}, Options{}); err == nil {
		t.Error("Render accepted an invalid roadmap")
	}
}

func TestTickTimes(t *testing.T) {
	start := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	for _, tt := range []struct {
		days        float64
		first, last string
	}{
		{7, "Sep 20", "Sep 26"},
		{20, "Sep 21", "Oct 9"},  // every other day
		{60, "Sep 21", "Nov 16"}, // Mondays
		{120, "Sep 21", "Jan 11"},
		{300, "Oct 2026", "Jul"},
		{700, "Oct 2026", "Jul"},
		{2000, "2027", "2032"},
	} {
		ticks := tickTimes(start, start.Add(time.Duration(tt.days*24)*time.Hour))
		if len(ticks) < 3 || len(ticks) > 14 {
			t.Errorf("%v days: %d ticks", tt.days, len(ticks))
			continue
		}
		if first, last := ticks[0].label, ticks[len(ticks)-1].label; first != tt.first || last != tt.last {
			t.Errorf("%v days: ticks %q .. %q, want %q .. %q", tt.days, first, last, tt.first, tt.last)
		}
	}
}

func TestCut(t *testing.T) {
	for in, want := range map[string]string{
		"short":             "short",
		"exactly ten":       "exactly t…",
		"trailing space  x": "trailing…",
		"ünïcödé ✓ ✓ ✓ ✓ ✓": "ünïcödé ✓…",
	} {
		if got := cut(in, 10); got != want {
			t.Errorf("cut(%q, 10) = %q, want %q", in, got, want)
		}
	}
}
