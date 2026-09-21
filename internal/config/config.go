package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

// Config is what a run uses: the repositories to read, the conventions and
// the model settings. It comes from beadline.toml, from the command line,
// or from Default alone: one repository needs no file.
type Config struct {
	Repos       []Repo
	Conventions Conventions
	Model       Model
	// Idle is expert.idle_gap_hours, expert.idle_resume_at and expert.idle:
	// what every clock masks out as time nothing ran (docs/design.md,
	// ADR-3).
	Idle Idle
	// Agents are the agent counts by repo name ([expert] agents). SetRepos
	// and ApplyAgents copy them onto Repos.
	Agents map[string]Concurrency
	// Hide lists high-level beads and goals to leave off the roadmap.
	Hide []string

	// Dir is the directory relative repo paths resolve against: the
	// directory of the config file, or the working directory.
	Dir string
}

// Idle configures the idle-time mask (docs/design.md, ADR-3): the stretches
// of calendar time beadline learns as downtime, not work.
type Idle struct {
	// GapHours is G: a gap of more than this many hours with no activity
	// anywhere across every loaded repo counts as inferred idle time. 0
	// disables inference; only Declared windows mask anything.
	GapHours float64
	// ResumeAt is when work is declared to resume, if the data shows the
	// city idle as of now (an open Declared window, or a gap already past
	// GapHours). It is the horizon a forecast counts dates from instead of
	// now, when it is set and in the future (ADR-3 §2). Zero: none
	// declared.
	ResumeAt time.Time
	// Declared are explicit idle windows: a zero-inference override for
	// GapHours, and the only way to mask a gap inference would miss.
	Declared []DeclaredWindow
}

// DeclaredWindow is one explicit idle window (expert.idle). End zero means
// the window was still open as of when the config was written.
type DeclaredWindow struct {
	Start, End time.Time
	Note       string
}

// Repo is one repository.
type Repo struct {
	Name string
	// Export is the repository as given: a directory with .beads, read
	// live with `bd -C DIR --readonly export`, or a `bd export` JSONL file.
	Export      string
	Concurrency Concurrency
}

// ExportPath is the export file path with relative paths resolved against
// the config directory.
func (r Repo) ExportPath(c *Config) string {
	if filepath.IsAbs(r.Export) || c.Dir == "" {
		return r.Export
	}
	return filepath.Join(c.Dir, r.Export)
}

// Concurrency is the maximum number of agents working a repository at once,
// or Measure to learn it from history. The zero value means Measure. A
// measured value is shown but sets no limit: measured lead times already
// hold the wait for a free agent (ADR-2 §1).
type Concurrency struct {
	Max int // > 0 when fixed; 0 means measure
}

// Measure reports whether concurrency is learned from history.
func (c Concurrency) Measure() bool { return c.Max == 0 }

func (c Concurrency) String() string {
	if c.Measure() {
		return "measure"
	}
	return fmt.Sprint(c.Max)
}

// UnmarshalTOML accepts a positive integer or the string "measure".
func (c *Concurrency) UnmarshalTOML(v any) error {
	switch v := v.(type) {
	case int64:
		if v < 1 {
			return fmt.Errorf("agents must be at least 1 or \"measure\", got %d", v)
		}
		c.Max = int(v)
		return nil
	case string:
		if v != "measure" {
			return fmt.Errorf("agents must be a positive integer or \"measure\", got %q", v)
		}
		c.Max = 0
		return nil
	default:
		return fmt.Errorf("agents must be a positive integer or \"measure\", got %v", v)
	}
}

