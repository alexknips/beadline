package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/alexknips/beadline/internal/config"
	"github.com/alexknips/beadline/internal/load"
)

// defaultConfig is the config file read when -c is not given, if present.
const defaultConfig = "beadline.toml"

// exportTimeout bounds reading the repositories, which run in parallel.
const exportTimeout = 5 * time.Minute

// newExporter reads repository directories; tests replace it.
var newExporter = load.BdExporter

// repoFlags are the flags of every command that reads repos.
type repoFlags struct {
	config string
	bd     string
}

func (o *repoFlags) register(fs *flag.FlagSet) {
	fs.StringVar(&o.config, "c", "", "")
	fs.StringVar(&o.config, "config", "", "")
	fs.StringVar(&o.bd, "bd", "bd", "")
}

// resolve returns the configuration a command runs with. Settings come from
// -c, else ./beadline.toml when it exists, else the defaults. Repos come
// from paths, else from the file, else the current directory when it holds
// .beads. A path that is neither a repository nor a file is a usage error,
// and so is finding no repos at all; a config file that cannot be read is
// a failure. On error resolve has reported it and returns the exit code.
func (o *repoFlags) resolve(cmd string, paths []string, stderr io.Writer) (*config.Config, int) {
	fail := func(err error) (*config.Config, int) {
		fmt.Fprintf(stderr, "%s: %v\n", name(cmd), err)
		return nil, ExitFailure
	}
	for _, p := range paths {
		if _, _, err := load.Locate(p); err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				what := "no such repository or file"
				if !strings.ContainsAny(p, `/\.`) {
					what = "not a command, repository or file"
				}
				return nil, usageError(stderr, cmd, "%s: %s%s", p, what, didYouMean(p))
			}
			return nil, usageError(stderr, cmd, "%v", err)
		}
	}

	var cfg *config.Config
	path := o.config
	if path == "" {
		if _, err := os.Stat(defaultConfig); err == nil {
			path = defaultConfig
		}
	}
	if path != "" {
		c, err := config.Load(path)
		if err != nil {
			return fail(err)
		}
		cfg = c
	} else {
		c := config.Default()
		cfg = &c
	}

	switch {
	case len(paths) > 0:
		// Paths on the command line are relative to the working directory,
		// not to the config file.
		if abs, _ := filepath.Abs(cfg.Dir); cfg.Dir != "" && abs != workingDir() {
			for n, p := range paths {
				if abs, err := filepath.Abs(p); err == nil {
					paths[n] = abs
				}
			}
		}
		cfg.SetRepos(paths)
	case len(cfg.Repos) > 0:
	default:
		if _, live, err := load.Locate("."); err == nil && live {
			cfg.SetRepos([]string{"."})
			break
		}
		return nil, usageError(stderr, cmd, "no repos: run beadline in a repository with .beads, "+
			"pass repository directories or bd export .jsonl files, or list repos in %s", defaultConfig)
	}
	return cfg, ExitOK
}

func workingDir() string {
	wd, _ := os.Getwd()
	return wd
}

// warnAgents warns about agent counts for repos that are not loaded.
func warnAgents(cfg *config.Config, stderr io.Writer) {
	if unknown := cfg.ApplyAgents(); len(unknown) > 0 {
		var names []string
		for _, r := range cfg.Repos {
			names = append(names, r.Name)
		}
		fmt.Fprintf(stderr, "warning: agents set for %s, which is not loaded (repos: %s); ignored\n",
			strings.Join(unknown, ", "), strings.Join(names, ", "))
	}
}

// read reads every repo in parallel and reports the ones that failed.
func (o *repoFlags) read(cfg *config.Config, stderr io.Writer) (exports []load.Export, failed int) {
	ctx, cancel := context.WithTimeout(context.Background(), exportTimeout)
	defer cancel()
	exports = load.ReadAll(ctx, cfg, newExporter(o.bd))
	for _, e := range exports {
		if e.Err != nil {
			fmt.Fprintf(stderr, "error: %v\n", e.Err)
			failed++
		}
	}
	return exports, failed
}

// parseExports builds the graph from the exports that were read, as the
// data stood at asOf (zero: as it is). An export that is not valid JSONL
// fails its repo, like one that could not be read, and the others still
// load.
func parseExports(cfg *config.Config, exports []load.Export, asOf time.Time, stderr io.Writer) (l *loaded, failed int, err error) {
	for {
		ex, err := load.ParseExports(exports)
		if err == nil {
			g, rep, err := ex.Graph(&cfg.Conventions, asOf)
			if err != nil {
				return nil, failed, err
			}
			return &loaded{graph: g, report: rep, exports: exports, horizon: ex.Horizon(),
				activity: ex.ActivityTimestamps(asOf)}, failed, nil
		}
		// Find the export at fault: the error names its source.
		bad := -1
		for n, e := range exports {
			if e.Err == nil && strings.HasPrefix(err.Error(), e.Source+":") {
				bad = n
			}
		}
		if bad < 0 {
			return nil, failed, err
		}
		exports[bad].Err = fmt.Errorf("repo %s: %w", exports[bad].Repo, err)
		exports[bad].Data = nil
		fmt.Fprintf(stderr, "error: %v\n", exports[bad].Err)
		failed++
	}
}

// agentFlags collects --agents REPO=N, repeatable and comma-separated.
type agentFlags map[string]config.Concurrency

func (a agentFlags) String() string {
	var parts []string
	for repo, c := range a {
		parts = append(parts, repo+"="+c.String())
	}
	sort.Strings(parts)
	return strings.Join(parts, ",")
}

func (a agentFlags) Set(v string) error {
	for _, part := range strings.Split(v, ",") {
		repo, n, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok || repo == "" {
			return fmt.Errorf("want REPO=N, got %q", part)
		}
		c, err := config.ParseConcurrency(n)
		if err != nil {
			return err
		}
		a[repo] = c
	}
	return nil
}

func name(cmd string) string {
	if cmd == "" {
		return "beadline"
	}
	return "beadline " + cmd
}
