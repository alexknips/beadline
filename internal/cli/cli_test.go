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
	tests := []struct {
		name       string
		args       []string
		wantCode   int
		wantStdout string
		wantStderr string
	}{
		{"no args prints usage to stderr", nil, ExitUsage, "", "Usage:"},
		{"help", []string{"help"}, ExitOK, "Commands:", ""},
		{"-h", []string{"-h"}, ExitOK, "Commands:", ""},
		{"--help", []string{"--help"}, ExitOK, "Commands:", ""},
		{"unknown command", []string{"frobnicate"}, ExitUsage, "", `unknown command "frobnicate"`},
		{"version", []string{"version"}, ExitOK, "beadline ", ""},
		{"check clean fixture", []string{"check", "--config", "../../testdata/multirepo/beadline.toml"}, ExitOK, "15 issues, 4 high-level, 2 goals, 3 human gates\n", ""},
		{"check problems fixture", []string{"check", "-config", "../../testdata/problems/beadline.toml"}, ExitFailure, "1 error, 4 warnings", "error: blocking cycle among open beads: a-1, a-2\n"},
		{"check missing config", []string{"check", "--config", "does-not-exist.toml"}, ExitFailure, "", "beadline check: open does-not-exist.toml"},
		{"check bad flag", []string{"check", "--frobnicate"}, ExitUsage, "", "flag provided but not defined"},
		{"check extra argument", []string{"check", "extra"}, ExitUsage, "", `unexpected argument "extra"`},
		{"check -h", []string{"check", "-h"}, ExitOK, "", "-config"},
		{"forecast missing config", []string{"forecast", "--config", "does-not-exist.toml"}, ExitFailure, "", "beadline forecast: open does-not-exist.toml"},
		{"forecast bad now", []string{"forecast", "--now", "yesterday"}, ExitUsage, "", "-now: parsing time"},
		{"forecast no runs", []string{"forecast", "--config", "../../testdata/multirepo/beadline.toml", "--runs", "0"}, ExitUsage, "", "-runs must be at least 1"},
		{"forecast extra argument", []string{"forecast", "extra"}, ExitUsage, "", `unexpected argument "extra"`},
		{"render without roadmap.json", []string{"render"}, ExitFailure, "", "beadline render: open roadmap.json"},
		{"render bad flag", []string{"render", "--frobnicate"}, ExitUsage, "", "flag provided but not defined"},
		{"render extra argument", []string{"render", "extra"}, ExitUsage, "", `unexpected argument "extra"`},
		{"render -h", []string{"render", "-h"}, ExitOK, "", "-title"},
		{"serve stub", []string{"serve"}, ExitFailure, "", "beadline serve: not implemented yet"},
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
		if !strings.Contains(stdout, "  "+c.name+" ") {
			t.Errorf("usage does not list %q:\n%s", c.name, stdout)
		}
	}
}

func TestVersion(t *testing.T) {
	saved := Version
	t.Cleanup(func() { Version = saved })

	Version = "v1.2.3"
	if _, stdout, _ := run("version"); stdout != "beadline v1.2.3\n" {
		t.Errorf("with linker version: stdout = %q", stdout)
	}

	Version = ""
	if got := version(); got == "" {
		t.Error("version() is empty without a linker version; want a module version or \"dev\"")
	}
}

func TestCheckOutput(t *testing.T) {
	code, stdout, stderr := run("check", "--config", "../../testdata/problems/beadline.toml")
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

	_, stdout, _ = run("check", "--config", "../../testdata/multirepo/beadline.toml")
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
