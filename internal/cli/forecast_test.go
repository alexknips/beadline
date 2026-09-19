package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/alexknips/beadline/internal/estimate"
	"github.com/alexknips/beadline/internal/forecast"
)

var fixtureForecast = []string{"forecast", "--config", "../../testdata/multirepo/beadline.toml", "--now", "2026-09-03T12:00:00Z", "--runs", "300"}

func TestForecastTable(t *testing.T) {
	code, stdout, stderr := run(fixtureForecast...)
	if code != ExitOK || stderr != "" {
		t.Fatalf("exit %d, stderr %q", code, stderr)
	}
	for _, want := range []string{
		"forecast as of 2026-09-03 12:00 UTC, 300 runs, seed 1\n",
		"agents: api 3, hq 1, web no limit (peak 1); human-gate lag from 0 closed gates\n",
		"learned from 3 delivered closes (2 with a start) and 1 open bead\n",
		"ITEM", "CRITICAL CHAIN (P80)",
		"\napi-m1 ", "\napi-e1 ", "\nweb-e1 ", "\ngoal hq-g1 ",
		"api-3 > api-4 > api-5",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("table does not contain %q:\n%s", want, stdout)
		}
	}
}

func TestForecastJSON(t *testing.T) {
	code, stdout, stderr := run(append(fixtureForecast, "--json")...)
	if code != ExitOK || stderr != "" {
		t.Fatalf("exit %d, stderr %q", code, stderr)
	}
	var res forecast.Result
	if err := json.Unmarshal([]byte(stdout), &res); err != nil {
		t.Fatalf("output is not a forecast: %v\n%s", err, stdout)
	}
	if len(res.Items) != 3 || len(res.Goals) != 1 || res.Runs != 300 || res.Seed != 1 {
		t.Fatalf("result = %+v", res)
	}
	m1 := res.Items[0]
	if m1.ID != "api-m1" || m1.Status != forecast.StatusForecast || m1.P50 == nil || m1.P95.At.Before(m1.P50.At) {
		t.Errorf("api-m1 = %+v", m1)
	}
	// The human review api-5 is on the chain, so part of the time is human.
	if m1.P80.HumanHours <= 0 || m1.P80.AgentHours <= 0 {
		t.Errorf("api-m1 P80 split = %+v", m1.P80)
	}
	if !reflect.DeepEqual(m1.Remaining, []string{"api-3", "api-4", "api-5", "api-e1"}) {
		t.Errorf("api-m1 remaining = %v", m1.Remaining)
	}

	// Same inputs, same seed: same forecast. Another seed: another one.
	if _, again, _ := run(append(fixtureForecast, "--json")...); again != stdout {
		t.Error("the same seed gave a different forecast")
	}
	if _, other, _ := run(append(fixtureForecast, "--json", "--seed", "2")...); other == stdout {
		t.Error("another seed gave the same forecast")
	}
}

func TestForecastWriteBack(t *testing.T) {
	tmp := t.TempDir()
	repo := filepath.Join(tmp, "api")
	if err := os.MkdirAll(filepath.Join(repo, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile("../../testdata/multirepo/api.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".beads", "issues.jsonl"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(tmp, "beadline.toml")
	if err := os.WriteFile(cfgPath, []byte(`repos = ["api/.beads/issues.jsonl"]`), 0o644); err != nil {
		t.Fatal(err)
	}

	var calls []string
	saved := newBdRunner
	t.Cleanup(func() { newBdRunner = saved })
	newBdRunner = func(bd string, dirs map[string]string) estimate.Runner {
		return func(_ context.Context, repo string, args []string) error {
			calls = append(calls, bd+" in "+dirs[repo]+": "+strings.Join(args[:2], " "))
			return nil
		}
	}
	code, _, stderr := run("forecast", "--config", cfgPath, "--now", "2026-09-03T12:00:00Z", "--runs", "10", "--write-back", "--bd", "/opt/bd")
	if code != ExitOK {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	// Open work beads only: api-5 is a human gate, the epics are not work.
	want := []string{"/opt/bd in " + repo + ": update api-3", "/opt/bd in " + repo + ": update api-4"}
	if !reflect.DeepEqual(calls, want) || stderr != "wrote estimates to 2 beads\n" {
		t.Errorf("calls = %v, stderr %q; want %v", calls, stderr, want)
	}

	calls = nil
	code, _, stderr = run(append(fixtureForecast, "--write-back")...)
	if code != ExitFailure || !strings.Contains(stderr, "is not in a .beads directory") || calls != nil {
		t.Errorf("export outside .beads: exit %d, stderr %q, calls %v", code, stderr, calls)
	}
}