// ParseConcurrency reads an agent count as the command line gives it: a
// positive integer or "measure".
func ParseConcurrency(s string) (Concurrency, error) {
	if s == "measure" {
		return Concurrency{}, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 1 {
		return Concurrency{}, fmt.Errorf("agents must be a positive integer or \"measure\", got %q", s)
	}
	return Concurrency{Max: n}, nil
}

// Conventions say how a team uses beads. They are configuration, not
// assumptions: every pattern here can be replaced.
type Conventions struct {
	// HighLevelTypes are the issue types shown on the roadmap.
	HighLevelTypes []string
	// GoalLabelPattern matches a label that links a bead to a cross-repo
	// goal. The goal ID is the submatch named "id", else the first submatch.
	GoalLabelPattern string
	// HumanGateTitlePatterns mark a bead as waiting on a human by title.
	HumanGateTitlePatterns []string
	// HumanGateMetadataKeys mark a bead as waiting on a human when the key is
	// set in its metadata to anything but "", false, 0 or null.
	HumanGateMetadataKeys []string
	// InfraTypes are orchestration issue types that are not work and are
	// dropped on load, together with every edge to them.
	InfraTypes []string

	goalRe  *regexp.Regexp
	goalIdx int
	gateRes []*regexp.Regexp
}

// Model holds the simulation settings (docs/design.md, ADR-1 §2).
type Model struct {
	WindowDays          int
	Simulations         int
	Seed                uint64
	HumanGateHoursPrior float64
	CycleMinutesPrior   float64
	PoolingStrength     float64
	TailCapFactor       float64
}

// Default returns the configuration used for every key a file leaves out.
// It has no repositories.
func Default() Config {
	c := Config{
		Conventions: Conventions{
			HighLevelTypes:         []string{"milestone", "epic"},
			GoalLabelPattern:       `^goal:(?P<id>.+)$`,
			HumanGateTitlePatterns: []string{`^HUMAN:`},
			HumanGateMetadataKeys:  []string{"awaiting_signoff", "plan_first", "hold_reason"},
			InfraTypes:             []string{"molecule", "wisp", "step", "convoy", "message", "gate", "event"},
		},
		Model: Model{
			WindowDays:          45,
			Simulations:         2000,
			Seed:                1,
			HumanGateHoursPrior: 24,
			CycleMinutesPrior:   60,
			PoolingStrength:     10,
			TailCapFactor:       3,
		},
		// 24h: a gap the whole loaded set of repos shares, not one quiet
		// repo. ADR-3 picked it from the release-gate backtest.
		Idle:   Idle{GapHours: 24},
		Agents: map[string]Concurrency{},
	}
	if err := c.Conventions.Compile(); err != nil {
		panic("config: invalid default conventions: " + err.Error())
	}
	return c
}

// file is the beadline.toml layout: the repositories, and an [expert] table
// for everything else (docs/design.md, ADR-2 §5).
type file struct {
	Repos  []string `toml:"repos"`
	Expert expert   `toml:"expert"`
}

type expert struct {
	Agents            map[string]Concurrency `toml:"agents"`
	RoadmapTypes      []string               `toml:"roadmap_types"`
	GoalLabel         string                 `toml:"goal_label"`
	HumanGate         humanGate              `toml:"human_gate"`
	Hide              []string               `toml:"hide"`
	HistoryDays       int                    `toml:"history_days"`
	IgnoreTypes       []string               `toml:"ignore_types"`
	Runs              int                    `toml:"runs"`
	Seed              uint64                 `toml:"seed"`
	CycleMinutesPrior float64                `toml:"cycle_minutes_prior"`
	PoolingStrength   float64                `toml:"pooling_strength"`
	TailCapFactor     float64                `toml:"tail_cap_factor"`
	IdleGapHours      float64                `toml:"idle_gap_hours"`
	IdleResumeAt      time.Time              `toml:"idle_resume_at"`
	Idle              []idleWindow           `toml:"idle"`
}

type humanGate struct {
	Titles     []string `toml:"titles"`
	Metadata   []string `toml:"metadata"`
	HoursPrior float64  `toml:"hours_prior"`
}

// idleWindow is one entry of expert.idle: an explicit idle interval. End
// zero (omitted) means the window was still open as of the config.
type idleWindow struct {
	Start time.Time `toml:"start"`
	End   time.Time `toml:"end"`
	Note  string    `toml:"note"`
}

// Keys lists every key beadline.toml accepts, as dotted paths. Under
// expert.agents any repo name is a key.
var Keys = []string{
	"repos",
	"expert.agents",
	"expert.roadmap_types",
	"expert.goal_label",
	"expert.human_gate.titles",
	"expert.human_gate.metadata",
	"expert.human_gate.hours_prior",
	"expert.hide",
	"expert.history_days",
	"expert.ignore_types",
	"expert.runs",
	"expert.seed",
	"expert.cycle_minutes_prior",
	"expert.pooling_strength",
	"expert.tail_cap_factor",
	"expert.idle_gap_hours",
	"expert.idle_resume_at",
	"expert.idle",
}

// renamed maps the keys of the layout before ADR-2 to their new names.
var renamed = map[string]string{
	"conventions.high_level_types":          "expert.roadmap_types",
	"conventions.goal_label_pattern":        "expert.goal_label",
	"conventions.human_gate_title_patterns": "expert.human_gate.titles",
	"conventions.human_gate_metadata_keys":  "expert.human_gate.metadata",
	"conventions.infra_types":               "expert.ignore_types",
	"model.window_days":                     "expert.history_days",
	"model.simulations":                     "expert.runs",
	"model.seed":                            "expert.seed",
	"model.human_gate_hours_prior":          "expert.human_gate.hours_prior",
	"model.cycle_minutes_prior":             "expert.cycle_minutes_prior",
	"model.pooling_strength":                "expert.pooling_strength",
	"model.tail_cap_factor":                 "expert.tail_cap_factor",
}

func fileOf(c *Config) file {
	cv, m := c.Conventions, c.Model
	windows := make([]idleWindow, len(c.Idle.Declared))
	for i, d := range c.Idle.Declared {
		windows[i] = idleWindow{Start: d.Start, End: d.End, Note: d.Note}
	}
	return file{Expert: expert{
		Agents:       map[string]Concurrency{},
		RoadmapTypes: cv.HighLevelTypes,
		GoalLabel:    cv.GoalLabelPattern,
		HumanGate: humanGate{Titles: cv.HumanGateTitlePatterns, Metadata: cv.HumanGateMetadataKeys,
			HoursPrior: m.HumanGateHoursPrior},
		HistoryDays:       m.WindowDays,
		IgnoreTypes:       cv.InfraTypes,
		Runs:              m.Simulations,
		Seed:              m.Seed,
		CycleMinutesPrior: m.CycleMinutesPrior,
		PoolingStrength:   m.PoolingStrength,
		TailCapFactor:     m.TailCapFactor,
		IdleGapHours:      c.Idle.GapHours,
		IdleResumeAt:      c.Idle.ResumeAt,
		Idle:              windows,
	}}
}

func (f *file) apply(c *Config) {
	e := f.Expert
	c.Conventions = Conventions{
		HighLevelTypes:         e.RoadmapTypes,
		GoalLabelPattern:       e.GoalLabel,
		HumanGateTitlePatterns: e.HumanGate.Titles,
		HumanGateMetadataKeys:  e.HumanGate.Metadata,
		InfraTypes:             e.IgnoreTypes,
	}
	c.Model = Model{
		WindowDays:          e.HistoryDays,
		Simulations:         e.Runs,
		Seed:                e.Seed,
		HumanGateHoursPrior: e.HumanGate.HoursPrior,
		CycleMinutesPrior:   e.CycleMinutesPrior,
		PoolingStrength:     e.PoolingStrength,
		TailCapFactor:       e.TailCapFactor,
	}
	declared := make([]DeclaredWindow, len(e.Idle))
	for i, w := range e.Idle {
		declared[i] = DeclaredWindow{Start: w.Start, End: w.End, Note: w.Note}
	}
	c.Idle = Idle{GapHours: e.IdleGapHours, ResumeAt: e.IdleResumeAt, Declared: declared}
	c.Agents = e.Agents
	c.Hide = e.Hide
	c.SetRepos(f.Repos)
}

// Load reads and validates the config file at path. Relative repo paths
// resolve against the file's directory.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	c, err := parse(string(data), filepath.Dir(path))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return c, nil
}

