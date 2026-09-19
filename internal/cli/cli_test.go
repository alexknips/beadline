package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alexknips/beadline/internal/config"
	"github.com/alexknips/beadline/internal/load"
	"github.com/alexknips/beadline/internal/roadmap"
)

func run(args ...string) (code int, stdout, stderr string) {
	var out, errOut bytes.Buffer
	code = Run(args, &out, &errOut)
	return code, out.String(), errOut.String()
}

func TestRun(t *testing.T) {
	multirepo := "../../testdata/multirepo/beadline.toml"
	tests := []struct {
		name       string
		args       []string
		wantCode   int
		wantStdout string
		wantStderr string
	}{
		{"help", []string{"help"}, ExitOK, "beadline [PATH...] [flags]", ""},
		{"-h", []string{"-h"}, ExitOK, "Expert flags:", ""},
		{"--help", []string{"--help"}, ExitOK, "Expert flags:", ""},
		{"help check", []string{"help", "check"}, ExitOK, "Usage: beadline check [PATH...]", ""},
		{"help doctor", []string{"help", "doctor"}, ExitOK, "Usage: beadline doctor [PATH...]", ""},
		{"help unknown", []string{"help", "chek"}, ExitUsage, "", `unknown command "chek"; did you mean 'beadline check'?`},
		{"help stray", []string{"help", "check", "doctor"}, ExitUsage, "", "takes one command"},
		{"version", []string{"version"}, ExitOK, "beadline ", ""},
		{"--version", []string{"--version"}, ExitOK, "beadline ", ""},
		{"-v", []string{"-v"}, ExitOK, "beadline ", ""},
		{"version -h", []string{"version", "-h"}, ExitOK, "Usage: beadline version", ""},
		{"version stray", []string{"version", "--json"}, ExitUsage, "", "flag provided but not defined: -json"},
		{"version extra", []string{"version", "extra"}, ExitUsage, "", `beadline version: unexpected argument "extra"`},
		{"mistyped command", []string{"chek"}, ExitUsage, "", "chek: not a command, repository or file; did you mean 'beadline check'?"},
		{"unknown word", []string{"frobnicate"}, ExitUsage, "", "frobnicate: not a command, repository or file\nRun 'beadline help' for usage."},
		{"missing path", []string{"./nope/x.jsonl"}, ExitUsage, "", "./nope/x.jsonl: no such repository or file"},
		{"not a repo", []string{"../../testdata"}, ExitUsage, "", "../../testdata has no .beads directory: not a beads repository"},
		{"unknown flag", []string{"--frobnicate"}, ExitUsage, "", "flag provided but not defined: -frobnicate"},
		{"no runs", []string{"--runs", "0", "../../testdata/problems/a.jsonl"}, ExitUsage, "", "--runs must be at least 1"},
		{"bad as-of", []string{"--as-of", "yesterday"}, ExitUsage, "", `--as-of: "yesterday" is not a date`},
		{"bad agents", []string{"--agents", "api"}, ExitUsage, "", `want REPO=N, got "api"`},
		{"json and explain", []string{"--json", "--explain", "x"}, ExitUsage, "", "pick one"},
		{"missing config", []string{"-c", "does-not-exist.toml"}, ExitFailure, "", "beadline: open does-not-exist.toml"},
		{"doctor clean fixture", []string{"doctor", "--config", multirepo}, ExitOK, "15 issues, 4 high-level, 2 goals, 3 human gates\n", ""},
		{"doctor problems fixture", []string{"doctor", "-c", "../../testdata/problems/beadline.toml"}, ExitFailure, "1 error, 4 warnings", "error: blocking cycle among open beads: a-1, a-2\n"},
		{"doctor paths", []string{"doctor", "../../testdata/multirepo/api.jsonl"}, ExitOK, "repo api: 7 issues from ../../testdata/multirepo/api.jsonl", ""},
		{"doctor missing config", []string{"doctor", "--config", "does-not-exist.toml"}, ExitFailure, "", "beadline doctor: open does-not-exist.toml"},
		{"doctor bad flag", []string{"doctor", "--frobnicate"}, ExitUsage, "", "flag provided but not defined"},
		{"doctor missing path", []string{"doctor", "extra"}, ExitUsage, "", "beadline doctor: extra: not a command, repository or file"},
		{"doctor -h", []string{"doctor", "-h"}, ExitOK, "--config FILE", ""},
		{"check -h", []string{"check", "-h"}, ExitOK, "--backtest SPAN", ""},
		{"check --help after a path", []string{"check", "../../testdata/problems/a.jsonl", "--help"}, ExitOK, "--backtest SPAN", ""},
		{"check bad span", []string{"check", "--backtest", "soon"}, ExitUsage, "", `--backtest: "soon" is not a positive number`},
		{"check bad flag", []string{"check", "--frobnicate"}, ExitUsage, "", "flag provided but not defined"},
		{"forecast missing config", []string{"forecast", "--config", "does-not-exist.toml"}, ExitFailure, "", "beadline forecast: open does-not-exist.toml"},
		{"forecast bad now", []string{"forecast", "--now", "yesterday"}, ExitUsage, "", "-now: parsing time"},
		{"forecast no runs", []string{"forecast", "--config", multirepo, "--runs", "0"}, ExitUsage, "", "-runs must be at least 1"},
		{"forecast extra argument", []string{"forecast", "extra"}, ExitUsage, "", `unexpected argument "extra"`},
		{"forecast -h", []string{"forecast", "-h"}, ExitOK, "--write-back", ""},
		{"render without roadmap.json", []string{"render"}, ExitFailure, "", "beadline render: open roadmap.json"},
		{"render bad flag", []string{"render", "--frobnicate"}, ExitUsage, "", "flag provided but not defined"},
		{"render extra argument", []string{"render", "extra"}, ExitUsage, "", `unexpected argument "extra"`},
		{"render -h", []string{"render", "-h"}, ExitOK, "--title TEXT", ""},
		{"serve is gone", []string{"serve"}, ExitUsage, "", "serve: not a command, repository or file"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, stdout, stderr := run(tt.args...)
			if code != tt.wantCode {
				t.Errorf("exit code = %d, want %d", code, tt.wantCode)
			}
			if !strings.Contains(stdout, tt.wantStdout) || (tt.wantStdout == "" && stdout != "") {
				t.Errorf("stdout = %q, want it to contain %q", stdout, tt.wantStdout)
			}
			if !strings.Contains(stderr, tt.wantStderr) || (tt.wantStderr == "" && stderr != "") {
				t.Errorf("stderr = %q, want it to contain %q", stderr, tt.wantStderr)
			}
		})
	}
}

