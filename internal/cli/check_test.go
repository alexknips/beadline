package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alexknips/beadline/internal/calibrate"
	"github.com/alexknips/beadline/internal/config"
)

const multirepo = "../../testdata/multirepo/beadline.toml"

func TestCheckSnapshots(t *testing.T) {
	snaps := filepath.Join(t.TempDir(), "snapshots")
	code, stdout, stderr := run("check", "--config", multirepo, "--snapshots", snaps)
	if code != ExitOK || stderr != "" || !strings.Contains(stdout, "no snapshots in "+snaps) {
		t.Fatalf("no snapshots: code %d, stdout %q, stderr %q", code, stdout, stderr)
	}

	record := []string{"forecast", "--config", multirepo, "--runs", "50", "--record", "--snapshots", snaps}
	code, stdout, stderr = run(record...)
	if code != ExitOK || !strings.HasPrefix(stderr, "recorded snapshot "+snaps+string(filepath.Separator)) || !strings.Contains(stdout, "ITEM") {
		t.Fatalf("forecast --record: code %d, stderr %q", code, stderr)
	}
	if code, _, stderr = run(record...); code != ExitOK || !strings.HasPrefix(stderr, "kept today's snapshot ") {
		t.Errorf("second forecast --record: code %d, stderr %q", code, stderr)
	}

	code, stdout, stderr = run("check", "--config", multirepo, "--snapshots", snaps)
	if code != ExitOK || stderr != "" {
		t.Fatalf("check: code %d, stderr %q", code, stderr)
	}
	for _, want := range []string{"1 snapshot graded against data as of 2026-09-03", "LEAF BEADS (the honest sample)", "HIGH-LEVEL"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("check output does not contain %q:\n%s", want, stdout)
		}
	}
	if strings.Contains(stdout, "release gate") {
		t.Errorf("snapshot grading printed the release gate:\n%s", stdout)
	}

	code, stdout, _ = run("check", "--config", multirepo, "--snapshots", snaps, "--json")
	var rep calibrate.Report
	if err := json.Unmarshal([]byte(stdout), &rep); code != ExitOK || err != nil {
		t.Fatalf("check --json: code %d, %v\n%s", code, err, stdout)
	}
	// The snapshot is newer than the fixture's data: every forecast is
	// still pending.
	if rep.Mode != "snapshots" || len(rep.Snapshots) != 1 || len(rep.Pairs) == 0 || rep.Leaf.Model.Open == 0 || rep.Gate != nil {
		t.Errorf("report = %+v", rep)
	}

	// A changed snapshot is skipped with a warning.
	entries, _ := os.ReadDir(snaps)
	path := filepath.Join(snaps, entries[0].Name())
	data, _ := os.ReadFile(path)
	os.Chmod(path, 0o644)
	os.WriteFile(path, []byte(strings.Replace(string(data), `"runs": 50`, `"runs": 51`, 1)), 0o644)
	code, stdout, stderr = run("check", "--config", multirepo, "--snapshots", snaps)
	if code != ExitOK || !strings.Contains(stderr, "warning: skipped "+path+": content hash does not match") || !strings.Contains(stdout, "no snapshots") {
		t.Errorf("tampered snapshot: code %d, stdout %q, stderr %q", code, stdout, stderr)
	}
}