// Parse decodes and validates a beadline.toml document. Keys it leaves out
// keep their Default values; an unknown key is an error that names the
// closest valid key, so a typo cannot silently fall back to a default.
// Relative repo paths resolve against the working directory.
func Parse(doc string) (*Config, error) {
	return parse(doc, "")
}

func parse(doc, dir string) (*Config, error) {
	c := Default()
	c.Dir = dir
	f := fileOf(&c)
	md, err := toml.Decode(doc, &f)
	if err != nil {
		var raw map[string]any
		if _, rerr := toml.Decode(doc, &raw); rerr == nil && isTables(raw["repos"]) {
			return nil, errors.New(`repos is a list of paths: repos = ["../api", "web.jsonl"]; ` +
				`set agent counts under [expert] as agents = { api = 3 }`)
		}
		return nil, err
	}
	if err := unknownKeys(md.Undecoded()); err != nil {
		return nil, err
	}
	f.apply(&c)
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

// isTables reports whether v is an array of tables: [[repos]] from the
// layout before ADR-2.
func isTables(v any) bool {
	_, ok := v.([]map[string]any)
	return ok
}

// unknownKeys reports every key that is not in Keys, with a suggestion. An
// unknown table is reported by its name, except a table of the layout
// before ADR-2, whose keys are reported one by one with their new names.
func unknownKeys(undecoded []toml.Key) error {
	all := make([]string, len(undecoded))
	for i, k := range undecoded {
		all[i] = k.String()
	}
	oldTable := func(k string) bool { return k == "model" || k == "conventions" }
	var errs []error
	for _, k := range all {
		skip := false
		for _, other := range all {
			switch {
			case oldTable(k) && strings.HasPrefix(other, k+"."):
				skip = true // its keys are reported
			case !oldTable(other) && strings.HasPrefix(k, other+"."):
				skip = true // the unknown table around it is reported
			}
		}
		if !skip {
			errs = append(errs, fmt.Errorf("unknown key %s%s", k, suggest(k)))
		}
	}
	return errors.Join(errs...)
}

// suggest names the valid key a mistyped or outdated one most likely meant.
func suggest(key string) string {
	if to, ok := renamed[key]; ok {
		return "; it is " + to + " now"
	}
	best, bestDist := "", -1
	for _, cand := range Keys {
		d := Distance(key, cand)
		if !strings.HasPrefix(key, "expert.") {
			d = min(d, Distance("expert."+key, cand))
		}
		last := cand[strings.LastIndex(cand, ".")+1:]
		if d <= max(3, len(last)/2) && (bestDist < 0 || d < bestDist) {
			best, bestDist = cand, d
		}
	}
	if best == "" {
		return "; valid keys: " + strings.Join(Keys, ", ")
	}
	return "; did you mean " + best + "?"
}

// Distance is the Levenshtein distance between a and b: what a suggestion
// for a mistyped key or command is ranked by.
func Distance(a, b string) int {
	prev := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur := make([]int, len(b)+1)
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev = cur
	}
	return prev[len(b)]
}