func TestUsageListsEveryCommand(t *testing.T) {
	_, stdout, _ := run("help")
	for _, c := range commands() {
		listed := strings.Contains(stdout, "  beadline "+c.name+" ")
		if listed == c.hidden {
			t.Errorf("command %q: listed %v, hidden %v:\n%s", c.name, listed, c.hidden, stdout)
		}
		// Every command, hidden or not, has help that -h prints.
		if code, out, _ := run(c.name, "-h"); code != ExitOK || !strings.HasPrefix(out, "Usage: beadline "+c.name) {
			t.Errorf("%s -h: exit %d, stdout %q", c.name, code, out)
		}
	}
	for _, gone := range []string{"serve", "almost surely"} {
		if strings.Contains(stdout, gone) {
			t.Errorf("usage mentions %q", gone)
		}
	}
}

func TestVersion(t *testing.T) {
	saved := Version
	t.Cleanup(func() { Version = saved })

	Version = "v1.2.3"
	for _, args := range [][]string{{"version"}, {"--version"}, {"-v"}} {
		if _, stdout, _ := run(args...); stdout != "beadline v1.2.3\n" {
			t.Errorf("%v with linker version: stdout = %q", args, stdout)
		}
	}

	Version = ""
	if got := version(); got == "" {
		t.Error("version() is empty without a linker version; want a module version, a revision or \"dev\"")
	}
}

