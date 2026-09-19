package roadmap

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// SchemaVersion is the roadmap.json format this beadline writes and reads.
// Adding a field keeps the version; renaming, removing or changing the
// meaning of one bumps it. A reader rejects a newer version rather than
// misread it.
const SchemaVersion = 1

// Status says where a milestone or goal stands. It decides whether it has a
// forecast at all.
const (
	StatusDone           = "done"            // closed (within the model window)
	StatusForecast       = "forecast"        // open work remains; the dates say when it lands
	StatusReadyToClose   = "ready_to_close"  // all its work is closed, the bead itself is still open
	StatusNotPlanned     = "not_planned"     // no children and no open blockers: nothing to forecast
	StatusDeferred       = "deferred"        // bd status deferred, or defer_until is in the future
	StatusBlockedOutside = "blocked_outside" // waits on a bead in a repo that was not loaded
	StatusStalled        = "stalled"         // set by the forecaster: work remains but nothing moves
)

// Statuses lists every Status value.
var Statuses = []string{StatusDone, StatusForecast, StatusReadyToClose, StatusNotPlanned,
	StatusDeferred, StatusBlockedOutside, StatusStalled}

// Schedule compares the forecast with the target (due_at). It is empty when
// there is no target, or no forecast to compare yet.
const (
	OnTrack = "on_track" // P80 on or before the target
	AtRisk  = "at_risk"  // P50 on or before the target, P80 after it
	Late    = "late"     // P50 after the target, or the target has passed
)

// Roadmap is roadmap.json: one forecast run over one set of exports. It is
// also the snapshot that calibration scores later against what actually
// closed, so it records exactly what went in: the inputs' fingerprint, the
// configuration with its seed, and the beadline and model versions.
type Roadmap struct {
	SchemaVersion   int       `json:"schema_version"`
	GeneratedAt     time.Time `json:"generated_at"` // "now" of the run, UTC
	BeadlineVersion string    `json:"beadline_version"`
	// ModelVersion names the forecasting model revision (e.g. "adr-1"). It
	// is empty when nothing was forecast.
	ModelVersion string       `json:"model_version,omitempty"`
	Config       Config       `json:"config"`
	Inputs       Inputs       `json:"inputs"`
	Repos        []Repo       `json:"repos"`
	Goals        []Goal       `json:"goals"`
	Milestones   []Milestone  `json:"milestones"` // every high-level type, not only milestones
	Calibration  *Calibration `json:"calibration,omitempty"`
}

// Config echoes beadline.toml as the run used it, defaults filled in.
type Config struct {
	Repos       []ConfigRepo `json:"repos"`
	Conventions Conventions  `json:"conventions"`
	Model       Model        `json:"model"`
}

// ConfigRepo is one [[repos]] entry. Export is the path as configured.
type ConfigRepo struct {
	Name        string      `json:"name"`
	Export      string      `json:"export"`
	Concurrency Concurrency `json:"concurrency"`
}

// Concurrency is a configured agent limit, or 0 for "measure". It encodes
// as in beadline.toml: a number or the string "measure".
type Concurrency int

// MarshalJSON writes a positive limit as a number and 0 as "measure".
func (c Concurrency) MarshalJSON() ([]byte, error) {
	if c <= 0 {
		return []byte(`"measure"`), nil
	}
	return strconv.AppendInt(nil, int64(c), 10), nil
}

// UnmarshalJSON accepts a positive number or "measure".
func (c *Concurrency) UnmarshalJSON(b []byte) error {
	if string(b) == `"measure"` {
		*c = 0
		return nil
	}
	n, err := strconv.Atoi(string(b))
	if err != nil || n < 1 {
		return fmt.Errorf("concurrency must be a positive integer or \"measure\", got %s", b)
	}
	*c = Concurrency(n)
	return nil
}

// Conventions mirrors [conventions].
type Conventions struct {
	HighLevelTypes         []string `json:"high_level_types"`
	GoalLabelPattern       string   `json:"goal_label_pattern"`
	HumanGateTitlePatterns []string `json:"human_gate_title_patterns"`
	HumanGateMetadataKeys  []string `json:"human_gate_metadata_keys"`
	InfraTypes             []string `json:"infra_types"`
}

// Model mirrors [model], seed included. Its fields match config.Model one to
// one: Build converts between the two, so a new [model] key does not compile
// until it is mirrored here.
type Model struct {
	WindowDays          int     `json:"window_days"`
	Simulations         int     `json:"simulations"`
	Seed                uint64  `json:"seed"`
	HumanGateHoursPrior float64 `json:"human_gate_hours_prior"`
	CycleMinutesPrior   float64 `json:"cycle_minutes_prior"`
	PoolingStrength     float64 `json:"pooling_strength"`
	TailCapFactor       float64 `json:"tail_cap_factor"`
}