// SetRepos replaces the repositories with paths, each named after its
// repository (see RepoName), and applies Agents to them.
func (c *Config) SetRepos(paths []string) {
	c.Repos = make([]Repo, 0, len(paths))
	used := map[string]bool{}
	for _, p := range paths {
		name := RepoName(filepath.Join(c.Dir, p))
		if filepath.IsAbs(p) {
			name = RepoName(p)
		}
		unique := name
		for n := 2; used[unique]; n++ {
			unique = fmt.Sprintf("%s-%d", name, n)
		}
		used[unique] = true
		c.Repos = append(c.Repos, Repo{Name: unique, Export: p})
	}
	c.ApplyAgents()
}

// ApplyAgents sets each repository's concurrency from Agents and returns
// the names in Agents that match no repository, sorted.
func (c *Config) ApplyAgents() (unknown []string) {
	names := map[string]bool{}
	for n := range c.Repos {
		r := &c.Repos[n]
		names[r.Name] = true
		r.Concurrency = c.Agents[r.Name]
	}
	for name := range c.Agents {
		if !names[name] {
			unknown = append(unknown, name)
		}
	}
	sort.Strings(unknown)
	return unknown
}

// RepoName names a repository after its path: the directory that holds
// .beads, or an export file's name without its extension.
func RepoName(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = filepath.Clean(path)
	}
	if IsExportFile(abs) {
		dir := filepath.Dir(abs)
		if filepath.Base(dir) == ".beads" {
			return filepath.Base(filepath.Dir(dir))
		}
		base := filepath.Base(abs)
		return strings.TrimSuffix(base, filepath.Ext(base))
	}
	if filepath.Base(abs) == ".beads" {
		abs = filepath.Dir(abs)
	}
	return filepath.Base(abs)
}