// writeHistory writes an export with forty days of history: task k is
// created on day k and closes two days later, and a third of the tasks
// belong to one epic. It returns the path of its beadline.toml.
func writeHistory(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	day0 := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	stamp := func(d float64) string { return day0.Add(time.Duration(d * float64(24*time.Hour))).Format(time.RFC3339) }
	var lines []string
	lines = append(lines, fmt.Sprintf(`{"id":"r-e","title":"Epic","issue_type":"epic","status":"closed","created_at":%q,"closed_at":%q,"updated_at":%q}`,
		stamp(0), stamp(42), stamp(42)))
	for k := 0; k < 40; k++ {
		deps := ""
		if k%3 == 0 {
			deps = fmt.Sprintf(`,"dependencies":[{"depends_on_id":"r-e","type":"parent-child","created_at":%q}]`, stamp(float64(k)))
		}
		lines = append(lines, fmt.Sprintf(`{"id":"r-%d","title":"Task %d","issue_type":"task","status":"closed","close_reason":"done","created_at":%q,"started_at":%q,"closed_at":%q,"updated_at":%q%s}`,
			k, k, stamp(float64(k)), stamp(float64(k)+0.1), stamp(float64(k)+2), stamp(float64(k)+2), deps))
	}
	if err := os.WriteFile(filepath.Join(dir, "r.jsonl"), []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(dir, "beadline.toml")
	if err := os.WriteFile(cfg, []byte("[[repos]]\nname = \"r\"\nexport = \"r.jsonl\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestCheckBacktest(t *testing.T) {
	cfg := writeHistory(t)
	args := []string{"check", "--config", cfg, "--backtest", "3w", "--step", "2d", "--runs", "100"}
	code, stdout, stderr := run(args...)
	if code != ExitOK || stderr != "" {
		t.Fatalf("check --backtest: code %d, stderr %q\n%s", code, stderr, stdout)
	}
	for _, want := range []string{
		"backtest: 8 origins from 2026-08-22 00:00 to 2026-09-05 00:00, 100 runs each; outcomes known to 2026-09-12 00:00 UTC\n",
		"each forecast used only the data visible at its origin",
		"model     P80", "baseline  P80", "model error: bias 0.0 d, median",
		"release gate (leaf P80 held 70%-90%): FAIL",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("backtest output does not contain %q:\n%s", want, stdout)
		}
	}

	code, stdout, _ = run(append(args, "--json")...)
	var rep calibrate.Report
	if err := json.Unmarshal([]byte(stdout), &rep); code != ExitOK || err != nil {
		t.Fatalf("check --backtest --json: code %d, %v", code, err)
	}
	if rep.Mode != "backtest" || len(rep.Origins) != 8 || rep.Runs != 100 || rep.Gate == nil || rep.Leaf.Model.Resolved == 0 || rep.Leaf.Baseline == nil {
		t.Errorf("report: mode %s, %d origins, %d runs, gate %+v, leaf %+v", rep.Mode, len(rep.Origins), rep.Runs, rep.Gate, rep.Leaf.Model)
	}
}

func TestCheckUsage(t *testing.T) {
	for _, tt := range []struct {
		args []string
		want string
	}{
		{[]string{"check", "--backtest", "soon"}, `-backtest: "soon" is not a positive number`},
		{[]string{"check", "--backtest", "60d", "--step", "0d"}, `-step: "0d" is not a positive number`},
		{[]string{"check", "--config", multirepo, "--runs", "0"}, "-runs must be at least 1"},
		{[]string{"check", "extra"}, `unexpected argument "extra"`},
		{[]string{"check", "--frobnicate"}, "flag provided but not defined"},
		{[]string{"forecast", "--config", multirepo, "--record", "--now", "2026-09-03T12:00:00Z"}, "-record takes no -now"},
	} {
		if code, _, stderr := run(tt.args...); code != ExitUsage || !strings.Contains(stderr, tt.want) {
			t.Errorf("%v: code %d, stderr %q, want usage error with %q", tt.args, code, stderr, tt.want)
		}
	}
	if code, _, stderr := run("check", "-h"); code != ExitOK || !strings.Contains(stderr, "-backtest") {
		t.Errorf("check -h: code %d, stderr %q", code, stderr)
	}
	if code, _, stderr := run("check", "--config", multirepo, "--backtest", "3d"); code != ExitFailure || !strings.Contains(stderr, "no backtest origin") {
		t.Errorf("span inside the last week: code %d, stderr %q", code, stderr)
	}
}

func TestParseSpan(t *testing.T) {
	for in, want := range map[string]time.Duration{"36h": 36 * time.Hour, "3.5d": 84 * time.Hour, "2w": 14 * 24 * time.Hour, "60d": 60 * 24 * time.Hour} {
		if got, err := parseSpan(in); err != nil || got != want {
			t.Errorf("parseSpan(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, in := range []string{"", "d", "-1d", "0w", "5", "5x", "1e9d", "NaNd"} {
		if _, err := parseSpan(in); err == nil {
			t.Errorf("parseSpan(%q) accepted", in)
		}
	}
	if got := snapshotDir(&config.Config{Dir: "/srv/town"}, ""); got != "/srv/town/.beadline/snapshots" {
		t.Errorf("default snapshot dir = %s", got)
	}
	if got := snapshotDir(&config.Config{Dir: "/srv/town"}, "snaps"); got != "snaps" {
		t.Errorf("snapshot dir flag = %s", got)
	}
}
