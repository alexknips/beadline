package load

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/alexknips/beadline/internal/config"
)

// fakeBd writes a bd that prints its arguments and BEADS_DIR as one JSONL
// record, or fails with a message on stderr when its directory is "broken".
func fakeBd(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the fake bd is a shell script")
	}
	path := filepath.Join(t.TempDir(), "bd")
	script := `#!/bin/sh
case "$2" in
*/broken) echo "warming up" >&2; echo "Error: database is locked" >&2; exit 3 ;;
esac
printf '{"id":"x-1","title":"%s","description":"%s"}\n' "$*" "$BEADS_DIR"
`
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestBdExporter(t *testing.T) {
	bd := fakeBd(t)
	export := BdExporter(bd)
	data, err := export(context.Background(), "/repos/api")
	if err != nil {
		t.Fatal(err)
	}
	want := `{"id":"x-1","title":"-C /repos/api --readonly export","description":"/repos/api/.beads"}` + "\n"
	if string(data) != want {
		t.Errorf("export = %s, want %s", data, want)
	}

	_, err = export(context.Background(), "/repos/broken")
	if err == nil || !strings.HasSuffix(err.Error(), ": exit status 3: Error: database is locked") {
		t.Errorf("failed export: %v", err)
	}
	if _, err = BdExporter(filepath.Join(t.TempDir(), "no-bd"))(context.Background(), "/repos/api"); err == nil ||
		!strings.Contains(err.Error(), "no-bd is not installed") {
		t.Errorf("missing bd: %v", err)
	}
	if _, err := BdExporter("beadline-no-such-bd")(context.Background(), "/repos/api"); err == nil ||
		!strings.Contains(err.Error(), "beadline-no-such-bd is not installed: install beads, or pass a bd export .jsonl file") {
		t.Errorf("bd not on PATH: %v", err)
	}
}

func TestLocate(t *testing.T) {
	dir := t.TempDir()
	repo := filepath.Join(dir, "repo")
	if err := os.MkdirAll(filepath.Join(repo, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "x.jsonl")
	if err := os.WriteFile(file, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]string{repo: repo, filepath.Join(repo, ".beads"): repo, file: ""} {
		got, live, err := Locate(path)
		if err != nil || got != want || live != (want != "") {
			t.Errorf("Locate(%s) = %q, %v, %v; want %q", path, got, live, err, want)
		}
	}
	if _, _, err := Locate(dir); err == nil || !strings.Contains(err.Error(), "has no .beads directory") {
		t.Errorf("a directory without .beads: %v", err)
	}
	if _, _, err := Locate(filepath.Join(dir, "missing")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a missing path: %v", err)
	}
}

func TestReadAll(t *testing.T) {
	bd := fakeBd(t)
	dir := t.TempDir()
	for _, repo := range []string{"live", "broken"} {
		if err := os.MkdirAll(filepath.Join(dir, repo, ".beads"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "file.jsonl"), []byte(`{"id":"f-1"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Dir = dir
	cfg.SetRepos([]string{"live", "file.jsonl", "broken", "missing.jsonl"})

	exports := ReadAll(context.Background(), &cfg, BdExporter(bd))
	if len(exports) != 4 {
		t.Fatalf("%d exports", len(exports))
	}
	live, file, broken, missing := exports[0], exports[1], exports[2], exports[3]
	if live.Err != nil || !live.Live || live.Repo != "live" || live.Path != "live" || live.Source != filepath.Join(dir, "live") ||
		!strings.Contains(string(live.Data), "-C "+filepath.Join(dir, "live")+" --readonly export") {
		t.Errorf("live = %+v", live)
	}
	if file.Err != nil || file.Live || file.Source != filepath.Join(dir, "file.jsonl") || string(file.Data) != `{"id":"f-1"}`+"\n" {
		t.Errorf("file = %+v", file)
	}
	if broken.Err == nil || broken.Data != nil || !strings.HasPrefix(broken.Err.Error(), "repo broken: ") {
		t.Errorf("broken = %+v", broken)
	}
	if missing.Err == nil || !errors.Is(missing.Err, os.ErrNotExist) {
		t.Errorf("missing = %+v", missing)
	}

	g, rep, err := FromExports(&cfg.Conventions, exports)
	if err != nil {
		t.Fatal(err)
	}
	if g.Len() != 2 || g.Issue("x-1").Repo != "live" || g.Issue("f-1").Repo != "file" || len(rep.Repos) != 2 {
		t.Errorf("graph of %d issues, report %+v", g.Len(), rep.Repos)
	}

	// Load fails on the first repo it cannot read.
	cfg.SetRepos([]string{"file.jsonl", "missing.jsonl"})
	if _, _, err := Load(&cfg); err == nil || !strings.HasPrefix(err.Error(), "repo missing: ") {
		t.Errorf("Load with a missing export: %v", err)
	}
}