// IsExportFile reports whether a repository path names an export file
// rather than a repository directory, by its extension.
func IsExportFile(path string) bool {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".jsonl", ".json", ".ndjson":
		return true
	}
	return false
}

// Validate checks everything but the repositories, which the command line
// may still replace; a repo path must not be empty.
func (c *Config) Validate() error {
	var errs []error
	for i, r := range c.Repos {
		if strings.TrimSpace(r.Export) == "" {
			errs = append(errs, fmt.Errorf("repos[%d] is empty", i))
		}
	}
	if err := c.Conventions.Compile(); err != nil {
		errs = append(errs, err)
	}
	errs = append(errs, c.Model.validate()...)
	errs = append(errs, c.Idle.validate()...)
	return errors.Join(errs...)
}

func (idl Idle) validate() []error {
	var errs []error
	if idl.GapHours < 0 {
		errs = append(errs, fmt.Errorf("expert.idle_gap_hours must not be negative, got %v", idl.GapHours))
	}
	for i, w := range idl.Declared {
		switch {
		case w.Start.IsZero():
			errs = append(errs, fmt.Errorf("expert.idle[%d]: start is required", i))
		case !w.End.IsZero() && !w.End.After(w.Start):
			errs = append(errs, fmt.Errorf("expert.idle[%d]: end (%v) must be after start (%v)", i, w.End, w.Start))
		}
	}
	return errs
}

func (m Model) validate() []error {
	var errs []error
	positive := func(key string, v float64) {
		if !(v > 0) {
			errs = append(errs, fmt.Errorf("expert.%s must be positive, got %v", key, v))
		}
	}
	positive("history_days", float64(m.WindowDays))
	positive("runs", float64(m.Simulations))
	positive("human_gate.hours_prior", m.HumanGateHoursPrior)
	positive("cycle_minutes_prior", m.CycleMinutesPrior)
	if !(m.PoolingStrength >= 0) {
		errs = append(errs, fmt.Errorf("expert.pooling_strength must not be negative, got %v", m.PoolingStrength))
	}
	if !(m.TailCapFactor >= 1) {
		errs = append(errs, fmt.Errorf("expert.tail_cap_factor must be at least 1, got %v", m.TailCapFactor))
	}
	return errs
}

// NonDefault lists the settings that differ from Default, in beadline.toml
// syntax (expert. omitted), for the page footer: agents first, then the
// other keys in the order of Keys.
func (c *Config) NonDefault() []string {
	def := Default()
	f, d := fileOf(c), fileOf(&def)
	var out []string
	for _, r := range c.Repos {
		if !r.Concurrency.Measure() {
			out = append(out, fmt.Sprintf("agents.%s = %d", r.Name, r.Concurrency.Max))
		}
	}
	list := func(v []string) string {
		b, _ := json.Marshal(v)
		return strings.ReplaceAll(string(b), `","`, `", "`)
	}
	add := func(key string, differs bool, value string) {
		if differs {
			out = append(out, key+" = "+value)
		}
	}
	e, de := f.Expert, d.Expert
	add("roadmap_types", !equal(e.RoadmapTypes, de.RoadmapTypes), list(e.RoadmapTypes))
	add("goal_label", e.GoalLabel != de.GoalLabel, strconv.Quote(e.GoalLabel))
	add("human_gate.titles", !equal(e.HumanGate.Titles, de.HumanGate.Titles), list(e.HumanGate.Titles))
	add("human_gate.metadata", !equal(e.HumanGate.Metadata, de.HumanGate.Metadata), list(e.HumanGate.Metadata))
	add("human_gate.hours_prior", e.HumanGate.HoursPrior != de.HumanGate.HoursPrior, num(e.HumanGate.HoursPrior))
	add("hide", len(c.Hide) > 0, list(c.Hide))
	add("history_days", e.HistoryDays != de.HistoryDays, strconv.Itoa(e.HistoryDays))
	add("ignore_types", !equal(e.IgnoreTypes, de.IgnoreTypes), list(e.IgnoreTypes))
	add("runs", e.Runs != de.Runs, strconv.Itoa(e.Runs))
	add("seed", e.Seed != de.Seed, strconv.FormatUint(e.Seed, 10))
	add("cycle_minutes_prior", e.CycleMinutesPrior != de.CycleMinutesPrior, num(e.CycleMinutesPrior))
	add("pooling_strength", e.PoolingStrength != de.PoolingStrength, num(e.PoolingStrength))
	add("tail_cap_factor", e.TailCapFactor != de.TailCapFactor, num(e.TailCapFactor))
	add("idle_gap_hours", e.IdleGapHours != de.IdleGapHours, num(e.IdleGapHours))
	if !c.Idle.ResumeAt.IsZero() {
		out = append(out, "idle_resume_at = "+strconv.Quote(c.Idle.ResumeAt.UTC().Format(time.RFC3339)))
	}
	if len(c.Idle.Declared) > 0 {
		out = append(out, fmt.Sprintf("idle = %s declared", plural(float64(len(c.Idle.Declared)), "window")))
	}
	return out
}