func TestDoctorOutput(t *testing.T) {
	code, stdout, stderr := run("doctor", "--config", "../../testdata/problems/beadline.toml")
	if code != ExitFailure {
		t.Errorf("exit code = %d", code)
	}
	wantStdout := "repo a: 5 issues from ../../testdata/problems/a.jsonl\n" +
		"repo b: 1 issue from ../../testdata/problems/b.jsonl\n" +
		"6 issues, 0 high-level, 0 goals, 0 human gates\n" +
		"1 error, 4 warnings\n"
	if stdout != wantStdout {
		t.Errorf("stdout =\n%s\nwant\n%s", stdout, wantStdout)
	}
	wantStderr := "error: blocking cycle among open beads: a-1, a-2\n" +
		"warning: blocking cycle among closed beads: a-3, a-4\n" +
		"warning: a-5 (repo a): blocks dependency a-99 is not in any loaded repo\n" +
		"warning: a-5 (repo a): parent-child dependency gone-epic is not in any loaded repo\n" +
		"warning: a-5 at ../../testdata/problems/b.jsonl:2 (repo b) duplicates the issue loaded from repo a; ignored\n"
	if stderr != wantStderr {
		t.Errorf("stderr =\n%s\nwant\n%s", stderr, wantStderr)
	}

	_, stdout, _ = run("doctor", "--config", "../../testdata/multirepo/beadline.toml")
	if !strings.Contains(stdout, "repo api: 7 issues from ../../testdata/multirepo/api.jsonl (skipped 1 ephemeral, 1 molecule, 1 template)\n") ||
		!strings.Contains(stdout, "(skipped 1 convoy, 1 non-issue)\n") {
		t.Errorf("skipped records not reported:\n%s", stdout)
	}
}

func TestRender(t *testing.T) {
	dir := t.TempDir()
	cfg, err := config.Load(filepath.Join("..", "..", "testdata", "multirepo", "beadline.toml"))
	if err != nil {
		t.Fatal(err)
	}
	g, rep, err := load.Load(cfg)
	if err != nil {
		t.Fatal(err)
	}
	in := filepath.Join(dir, "roadmap.json")
	if err := roadmap.WriteFile(in, roadmap.Build(g, rep, cfg, time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC))); err != nil {
		t.Fatal(err)
	}

	out := filepath.Join(dir, "site", "roadmap.html")
	if err := os.Mkdir(filepath.Dir(out), 0o755); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := run("render", "--in", in, "--out", out, "--title", "Town roadmap")
	if code != ExitOK || stdout != "" || stderr != "" {
		t.Fatalf("render: code %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	page, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(page), "<!DOCTYPE html>") || !strings.Contains(string(page), "<title>Town roadmap</title>") {
		t.Errorf("unexpected page:\n%.300s", page)
	}
	if entries, _ := os.ReadDir(filepath.Dir(out)); len(entries) != 1 {
		t.Errorf("render left temporary files: %v", entries)
	}

	code, stdout, _ = run("render", "--in", in, "--out", "-")
	if code != ExitOK || stdout != strings.Replace(string(page), "Town roadmap", "Roadmap", 2) {
		t.Errorf("render --out -: code %d, stdout differs from the file", code)
	}

	bad := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(bad, []byte(`{"schema_version": 99}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, _, stderr := run("render", "--in", bad, "--out", out); code != ExitFailure || !strings.Contains(stderr, "newer than this beadline reads") {
		t.Errorf("render of a newer schema: code %d, stderr %q", code, stderr)
	}
	if code, _, stderr := run("render", "--in", in, "--out", filepath.Join(dir, "missing", "x.html")); code != ExitFailure || stderr == "" {
		t.Errorf("render into a missing directory: code %d, stderr %q", code, stderr)
	}
}