// Inputs fingerprints the exports a run read. Two runs with the same
// fingerprint, config and seed produce the same roadmap.
type Inputs struct {
	// Fingerprint is "sha256:" and the hex SHA-256 of one line per export,
	// in config order: the repo name, a tab, the export's SHA-256, a newline.
	Fingerprint string   `json:"fingerprint"`
	Exports     []Export `json:"exports"`
}

// Export is one repository export file as read.
type Export struct {
	Repo   string `json:"repo"`
	Path   string `json:"path"`   // as configured
	SHA256 string `json:"sha256"` // hex
	Bytes  int64  `json:"bytes"`
}

// Repo is one swimlane: a configured repository and its agents.
type Repo struct {
	Name   string `json:"name"`
	Issues int    `json:"issues"` // work beads loaded (infra dropped)
	Open   int    `json:"open"`
	Closed int    `json:"closed"`
	// Concurrency is the number of agents that work the repo at once, as
	// configured, or as measured by the forecaster ("measured"; absent
	// until measured).
	Concurrency       *float64 `json:"concurrency,omitempty"`
	ConcurrencySource string   `json:"concurrency_source"` // "configured" or "measured"
	// RatePerDay is the pace the forecaster measured: work beads closed per
	// day over the model window.
	RatePerDay *float64 `json:"rate_per_day,omitempty"`
}

// Milestone is one high-level bead on the roadmap. Low-level beads are never
// listed; they appear only as IDs in RemainingIDs and CriticalChain.
type Milestone struct {
	ID    string   `json:"id"`
	Title string   `json:"title"`
	Repo  string   `json:"repo"`
	Type  string   `json:"type"`            // issue_type
	Goals []string `json:"goals,omitempty"` // goals named by its labels
	Outlook
}

// Goal is a cross-repo goal: the work of every bead labelled with it. Its
// forecast is the latest of its members' finishes in each simulated run.
type Goal struct {
	ID    string `json:"id"`
	Title string `json:"title"`          // the goal bead's title, or the ID when it is not loaded
	Repo  string `json:"repo,omitempty"` // the goal bead's repo when it is loaded
	// Members are the high-level beads labelled with the goal. Low-level
	// beads with the label count towards its work but are not listed.
	Members []string `json:"members"`
	Outlook
}

// Outlook is where a milestone or goal stands and when it is forecast to
// land. Graph facts (status, counts, target) are always set; the forecast
// fields are set by the forecaster and absent otherwise.
type Outlook struct {
	Status      string     `json:"status"`
	Schedule    string     `json:"schedule,omitempty"`
	TargetDueAt *time.Time `json:"target_due_at,omitempty"` // the bead's due_at
	ClosedAt    *time.Time `json:"closed_at,omitempty"`     // when Status is done

	// Scope counts: the descendants and open upstream blockers the bead
	// waits on (graph.Scope), without the bead itself. Total = Done +
	// Remaining.
	Total     int     `json:"total"`
	Done      int     `json:"done"`
	Remaining int     `json:"remaining"`
	DonePct   float64 `json:"done_pct"` // 100 × Done / Total, one decimal; 0 when Total is 0
	// RemainingIDs is the open work the forecast covers, sorted: calibration
	// needs it to tell a late finish from grown scope.
	RemainingIDs []string `json:"remaining_ids"`
	HumanGates   int      `json:"human_gates"` // remaining beads that wait on a human
	// WaitsOnUnloaded lists blockers that are in no loaded repo.
	WaitsOnUnloaded []string `json:"waits_on_unloaded,omitempty"`

	// Forecast finish dates, UTC.
	P50 *time.Time `json:"p50,omitempty"`
	P80 *time.Time `json:"p80,omitempty"`
	P95 *time.Time `json:"p95,omitempty"`
	// The median run's time to finish, split into agent time (queue and
	// cycle) and time waiting on human gates.
	AgentHours *float64 `json:"agent_hours,omitempty"`
	HumanHours *float64 `json:"human_hours,omitempty"`
	// Forecast inputs, shown with the dates: the pace (work beads closed per
	// day) and the agents working the scope at once.
	RatePerDay  *float64 `json:"rate_per_day,omitempty"`
	Concurrency *float64 `json:"concurrency,omitempty"`
	// CriticalChain is the chain of beads that set the date, first to last.
	CriticalChain []string `json:"critical_chain,omitempty"`
}

// Calibration reports how earlier forecasts held up.
type Calibration struct {
	Samples     int     `json:"samples"`      // closed milestones and goals scored against a snapshot
	P50Coverage float64 `json:"p50_coverage"` // share (0–1) that closed on or before their P50
	P80Coverage float64 `json:"p80_coverage"` // share (0–1) that closed on or before their P80
}

// Assess sets Schedule on every milestone and goal from its status, forecast
// and target, taking GeneratedAt as now. Build calls it; call it again after
// setting forecasts.
func (r *Roadmap) Assess() {
	for n := range r.Milestones {
		r.Milestones[n].assess(r.GeneratedAt)
	}
	for n := range r.Goals {
		r.Goals[n].assess(r.GeneratedAt)
	}
}