func plural(n float64, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return num(n) + " " + noun + "s"
}

func num(f float64) string { return strconv.FormatFloat(f, 'g', -1, 64) }

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// Compile validates the conventions and prepares their matchers. Parse, Load
// and Default call it; code that changes Conventions must call it again.
func (cv *Conventions) Compile() error {
	var errs []error
	if len(cv.HighLevelTypes) == 0 {
		errs = append(errs, errors.New("expert.roadmap_types must name at least one type"))
	}
	infra := map[string]bool{}
	for _, t := range cv.InfraTypes {
		infra[t] = true
	}
	for _, t := range cv.HighLevelTypes {
		if infra[t] {
			errs = append(errs, fmt.Errorf("expert: type %q is in both roadmap_types and ignore_types", t))
		}
	}

	cv.goalRe, cv.goalIdx = nil, 0
	if cv.GoalLabelPattern != "" {
		re, err := regexp.Compile(cv.GoalLabelPattern)
		switch {
		case err != nil:
			errs = append(errs, fmt.Errorf("expert.goal_label: %w", err))
		case re.NumSubexp() == 0:
			errs = append(errs, errors.New("expert.goal_label needs a submatch for the goal ID, e.g. (?P<id>.+)"))
		default:
			cv.goalRe, cv.goalIdx = re, 1
			if i := re.SubexpIndex("id"); i > 0 {
				cv.goalIdx = i
			}
		}
	}

	cv.gateRes = nil
	for _, p := range cv.HumanGateTitlePatterns {
		re, err := regexp.Compile(p)
		if err != nil {
			errs = append(errs, fmt.Errorf("expert.human_gate.titles: %w", err))
			continue
		}
		cv.gateRes = append(cv.gateRes, re)
	}
	return errors.Join(errs...)
}

// IsHighLevel reports whether issues of this type appear on the roadmap.
func (cv *Conventions) IsHighLevel(issueType string) bool {
	return contains(cv.HighLevelTypes, issueType)
}

// IsInfra reports whether issues of this type are orchestration, not work.
func (cv *Conventions) IsInfra(issueType string) bool {
	return contains(cv.InfraTypes, issueType)
}

// Goals returns the goal IDs named by labels, sorted and without duplicates.
func (cv *Conventions) Goals(labels []string) []string {
	if cv.goalRe == nil {
		return nil
	}
	var goals []string
	for _, l := range labels {
		if m := cv.goalRe.FindStringSubmatch(l); m != nil && m[cv.goalIdx] != "" && !contains(goals, m[cv.goalIdx]) {
			goals = append(goals, m[cv.goalIdx])
		}
	}
	sort.Strings(goals)
	return goals
}

// IsHumanGate reports whether a bead waits on a human: its title matches a
// gate pattern or one of the gate metadata keys is set.
func (cv *Conventions) IsHumanGate(title string, metadata map[string]json.RawMessage) bool {
	for _, re := range cv.gateRes {
		if re.MatchString(title) {
			return true
		}
	}
	for _, k := range cv.HumanGateMetadataKeys {
		if v, ok := metadata[k]; ok && isSet(v) {
			return true
		}
	}
	return false
}

// isSet reports whether a metadata value means "yes": anything but "",
// false, 0 or null. A cleared hold is often left behind as "".
func isSet(v json.RawMessage) bool {
	switch string(bytes.TrimSpace(v)) {
	case "", `""`, "false", "0", "null":
		return false
	}
	return true
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
