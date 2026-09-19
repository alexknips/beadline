package load

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/alexknips/beadline/internal/config"
	"github.com/alexknips/beadline/internal/graph"
)

// Export is one repository's bd export, read into memory.
type Export struct {
	Repo   string // repo name
	Path   string // the repository as configured: a directory or an export file
	Source string // where the data came from, for reports: the file, or the directory bd ran in
	Live   bool   // read with bd rather than from a file
	Data   []byte
	Err    error // why the repository could not be read; Data is nil then
}

// Exporter returns the `bd export` of the beads repository in dir.
type Exporter func(ctx context.Context, dir string) ([]byte, error)

// BdExporter returns an Exporter that runs `bd -C DIR --readonly export`
// with the bd binary (a name looked up in PATH, or a path). It points
// BEADS_DIR at DIR/.beads, so a BEADS_DIR inherited from the environment
// cannot swap in another database.
func BdExporter(bd string) Exporter {
	return func(ctx context.Context, dir string) ([]byte, error) {
		var stdout, stderr bytes.Buffer
		cmd := exec.CommandContext(ctx, bd, "-C", dir, "--readonly", "export")
		cmd.Env = append(os.Environ(), "BEADS_DIR="+filepath.Join(dir, ".beads"))
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		err := cmd.Run()
		switch {
		case errors.Is(err, exec.ErrNotFound), errors.Is(err, fs.ErrNotExist):
			return nil, fmt.Errorf("%s is not installed: install beads, or pass a bd export .jsonl file", bd)
		case ctx.Err() != nil:
			return nil, fmt.Errorf("%s export did not finish: %w", bd, ctx.Err())
		case err != nil:
			msg := strings.TrimSpace(stderr.String())
			if i := strings.LastIndexByte(msg, '\n'); i >= 0 {
				msg = msg[i+1:]
			}
			if msg == "" {
				return nil, fmt.Errorf("%s export: %w", bd, err)
			}
			return nil, fmt.Errorf("%s export: %w: %s", bd, err, msg)
		}
		return stdout.Bytes(), nil
	}
}

// Locate says how a repository path is read: from an export file, or live
// from dir, the absolute path of the repository directory that holds .beads
// (path itself, or its parent when path is the .beads directory).
func Locate(path string) (dir string, live bool, err error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", false, err
	}
	if !info.IsDir() {
		return "", false, nil
	}
	if dir, err = filepath.Abs(path); err != nil {
		return "", false, err
	}
	if filepath.Base(dir) == ".beads" {
		dir = filepath.Dir(dir)
	}
	if info, err := os.Stat(filepath.Join(dir, ".beads")); err != nil || !info.IsDir() {
		return "", false, fmt.Errorf("%s has no .beads directory: not a beads repository", path)
	}
	return dir, true, nil
}

// ReadAll reads every configured repository at once, in config order: a
// directory live through export, a file from disk. A repository that fails
// carries its error and the others still load.
func ReadAll(ctx context.Context, cfg *config.Config, export Exporter) []Export {
	out := make([]Export, len(cfg.Repos))
	var wg sync.WaitGroup
	for n, r := range cfg.Repos {
		path := r.ExportPath(cfg)
		out[n] = Export{Repo: r.Name, Path: r.Export, Source: path}
		wg.Add(1)
		go func(e *Export) {
			defer wg.Done()
			dir, live, err := Locate(path)
			switch {
			case err != nil:
				e.Err = err
			case live:
				e.Live, e.Source = true, dir
				e.Data, e.Err = export(ctx, dir)
			default:
				e.Data, e.Err = os.ReadFile(path)
			}
			if e.Err != nil {
				e.Data = nil
				e.Err = fmt.Errorf("repo %s: %w", e.Repo, e.Err)
			}
		}(&out[n])
	}
	wg.Wait()
	return out
}

// ParseExports parses the exports that were read, in order, as Parse does.
// Exports that failed are left out.
func ParseExports(exports []Export) (*Exports, error) {
	var sources []Source
	for _, e := range exports {
		if e.Err == nil {
			sources = append(sources, Source{Repo: e.Repo, Name: e.Source, R: bytes.NewReader(e.Data)})
		}
	}
	return Parse(sources...)
}

// FromExports builds one graph from the exports that were read, in order,
// as Read does. Exports that failed are left out.
func FromExports(cv *config.Conventions, exports []Export) (*graph.Graph, *Report, error) {
	ex, err := ParseExports(exports)
	if err != nil {
		return nil, nil, err
	}
	return ex.Graph(cv, time.Time{})
}