func (o *Outlook) assess(now time.Time) {
	o.Schedule = ""
	if o.TargetDueAt == nil {
		return
	}
	target := *o.TargetDueAt
	switch {
	case o.Status == StatusDone:
		o.Schedule = OnTrack
		if o.ClosedAt != nil && o.ClosedAt.After(target) {
			o.Schedule = Late
		}
	case o.P80 != nil && !o.P80.After(target):
		o.Schedule = OnTrack
	case o.P50 != nil && !o.P50.After(target):
		o.Schedule = AtRisk
	case o.P50 != nil, target.Before(now):
		o.Schedule = Late
	case o.Status == StatusReadyToClose:
		o.Schedule = OnTrack
	}
}

// Validate checks what a reader relies on: a known schema version and
// statuses, consistent counts and ordered quantiles.
func (r *Roadmap) Validate() error {
	var errs []error
	if r.SchemaVersion != SchemaVersion {
		errs = append(errs, fmt.Errorf("schema_version %d, want %d", r.SchemaVersion, SchemaVersion))
	}
	if r.GeneratedAt.IsZero() {
		errs = append(errs, errors.New("generated_at is missing"))
	}
	repos := map[string]bool{}
	for _, rp := range r.Repos {
		if rp.Name == "" || repos[rp.Name] {
			errs = append(errs, fmt.Errorf("repos: missing or duplicate name %q", rp.Name))
		}
		repos[rp.Name] = true
	}
	seen := map[string]bool{}
	for _, m := range r.Milestones {
		where := "milestone " + m.ID
		switch {
		case m.ID == "":
			errs = append(errs, errors.New("milestone without an id"))
		case seen[m.ID]:
			errs = append(errs, fmt.Errorf("%s: duplicate id", where))
		case !repos[m.Repo]:
			errs = append(errs, fmt.Errorf("%s: repo %q is not in repos", where, m.Repo))
		}
		seen[m.ID] = true
		errs = append(errs, m.Outlook.validate(where)...)
	}
	for _, g := range r.Goals {
		where := "goal " + g.ID
		switch {
		case g.ID == "":
			errs = append(errs, errors.New("goal without an id"))
		case seen[g.ID]:
			errs = append(errs, fmt.Errorf("%s: duplicate id", where))
		}
		seen[g.ID] = true
		errs = append(errs, g.Outlook.validate(where)...)
	}
	return errors.Join(errs...)
}

func (o *Outlook) validate(where string) []error {
	var errs []error
	known := false
	for _, s := range Statuses {
		known = known || o.Status == s
	}
	if !known {
		errs = append(errs, fmt.Errorf("%s: unknown status %q", where, o.Status))
	}
	switch o.Schedule {
	case "", OnTrack, AtRisk, Late:
	default:
		errs = append(errs, fmt.Errorf("%s: unknown schedule %q", where, o.Schedule))
	}
	if o.Total != o.Done+o.Remaining || o.Remaining != len(o.RemainingIDs) || o.Done < 0 {
		errs = append(errs, fmt.Errorf("%s: total %d, done %d, remaining %d with %d remaining_ids do not add up",
			where, o.Total, o.Done, o.Remaining, len(o.RemainingIDs)))
	}
	if (o.P50 != nil && o.P80 != nil && o.P80.Before(*o.P50)) || (o.P80 != nil && o.P95 != nil && o.P95.Before(*o.P80)) {
		errs = append(errs, fmt.Errorf("%s: quantiles out of order (p50 <= p80 <= p95)", where))
	}
	return errs
}

// Write validates r and writes it as indented JSON.
func Write(w io.Writer, r *Roadmap) error {
	if err := r.Validate(); err != nil {
		return fmt.Errorf("roadmap: %w", err)
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	return enc.Encode(r)
}

// WriteFile writes r to path through a temporary file in the same
// directory, so a reader (a web server, a dashboard) never sees half a file.
func WriteFile(path string, r *Roadmap) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".roadmap-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := Write(tmp, r); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// Read decodes and validates a roadmap. Unknown fields are ignored, so a
// roadmap from a newer beadline with additive changes still reads; a newer
// schema version is an error.
func Read(rd io.Reader) (*Roadmap, error) {
	var r Roadmap
	if err := json.NewDecoder(rd).Decode(&r); err != nil {
		return nil, fmt.Errorf("roadmap: %w", err)
	}
	switch {
	case r.SchemaVersion == 0:
		return nil, errors.New("roadmap: no schema_version; not a beadline roadmap.json")
	case r.SchemaVersion > SchemaVersion:
		return nil, fmt.Errorf("roadmap: schema_version %d is newer than this beadline reads (%d); upgrade beadline",
			r.SchemaVersion, SchemaVersion)
	}
	if err := r.Validate(); err != nil {
		return nil, fmt.Errorf("roadmap: %w", err)
	}
	return &r, nil
}

// ReadFile reads and validates the roadmap at path.
func ReadFile(path string) (*Roadmap, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	r, err := Read(f)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return r, nil
}
