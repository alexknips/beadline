package render

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"regexp"
	"testing"
	"time"

	"github.com/alexknips/beadline/internal/config"
	"github.com/alexknips/beadline/internal/load"
	"github.com/alexknips/beadline/internal/roadmap"
)

// TestRealExports renders real exports: set BEADLINE_REAL_CONFIG to a
// beadline.toml. It checks that every open high-level bead and goal gets a
// row, that nothing else does, and that the page stays self-contained. With
// BEADLINE_REAL_OUT set to a directory it also writes roadmap.json and
// roadmap.html there, for a look in a browser.
func TestRealExports(t *testing.T) {
	path := os.Getenv("BEADLINE_REAL_CONFIG")
	if path == "" {
		t.Skip("BEADLINE_REAL_CONFIG is not set")
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	exports := load.ReadAll(context.Background(), cfg, load.BdExporter("bd"))
	for _, e := range exports {
		if e.Err != nil {
			t.Fatal(e.Err)
		}
	}
	g, rep, err := load.FromExports(&cfg.Conventions, exports)
	if err != nil {
		t.Fatal(err)
	}
	r := roadmap.Build(g, rep, cfg, time.Now())
	r.BeadlineVersion = "dev"
	r.SetInputs(exports)
	var js bytes.Buffer
	if err := roadmap.Write(&js, r); err != nil {
		t.Fatal(err)
	}
	if _, err := roadmap.Read(bytes.NewReader(js.Bytes())); err != nil {
		t.Fatal(err)
	}
	var page bytes.Buffer
	if err := Render(&page, r, Options{Title: "Roadmap"}); err != nil {
		t.Fatal(err)
	}
	html := page.String()
	selfContained(t, html)

	want := map[string]bool{}
	for _, m := range r.Milestones {
		if m.Status != roadmap.StatusDone {
			want[m.ID] = true
		}
	}
	for _, gl := range r.Goals {
		if gl.Status != roadmap.StatusDone {
			want[gl.ID] = true
		}
	}
	goals := map[string]bool{}
	for _, gl := range g.Goals() {
		goals[gl.ID] = true
	}
	for _, i := range g.HighLevel() {
		if !i.Closed() && !goals[i.ID] && !want[i.ID] {
			t.Errorf("open high-level bead %s is not on the roadmap", i.ID)
		}
	}
	rows := map[string]bool{}
	for _, m := range regexp.MustCompile(`<g class="row" data-id="([^"]+)"`).FindAllStringSubmatch(html, -1) {
		rows[m[1]] = true
		if !want[m[1]] {
			t.Errorf("timeline row %s is not an open milestone or goal", m[1])
		}
	}
	for id := range want {
		if !rows[id] {
			t.Errorf("%s has no timeline row", id)
		}
	}
	t.Logf("%d repos, %d open rows, %d milestones and %d goals in the JSON; page %d bytes, roadmap.json %d bytes",
		len(r.Repos), len(rows), len(r.Milestones), len(r.Goals), page.Len(), js.Len())

	if out := os.Getenv("BEADLINE_REAL_OUT"); out != "" {
		if err := roadmap.WriteFile(filepath.Join(out, "roadmap.json"), r); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(out, "roadmap.html"), page.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}
