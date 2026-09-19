package estimate

import (
	"context"
	"errors"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestUpdateArgs(t *testing.T) {
	got := Update{Repo: "api", ID: "api-12", Minutes: 31, P80Minutes: 47}.Args()
	want := []string{"update", "api-12", "--estimate", "31", "--set-metadata", "ai_est_p80_minutes=47"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Args = %q, want %q", got, want)
	}
}

func TestPlanWriteBack(t *testing.T) {
	m := learn(t, twoRepos())
	api := m.Estimate(Bead{Repo: "api", Type: "task"}).Cycle
	web := m.Estimate(Bead{Repo: "web", Type: "task"}).Cycle
	p50, p80 := int(math.Ceil(api.P50)), int(math.Ceil(api.P80))

	beads := []Bead{
		{ID: "new", Repo: "api", Type: "task", Status: "open"},
		{ID: "closed", Repo: "api", Type: "task", Status: "closed", ClosedAt: now},
		{ID: "unchanged", Repo: "api", Type: "task", Status: "open", EstimatedMinutes: p50, P80Minutes: p80},
		{ID: "stale-p80", Repo: "api", Type: "task", Status: "open", EstimatedMinutes: p50, P80Minutes: p80 + 5},
		// A started bead records its full cycle time, not the time remaining.
		{ID: "started", Repo: "web", Type: "task", Status: StatusInProgress, StartedAt: now.Add(-3 * time.Hour)},
	}
	got := m.PlanWriteBack(beads)
	want := []Update{
		{Repo: "api", ID: "new", Minutes: p50, P80Minutes: p80},
		{Repo: "api", ID: "stale-p80", Minutes: p50, P80Minutes: p80},
		{Repo: "web", ID: "started", Minutes: int(math.Ceil(web.P50)), P80Minutes: int(math.Ceil(web.P80))},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("PlanWriteBack =\n%+v\nwant\n%+v", got, want)
	}

	// Applying the plan and planning again writes nothing.
	for i := range beads {
		for _, u := range got {
			if beads[i].ID == u.ID {
				beads[i].EstimatedMinutes, beads[i].P80Minutes = u.Minutes, u.P80Minutes
			}
		}
	}
	if again := m.PlanWriteBack(beads); len(again) != 0 {
		t.Errorf("second plan = %+v, want nothing to write", again)
	}
}

func TestWholeMinutes(t *testing.T) {
	for _, tt := range []struct {
		in   float64
		want int
	}{{0.2, 1}, {1, 1}, {1.01, 2}, {59.5, 60}} {
		if got := wholeMinutes(tt.in); got != tt.want {
			t.Errorf("wholeMinutes(%v) = %d, want %d", tt.in, got, tt.want)
		}
	}
}

type call struct {
	repo string
	args []string
}

func TestWriteBackContinuesPastFailures(t *testing.T) {
	updates := []Update{{Repo: "api", ID: "a", Minutes: 1, P80Minutes: 2}, {Repo: "web", ID: "b", Minutes: 3, P80Minutes: 4}, {Repo: "api", ID: "c", Minutes: 5, P80Minutes: 6}}
	var calls []call
	run := func(_ context.Context, repo string, args []string) error {
		calls = append(calls, call{repo, args})
		if args[1] == "b" {
			return errors.New("database locked")
		}
		return nil
	}
	err := WriteBack(context.Background(), updates, run)
	if err == nil || !strings.Contains(err.Error(), "write-back b: database locked") {
		t.Errorf("error = %v, want the failure of b", err)
	}
	if len(calls) != 3 {
		t.Fatalf("ran %d updates, want all 3", len(calls))
	}
	for i, u := range updates {
		if calls[i].repo != u.Repo || !reflect.DeepEqual(calls[i].args, u.Args()) {
			t.Errorf("call %d = %+v, want %s %q", i, calls[i], u.Repo, u.Args())
		}
	}
}

func TestWriteBackStopsWhenCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	n := 0
	run := func(context.Context, string, []string) error {
		n++
		cancel()
		return nil
	}
	err := WriteBack(ctx, []Update{{ID: "a"}, {ID: "b"}}, run)
	if !errors.Is(err, context.Canceled) || n != 1 {
		t.Errorf("err = %v after %d updates, want context.Canceled after 1", err, n)
	}
}

func TestBdRunner(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses a shell script as a stand-in for bd")
	}
	tmp := t.TempDir()
	log := filepath.Join(tmp, "calls.log")
	bd := filepath.Join(tmp, "bd")
	script := "#!/bin/sh\necho \"$(basename \"$PWD\") $*\" >> " + log + "\n[ \"$2\" = fail ] && { echo 'no such issue' >&2; exit 1; }\nexit 0\n"
	if err := os.WriteFile(bd, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	dirs := map[string]string{}
	for _, repo := range []string{"api", "web"} {
		dirs[repo] = filepath.Join(tmp, repo)
		if err := os.Mkdir(dirs[repo], 0o755); err != nil {
			t.Fatal(err)
		}
	}
	run := BdRunner(bd, dirs)

	err := WriteBack(context.Background(), []Update{
		{Repo: "api", ID: "api-1", Minutes: 30, P80Minutes: 46},
		{Repo: "web", ID: "fail", Minutes: 1, P80Minutes: 2},
		{Repo: "cli", ID: "cli-1", Minutes: 1, P80Minutes: 2},
	}, run)
	for _, want := range []string{"no such issue", `no directory for repository "cli"`} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("error = %v, want it to mention %s", err, want)
		}
	}
	data, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	want := "api update api-1 --estimate 30 --set-metadata ai_est_p80_minutes=46\n" +
		"web update fail --estimate 1 --set-metadata ai_est_p80_minutes=2\n"
	if string(data) != want {
		t.Errorf("bd calls:\n%s\nwant\n%s", data, want)
	}
}
