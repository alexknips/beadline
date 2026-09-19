# beadline — design

## Problem
Teams whose work is executed by AI coding agents track that work in beads. They can see *what* is open,
but not *when* the high-level things (milestones, epics, goals) will land — and every existing answer
either asks a human to type dates or scales effort for human developers.

## Assumptions (explicit)
1. **Beads are executed by AI agents**, orchestrated by something (Gas City, a herdr loop, a cron of
   `claude -p`, a human pasting prompts). beadline does not care which; it measures the outcome.
2. **Humans are gates, not executors.** Approval, design sign-off and decisions add latency; they are
   modelled as such.
3. **The graph is the plan.** Milestones/epics with parent-child and blocking dependencies exist; the
   roadmap is a projection of that graph, never a separate document.

## Inputs
- One or more beads sources. v0: `bd export` JSONL files (universal across Dolt/SQLite backends);
  later: live `bd … --json` and a Dolt reader. *Superseded by ADR-2 §5:* a repo directory is read live
  through `bd -C DIR --readonly export`, and a JSONL file still works.
- `beadline.toml`:
  ```toml
  [[repos]]
  name = "api"          # display name
  export = "api/.beads/issues.jsonl"
  concurrency = 4       # max agents working this repo at once (or "measure")

  [conventions]
  high_level_types = ["milestone", "epic"]
  goal_label_pattern = "^goal:(?P<id>.+)$"      # cross-repo: product bead -> goal bead
  human_gate_title_patterns = ["^HUMAN:", "^ALEX:"]
  human_gate_metadata_keys = ["awaiting_signoff", "plan_first", "hold_reason"]
  infra_types = ["molecule", "wisp", "step", "convoy", "message", "gate", "event"]

  [model]
  window_days = 45
  simulations = 2000
  seed = 1                      # same inputs + same seed = same roadmap
  human_gate_hours_prior = 24
  cycle_minutes_prior = 60      # median of the root prior when there is no history at all
  pooling_strength = 10         # k: pseudo-observations a class borrows from its parent (ADR-1 §2)
  tail_cap_factor = 3           # samples capped at factor × longest observed duration (ADR-1 §2)
  ```
  Every key but `[[repos]]` is optional. The defaults are the values shown, except that
  `human_gate_title_patterns` defaults to `["^HUMAN:"]` and `concurrency` to `"measure"`. An unknown
  key is an error, so a typo cannot silently fall back to a default. *Superseded by ADR-2 §5:* the
  config is `repos` plus an `[expert]` table, and one repo needs none (see "Command line and
  beadline.toml").

### Loading rules (bl-ya5.2)
- **One graph across repos.** Exports are read in config order. bd prefixes keep IDs unique, so
  edges and goal labels cross repositories freely. A second copy of an ID is reported and ignored.
- **Edges.** `parent-child` gives the hierarchy, with the dependent as the child. `blocks`,
  `conditional-blocks` and `waits-for` become blocking edges, because bd treats exactly these as hard
  blockers. Other types (`related`, `discovered-from`, `tracks`, ...) carry no scheduling meaning and
  are ignored.
- **Not work.** Records whose type is in `infra_types` are dropped, as are ephemeral records (wisps)
  and templates. Every edge to a dropped record goes with it. Non-issue records, such as memories,
  are skipped.
- **Human gate.** A bead is a gate when its title matches a gate pattern or a gate metadata key is
  set to anything but `""`, `false`, `0` or `null`. A cleared hold is often left behind as `""`.
- **Goal.** Every label that matches `goal_label_pattern` names a goal. The goal's own bead is
  optional, because it often lives in a coordination repo that is not forecast.
- **Reported, not fatal.** The loader reports dependencies on IDs that no loaded repo has, duplicate
  IDs, and cycles (strongly connected components) of blocking or of parent-child edges. `beadline
  doctor` prints them (the loader's former `check`, renamed by ADR-2 §5). It exits 1 only for a cycle
  among open beads, since no schedule can satisfy one. Malformed JSON, by contrast, is fatal, and the
  error names the file and line.

## Model
### 1. Estimator (per bead, invisible)
- Class = (repo, issue_type, size bucket). Size proxy: description length, child count, labels. A
  `size:s|m|l` label decides (`xs`, `xl` count as `s`, `l`). Otherwise a bead is **L** with a
  description of 1,500+ characters or 3+ children, **M** with 400+ characters or any child, else **S**.
  *Superseded by ADR-2 §1:* class = (repo, issue_type, priority); size buckets are dropped.
- From closed beads in `window_days`: empirical distribution of **cycle time** (`started_at → closed_at`,
  fallback `created_at → closed_at`) and **queue latency** (`created_at → started_at`). Only work
  beads count: the caller leaves out infra and high-level types, whose durations span other work.
  Queue latency has no prior of its own and uses `cycle_minutes_prior`. *Superseded by ADR-2 §1:*
  queue latency runs from `ready_at`, a bead without `started_at` draws one `ready_at → closed_at` lead
  time, still-open beads enter as censored observations, and every open bead's draw is conditioned on
  its age.
- Unseen class → repo prior → global prior. Fat tails are kept, never `median × count`: samples come
  from a smoothed bootstrap with hierarchical backoff and a tail cap (ADR-1 §2).
- Optional write-back, off unless a command-line flag asks for it: `estimated_minutes` (P50) and
  `ai_est_p80_minutes` of the full cycle time on open beads via `bd update`, so other tools
  (e.g. `bv --robot-capacity`) get agent-scale numbers. Beads that already carry the same numbers are
  skipped, so a repeated run writes nothing.

### 2. Forecaster (per high-level bead)
- Collect remaining descendants via parent-child edges; blocking edges define order. *Refined by
  ADR-2 §3:* the scope also takes in the upstream blocking closure.
- Monte Carlo: for each run, agents pull ready beads (respecting `concurrency` per repo), each bead
  waits its sampled queue latency, then its sampled cycle time; children become ready when blockers
  close. Milestone finish = last descendant closed. *Superseded in part by ADR-2 §1:* the measured
  concurrency no longer drives the dates.
- Human gates: beads matching the gate patterns add a sampled human latency (prior from config,
  learned from history once available) *before* they can start.
- Output per high-level bead: P50 / P80 dates, split into agent time and human-gate time, remaining
  count, % done (closed descendants / total), and the critical chain that determined the date.
- Cross-repo goals: a goal's forecast is the max over the high-level beads carrying its
  `goal:<id>` label.

#### Simulation rules (bl-ya5.4)
- **Scope.** An item (a high-level bead, or a goal) waits for its descendants, open or closed, and,
  transitively, for every open bead that it or its open work is blocked by, together with that
  blocker's open descendants. Closed blockers are satisfied and left out. For a goal the members
  and their scopes count. `total`, `closed`, `done_pct` and the remaining-bead set come from the
  scope, by the same rule as `roadmap.json`.
- **One schedule for everything.** Each run schedules every open bead of every loaded repo, not
  one item at a time: items compete for the same agents, as they do in reality. Beads outside any
  roadmap item take agents too. An item finishes, in that run, when the last bead of its scope
  finishes; a goal therefore finishes with its last member, run by run.
- **Readiness.** A bead is ready when its open blockers are done, and a container (a bead with
  children, a high-level bead, or a goal's own bead) when its open children are too. A child also
  waits for its ancestors' blockers, as bd does not offer the children of a blocked epic as ready
  work; a blocker that sits under that same ancestor is not inherited, so an epic blocked by its own
  child does not deadlock. `defer_until` in the future holds a bead back until then.
- **Work.** A ready work bead waits its queue latency, then for a free agent in its repo; among
  eligible beads the lowest priority number goes first, then the oldest. Beads already
  `in_progress` hold their agent from the start, ignore their blockers, and draw only their
  remaining time (ADR-1 §2).
- **Concurrency.** A configured number is a hard limit. `"measure"` takes the most work beads that
  were in progress at once in the window (`started_at` to `closed_at`), at least 1. *Superseded in
  part by ADR-2 §1:* the measured value is still reported, but it no longer drives the dates.
- **Human gates.** A gate is a wait, not work: it holds no agent. A gate leaf is done after its human
  wait; a container gate (e.g. an epic awaiting sign-off) waits after its children. The lag is learned
  from the gates closed in the window, from ready (the last blocker or child closed) to close, with the
  same smoothed bootstrap and `human_gate_hours_prior` as its root prior. A gate that is already ready
  draws the rest of a lag longer than what it has waited. Gates, containers and goal beads never teach
  the estimator: sign-off time does not leak into agent cycle time.
- **Parked and stuck.** A bead with status `deferred` and no future `defer_until` is parked: left out of
  the schedule. A container does not wait for a parked child. A bead blocked by a parked bead, or in a
  blocking cycle, is stuck: it can never start.
- **Status per item.** `deferred` (the item is deferred, or all of its remaining work is parked),
  `not_planned` (empty scope), `ready_to_close` (all of the scope closed, the item open), `stalled` (some
  of its work is stuck), `blocked_outside` (some of its work is blocked by a bead no loaded repo has),
  else `forecast`. Only `forecast`, `blocked_outside` and `stalled` carry dates; for the last two the dates
  cover the loaded, schedulable work and are a lower bound.
- **Output.** P50, P80 and P95 are the nearest-rank runs, so every date is the outcome of an actual run.
  Each is split into agent hours (queue, waiting for an agent, work) and human hours (gates, and waiting
  for a `defer_until` a person set) along the chain that set it; the two add up to the time from now.
  The critical chain is that of the P80 run: from the last bead to finish, each step is the predecessor
  whose finish made the bead ready. Also reported: the share of runs that finish by `due_at`.
- **Determinism.** Run *n* draws from a PCG seeded with (`seed`, *n*) and draws every duration before
  scheduling, so the result does not depend on the number of goroutines, and two configurations that
  differ only in concurrency see the same durations (common random numbers).
- **Properties under test.** In every run the schedule respects dependencies and agent limits, unlimited
  agents give exactly the longest path, and a blocking chain sets a floor. More agents never make a
  reported quantile later. Within a single run they can, rarely: greedy list scheduling admits Graham's
  anomalies (one more agent reorders work so that a bead finishes later, in about 0.2 % of paired runs on
  random graphs), so the guarantee is on the quantiles.
- **CLI.** `beadline forecast` prints the table, or with `-json` the forecast; `-now`, `-runs` and `-seed`
  override the defaults, and `-write-back` applies the estimator's write-back (off by default). It needs
  each export at `<repo>/.beads/<file>` and runs `bd` there with `BEADS_DIR` pinned to that directory.
  `-record` also records the forecast as a snapshot for calibration (§3).
  *Superseded by ADR-2 §5:* the default command `beadline [DIR|FILE ...]` forecasts, and `forecast`
  is hidden.

### 3. Calibration (bl-ya5.6)
A forecast is only worth what its track record says. `internal/calibrate` records forecasts, grades
them once reality has moved on, and replays history to test the model before anyone trusts it.
Both paths are graded and reported the same way. This implements ADR-2 §6, with two refinements
recorded here: every run of `beadline` records a snapshot (bl-ya5.11), as the hidden
`forecast -record` does; and besides descoped and vanished, a void forecast can be parked or
undated.

- **Snapshots.** `beadline` (and `beadline forecast -record`) also writes the forecast as an immutable snapshot,
  `beadline.snapshot/v1`, into `.beadline/snapshots/` beside beadline.toml (or `-snapshots DIR`).
  - A snapshot records `generated_at` and `as_of` (the forecast's now, which the quantiles count
    from). It also records `beadline_version` and `beadline_commit`, `model_version` (`adr-1`
    until bl-ya5.12 moves the model to ADR-2), the seed and runs, the config echo with its
    `config_sha256`, and `inputs` (each export's SHA-256 and the fingerprint, as in roadmap.json).
  - `items[]` holds every open high-level bead and goal. Each has its status, `descendant_ids` (the
    breakdown at as_of, members included for a goal), `remaining_ids` and `assumptions` (what the
    dates leave out). A dated item also has P50, P80 and P95 as dates and `quantile_hours`: the grid
    p05, p10, p20, p25, p30, p40, p50, p60, p70, p75, p80, p90, p95 and p99, in hours after as_of.
  - `leaves[]` holds the same grid for every open leaf the schedule covers. A leaf is a bead with no
    open child that is neither high-level nor a goal's bead. Parked and stuck beads have no forecast.
  - `content_sha256` is the SHA-256 of the JSON without it: its top-level members sorted by key,
    compact. A snapshot is written through a temporary file and a hard link, so it is create-only
    and read-only (0444), and it is named `<as_of>_<hash>.json`.
  - One snapshot a UTC day per model version and configuration is recorded. Later runs that day
    keep the first, so a forecast run hourly does not outweigh the days it ran once.
  - `-record` refuses `-now`: a snapshot must not be dated before the data it was made from.
- **Grading: `beadline check`.** It reads the snapshots and skips, with a warning, any file whose
  hash fails, whose schema is newer or whose layout is not this one. It grades every forecast
  against the exports as they are now. Their horizon, the latest timestamp in any record, stands
  for now. Each forecast gets an outcome:
  - **resolved**: the bead closed; the actual time is `closed_at − as_of`.
  - **open**: right-censored. A quantile is a known miss once the time since as_of passes it, and
    pending before that.
  - **void**, which is not scored:
    - `descoped`: closed without delivery. Its close reason starts with duplicate, superseded, won't
      fix, obsolete, not needed, not planned, moot, abandoned, descoped, out of scope, closing stale
      or bulk close. Or `gc.work_outcome` is `no-op` or `abandoned`. Or it closed with 8 or more
      beads of its repo in the same minute, a bulk close.
    - `parked`: still open and deferred, or snoozed past the horizon. That is a decision, not a
      miss. It also covers beads imported with a backdated `created_at`. On the town's repos these
      are the 50 `recovered-br-shadow` beads, which would otherwise look open since June.
    - `vanished`: the bead is gone from the exports.
    - `undated`: closed with no `closed_at`.
- **Scores.** Leaf beads come first because they are the honest sample: many beads, each forecast
  on its own. High-level items come second.
  - **Coverage.** For P50, P80 and P95 the report shows how many held out of the known outcomes, the
    share, and its bounds once pending forecasts resolve (all late, or all in time).
  - **Interval.** A 90% interval of the share resamples targets, not forecasts, because one bead
    forecast at several moments is not independent evidence.
  - **Stretch.** The factor 2^(k/6) by which a quantile would have had to stretch to hold at its
    nominal level. Above 1 the intervals are too narrow; below 1, too wide. It says how far to widen
    or narrow them. Nothing is re-tuned automatically.
  - **Errors, on resolved forecasts.** The bias is the median of P50 − actual (negative means
    forecasts too early). The report also gives the median |P50 − actual| and the CRPS from the
    grid: twice the pinball loss integrated over the levels, each level standing for the levels
    halfway to its neighbours. The PIT is counted in fifths.
  - **Too few targets.** Fewer than 20 distinct targets are flagged as indicative, not something
    to tune on.
  - **Scope.** High-level forecasts are also scored apart by scope. Fixed scope means the breakdown
    is unchanged since as_of. Changed scope means beads were added or removed, which tells scope
    growth from model error.
- **Backtest: `beadline check -backtest 60d [-step 3.5d] [-runs N]`.** A rolling-origin replay
  from one set of exports.
  - **Origins.** From midnight (UTC) on or before horizon − span, every step, up to a week before
    the horizon, so outcomes have time to arrive.
  - **Rewind.** At each origin `load.Exports.Graph(asOf)` rewinds the exports:
    - Beads and dependencies created later are left out, and closes and starts after the origin
      are undone.
    - A bead not updated since keeps its status. A bead updated since is open, or `in_progress` if
      it had started, with no `defer_until`, because its status then is unknown.
    - Other fields (title, priority, labels, metadata) are taken as they are now.
    - **Dependency skew.** Dependency timestamps can be skewed: bd stored some in the server's local
      time and exported them as UTC. A whole-hour lag behind the later bead, shared by at least 20
      dependencies and a tenth of those created since, is taken as that skew and corrected. On the
      town's repos it is 2 hours from 2026-06-15.
  - **Forecast and grading.** The forecast is the one `beadline forecast` makes, learning only
    from the rewound graph, and it is graded like a snapshot.
  - **Throughput baseline.** A baseline is scored alongside. It bootstraps the repo's daily
    delivered closes over the model window until as many beads have closed as remain: 1 for a
    leaf, the scheduled remaining work for an item. A goal counts all repos. It knows nothing of the
    graph, of age or of priority, which is the point of comparing with it.
  - **Not yet read:** status at a past moment from the Dolt events table. That would sharpen the
    rewind of beads updated since the origin.
- **Release gate (ADR-2, D3).** The backtest ends with `release gate (leaf P80 held 70%-90%): PASS`
  or `FAIL`, with the reason for a failure. Fewer than 20 distinct leaf beads fail it. The exit
  status stays 0, and the JSON (`-json`) carries `gate.pass` and every graded forecast.

### 4. Cost (optional adapter)
- Given token usage per bead or per repo-week (adapters for orchestrator logs), forecast cost P50/P80
  alongside dates.

## Outputs
- `roadmap.json`: `{generated_at, repos[], goals[], milestones[{id, title, repo, p50, p80, agent_hours,
  human_hours, remaining, done_pct, critical_chain[], target_due_at?, status}], calibration{}}`.
- `roadmap.html`: single file, no server, dark/light, table view for accessibility; swimlane per repo +
  goals lane; bars to P80 with P50 marker, target marker if `due_at` is set.

### roadmap.json, schema version 1 (bl-ya5.5)
`internal/roadmap` defines the file. It is the contract between forecast, render and calibrate. Each
run's roadmap.json is also the immutable **snapshot** that calibration later scores against the
beads that actually closed, so it records exactly what went in.
- **Versioning.** `schema_version` is 1. Adding a field keeps the version. Renaming or removing a
  field, or changing what one means, bumps it. A reader ignores unknown fields and rejects a newer
  version instead of misreading it.
- **Provenance.** It records `generated_at` (the run's "now", UTC), `beadline_version` and
  `model_version` (the forecasting model revision, such as `adr-1`, and absent when nothing was
  forecast). It also records `config`, beadline.toml as used with defaults filled in, seed included.
  `inputs.exports[]` gives each export's repo, path, SHA-256 and size, with `live` when it was read
  with `bd export` from a repository directory, and `inputs.fingerprint` hashes those in config
  order. Same fingerprint, config and seed give the same roadmap. `config.hide` lists the beads left
  off the roadmap, and `config.settings` the settings that differ from the defaults, in
  beadline.toml syntax without `expert.` (bl-ya5.11).
- **`repos[]`** are the swimlanes, in config order. Each has bead counts, `concurrency` and
  `concurrency_source` (`configured` or `measured`), and `rate_per_day` once the forecaster measures
  it. A repo that could not be read has `error`, and no beads (bl-ya5.11).
- **`milestones[]`** holds every open high-level bead, plus those that closed within `window_days`.
  A bead that a goal label names appears as that goal instead. **`goals[]`** holds every goal named
  by a label, with its bead's title when that bead is loaded and `members[]`, its labelled high-level
  beads. Both share these fields:
  - **Scope** (`graph.Scope`) covers the parent-child descendants, open or closed, plus every
    blocker of the bead or of open work in scope, transitively, open or closed, with that blocker's
    descendants. Blocking edges are followed only from open beads, because what a closed bead
    waited on is history. The open beads in scope are therefore exactly the work still to land.
    On the town's exports this counts 17 beads for hivemind-m306, which has 8 children, and 3 for
    M6 (hivemind-zdsh), which has 1 child and two closed blockers. A goal's scope is the union over
    its labelled beads. `total = done + remaining`, and `done_pct` is `100 × done / total`.
    `remaining_ids[]` is the exact open set the forecast covers, so calibration can tell a late
    finish from scope that grew. `human_gates` counts the remaining gates. `waits_on_unloaded[]`
    lists blockers that are in no loaded repo.
  - **`status`** is one of `done`, `forecast`, `ready_to_close`, `not_planned`, `deferred`,
    `blocked_outside` or `stalled`. `done` means closed; a goal whose bead is not loaded is done when
    all its work is. `forecast` means open work remains. `ready_to_close` means all the work is
    closed but the bead is still open. `not_planned` means there is no work under it at all.
    `deferred` comes from the bd status or a future `defer_until`. `blocked_outside` means something
    waits on an unloaded repo. `stalled` is set only by the forecaster.
  - **Forecast fields** are set by the forecaster and absent otherwise. They are `p50`, `p80` and
    `p95` (finish dates, UTC), and `agent_hours` and `human_hours`, which split the median run into
    agent time and time waiting on human gates. They also carry the inputs shown with the dates,
    `rate_per_day` and `concurrency`, and `critical_chain[]`.
  - **`schedule`** compares the forecast with `target_due_at` (the bead's `due_at`). It is `on_track`
    when P80 falls on or before the target, `at_risk` when only P50 does, and `late` when P50 falls
    after it or the target has passed. It is absent when there is no target, or no forecast yet to
    compare with. `Roadmap.Assess` computes it.
- **`calibration`** holds `samples`, `p50_coverage` and `p80_coverage`. `beadline` fills it by
  grading the recorded snapshots as `beadline check` does (§3): `samples` counts the milestone and
  goal forecasts whose 80% outcome is known, and the coverages are the shares that held. It is
  absent before any outcome is known, and with `--as-of` (bl-ya5.11). The page and the summary show
  it as the track record, "80% dates held X of Y".

`roadmap.Build` fills everything the graph alone determines. `Roadmap.SetForecast` then copies the
forecaster's status (which knows `stalled` and parked work), dates, split of the median run and
critical chain, and each repo's simulated agents and pace, and calls `Assess` again (bl-ya5.11).

### roadmap.html (bl-ya5.5)
`beadline` writes the page next to roadmap.json; the hidden `beadline render --in roadmap.json --out
roadmap.html [--title ...]` renders an existing roadmap.json. Either replaces the old file
atomically, so a web server never serves half a page. The page follows ADR-1 §3:
- **Timeline.** Inline SVG with a goals lane, then one lane per repo. Each open milestone or goal is
  a row. Its bar runs from now to P80, coloured by schedule, with a tick at P50, a whisker on to P95
  and a diamond at the target. A target that has already passed sits at the left edge in red. A row
  with no forecast shows its status instead of a bar. Done items appear only in the table and in
  the lane counts. Low-level beads never get a row.
- **Hover details** show the dates, the target and schedule, remaining work with done % and human
  gates, pace (beads/day) and agents, agent and human time, the critical chain's length, and any
  waits on unloaded repos. They come from SVG `<title>` elements, which the script turns into a
  popover that also opens on keyboard focus.
- **Table.** The same rows grouped by lane, done items included.
- **Wording (bl-ya5.11).** The page says "plan for <date> (80% chance)" and "50/50: <date>": the
  table's columns, the legend and the first line of each tooltip. P50, P80 and P95 appear only in
  the JSON and on a second tooltip line for experts. A banner lists the repos that could not be
  read, whose lanes say so, and the footer lists the non-default settings (`config.settings`), or
  "Default settings.".
- **Self-contained.** The page has no `src` or `href` attributes and no links. It makes no network
  requests. A `Content-Security-Policy` of `default-src 'none'` allows only the page's own inline
  style and script, by SHA-256. So the page works from `file://`, from any directory of a static
  server, and next to other pages. `roadmap.json` is embedded as `<script type="application/json"
  id="roadmap-data">`.
- **Script.** Under 10 KB and optional. It adds a timeline/table toggle, a repo filter that re-stacks
  the lanes (goal rows follow their members' repos), a theme toggle (auto, light or dark) and the
  popover. It saves preferences in `localStorage` when that is available. Without it, both views
  show and the theme follows the system.

## Command line and beadline.toml (bl-ya5.11)
The surface of ADR-2 §5. `internal/cli` implements it; `beadline help` and `beadline help COMMAND`
print it.

```
beadline [PATH...] [flags]   forecast; write roadmap.html and roadmap.json; record a snapshot
beadline check [PATH...]     grade past forecasts (--backtest SPAN replays history; Calibration §3)
beadline doctor [PATH...]    check the data: cycles, dangling dependencies, duplicate IDs
beadline version             also --version and -v
beadline help [COMMAND]
```

- **Repos.** A PATH is a repository directory with `.beads` (or the `.beads` directory itself), read
  live with `bd -C DIR --readonly export`, or a `bd export` JSONL file. Directories are read in
  parallel, with `BEADS_DIR` pinned to `DIR/.beads` so an inherited one cannot swap the database, and
  with a 5-minute limit. Without PATH, beadline reads the repos listed in `beadline.toml`, else the
  current directory when it holds `.beads`. The bead says "`.` if it has .beads, else the file's
  repos"; the order is reversed because a file that lists repos is the more explicit of the two,
  and a file with only `[expert]` still gets `.`. A repo is named after its directory, or its file's
  name without the extension (`hivemind/.beads/issues.jsonl` is `hivemind`); a second repo with the
  same name gets `-2`. Paths on the command line are relative to the working directory, paths in the
  file to the file's directory. A first argument that names a command runs it; `./check` is a path.
- **Outputs.** `-o DIR` (default `.`) writes `DIR/roadmap.html` and `DIR/roadmap.json`, creating
  DIR. `-o FILE.html` or `-o FILE.json` names one file, and the other goes beside it with the same
  stem. Both are written atomically. The snapshot (Calibration §3) goes to `.beadline/snapshots/`
  beside beadline.toml, else in the working directory, where `beadline check` looks. It is not
  recorded with `--no-record`, with `--as-of` (a snapshot must not be dated before its data), or when
  a repo could not be read (it would miss that repo's work).
- **Summary.** stdout gets the answer, soonest plan date first: each dated item as "plan for <date>
  (80% chance) · 50/50: <date> · k of n left", then "waits on a person" when a human gate remains,
  the target and schedule when `due_at` is set, and the caveat of a stalled or blocked item. The
  items without a date follow, grouped by why: ready to close, stalled, waiting on repos that are not
  loaded, not planned, deferred. stderr gets what was written and recorded, and a pointer to
  `beadline doctor` when the data has problems. `--json` prints roadmap.json instead, `--explain ID`
  prints what one item's dates rest on (scope, agent and human time, agents and pace, the critical
  chain and the work left with each bead's expected time) or one bead's estimate, and `-q` prints
  only errors and warnings. Nothing says "almost surely".
- **Flags.** `-o/--out`, `-c/--config FILE` (default `./beadline.toml` when present), `--no-record`,
  `--json`, `-q/--quiet`, `--explain ID`, `-h/--help`, `-v/--version`. Expert: `--as-of DATE`
  (`2026-09-19` or RFC 3339: the forecast from the data as it stood then, rewound as the backtest
  rewinds it, Calibration §3), `--seed N`, `--runs N`, `--agents REPO=N` (or `REPO=measure`;
  repeatable and comma-separated), `--bd PATH`. Flags may come before, between or after the paths;
  `--` ends them.
- **Exit codes** (ADR-1 §4). 0 ok. 1 failure, including a repo that could not be read or parsed:
  the others still load, the page is still written with a banner, and stderr names the repo. When no
  repo can be read, nothing is written. 2 usage error: an unknown flag, a stray argument, a PATH that
  is neither a repository nor a file, or no repos at all. `-h` prints the command's help to stdout
  and exits 0 on every command. `check` exits 1 rather than grade against a partial read, since a
  forecast of a missing repo's work would look descoped.
- **Version.** The linker-set version (`make build` stamps `git describe`), else the module version
  of `go install ...@vX.Y.Z`, else `dev-<revision>` from the VCS stamp, else `dev`.
- **Hidden and dropped.** `forecast` (the table, `--write-back`, `--record`) and `render` still work
  and are not listed. `serve` is gone.
- **beadline.toml.** Needed only for more than one repo or a non-default setting:
  ```toml
  repos = ["../hivemind", "../beadline", "exports/web.jsonl"]

  [expert]
  agents = { hivemind = 4 }         # agents per repo; default "measure" (from history)
  roadmap_types = ["milestone", "epic"]
  goal_label = "^goal:(?P<id>.+)$"
  human_gate = { titles = ["^HUMAN:"], metadata = ["awaiting_signoff", "plan_first", "hold_reason"], hours_prior = 24 }
  hide = []                         # high-level beads and goals to leave off the roadmap
  history_days = 45
  ignore_types = ["molecule", "wisp", "step", "convoy", "message", "gate", "event"]
  runs = 2000
  seed = 1
  cycle_minutes_prior = 60
  pooling_strength = 10
  tail_cap_factor = 3
  ```
  The values shown for the expert keys are the defaults. They map onto the layout under Inputs:
  `roadmap_types` is `high_level_types`, `goal_label` is `goal_label_pattern`, `human_gate.titles`,
  `.metadata` and `.hours_prior` are the gate patterns, keys and prior, `history_days` is
  `window_days`, `ignore_types` is `infra_types`, `runs` is `simulations`, and `agents` replaces
  each repo's `concurrency`. An unknown key is an error that names the closest valid key ("unknown
  key expert.agent; did you mean expert.agents?"), or the new name of a key of the old layout; an
  old `[[repos]]` table says how to write `repos` now. `agents` for a repo that is not loaded is a
  warning, since the command line may load fewer repos than the file lists.

## Relationship to beads_viewer (`bv`)
`bv` (Dicklesworthstone/beads_viewer) already computes several things beadline needs: dependency graph
construction and cycle detection, critical path, dependency-respecting execution "waves"
(`--robot-plan`), serial-vs-parallel capacity (`--robot-capacity`), git-history time travel
(`--robot-diff --diff-since`), and an interactive graph HTML export. Its Go packages are public
(`pkg/analysis`, `pkg/loader`, `pkg/export`).

**Decision: beadline takes no code from `bv`.** `bv` is licensed "MIT with an OpenAI/Anthropic rider":
derivative works must carry a rider that grants no rights to those companies or anyone acting on their
behalf, and forbids use in ML/automated pipelines. Importing, vendoring or forking `bv` would therefore
make beadline non-open-source in the ordinary sense and is incompatible with beadline's plain MIT
license and with how beadline is built (by AI agents). Consequently:

- **Clean room.** beadline implements its own graph loading, critical path, topological waves and
  scheduling simulation from first principles (they are textbook algorithms). Contributors must not
  copy or closely paraphrase `bv` source.
- **Optional integration by CLI only.** If a user has `bv` installed, beadline may run it as an external
  tool and consume its robot JSON (critical path, plan, history/diff) for cross-checks and extra views —
  on an *enriched* JSONL copy where beadline has filled `estimated_minutes` from its estimator, so
  `bv`'s capacity numbers come out at agent scale. beadline never bundles or depends on `bv`.
- **Language choice stands on its own merits**, not on `bv` reuse.

## Non-goals
- Not an issue tracker or a board (bd, bv, beads-ui do that). No writes except the optional
  `estimated_minutes` write-back.
- No low-level bead display.

## Decisions — ADR-1 (bl-ya5.1)
Status: **Accepted (Alex, 2026-09-19)**. The implementation beads (bl-ya5.2 to bl-ya5.8) build on
these decisions. **Partly superseded by ADR-2** (§2 classes, conditioning and censoring; `serve` and the
subcommands of §4), as marked below.

### 1. Language and distribution: Go, one static binary
- **Decision.** Go (go.mod pins the minimum, currently 1.22), built with `CGO_ENABLED=0`. Users install
  it with `go install github.com/alexknips/beadline/cmd/beadline@latest` or download a release binary.
  Like `bd`, there is no runtime to install.
- **Why.** The standard library covers all of v0.1. `encoding/json` reads the JSONL exports.
  `html/template` with `embed` builds the single-file page and escapes untrusted bead titles by context.
  `net/http` backs `serve` (dropped by ADR-2 §5), and `os/exec` runs live `bd` and the optional `bv`.
  `math/rand/v2` gives a seeded PCG, so a forecast is reproducible. Goroutines run simulations in
  parallel. 2,000 runs over a few thousand beads is a plain CPU loop that Go finishes in well under a
  second. For statistics we need sampling, quantiles and a bandwidth rule, roughly 150 lines, so scipy
  is not needed.
- **Rejected.** *Python*: numpy and scipy are attractive, but every user would need a Python
  environment, and packaging undercuts the "drop one binary into CI" story. *TypeScript*: it is the
  same language as the page, but shipping it needs Node or a compile step, and the page needs no
  framework (§3).
- **Clean room, enforced.** `bv` is also Go, which makes an accidental import easy. So `make check`
  fails if any module in the module graph matches `beads_viewer`. Standard library first. The only
  planned third-party module is `github.com/BurntSushi/toml` (MIT) for `beadline.toml`. Any new
  dependency needs an OSI-approved licence with no field-of-use restriction.

### 2. Duration distributions: smoothed bootstrap with hierarchical backoff
The same representation covers cycle time, queue latency and human-gate latency.
- **Observations.** Durations in minutes, floored at 1 minute (beads can close in the second they
  open) and stored as logs.
- **Backoff chain.** class (repo × type × size bucket) → repo → all repos → root prior. A node with
  `n` observations draws from its own data with probability `n / (n + k)` and from its parent
  otherwise. `k` is `pooling_strength`, default 10. A class with no history is pure repo data, one with
  10 observations is half its own, and one with 100 is 91 % its own. *Superseded in part by ADR-2
  §1:* the class is repo × type × priority; the size bucket is dropped.
- **Own-data draw.** Pick an observation uniformly and add Gaussian noise in log space. The bandwidth
  `h` follows Silverman's rule, `0.9 · min(sd, IQR/1.34) · n^(-1/5)`, with a minimum of 0.1. This keeps
  multi-modality, such as a clean pass versus a rejection-and-retry loop. Percentiles come out smooth
  rather than stepped, and draws can modestly exceed the largest observation.
- **Root prior.** A log-normal with median `cycle_minutes_prior` (for human gates,
  `human_gate_hours_prior`) and σ = 1. With these values P80 ≈ 2.3 × median and P99 ≈ 10 × median.
- **Tail cap.** Every draw is clamped to `tail_cap_factor` (default 3) × the longest observation of
  that quantity across all repos in the window. With no history the cap is the prior's P99. The cap
  stops one wild draw from the prior from setting a long chain's P80. Clamping rather than rejecting
  keeps the number of random draws, and therefore seeded runs, stable.
- **In-progress beads.** Draw `d` conditioned on `d > elapsed` by rejection, up to 64 tries. The
  remaining time is `d − elapsed`. If every try is rejected, the bead is already beyond almost all
  history, so the remaining time is a fresh unconditioned draw. Started beads get no queue latency.
  *Superseded by ADR-2 §1:* every open bead is conditioned on its age, and when every try is rejected
  the fallback is Lindy (about as long again), not a fresh draw.
- **Write-back numbers.** P50/P80 of 4,000 draws with the configured seed, from the same sampler the
  simulation uses, so the two always agree.
- **Rejected.** *Pure empirical bootstrap*: it can never exceed the observed maximum, its percentiles
  are stepped, and it has nothing to offer a thin class. *Fitted log-normal*: it forces one mode onto
  data that is often bimodal, the fit is sensitive to outliers, and the tail cap ends up doing all the
  work. The log-normal survives only as the root prior.
- **Known bias.** Beads still open are missing from the closed-bead window (right-censoring), which
  makes estimates optimistic. v0.1 accepts this. Calibration (bl-ya5.6) measures coverage and widens
  intervals. A Kaplan–Meier correction follows if coverage stays low. *Superseded by ADR-2 §1:*
  Kaplan–Meier censoring is in v0.1.

### 3. HTML stack: server-rendered, zero-dependency single file
- **Decision.** Go `html/template` renders one `roadmap.html` with everything inline: CSS, a small
  vanilla-JS enhancement script (budget ≤ 10 KB), and the timeline as inline SVG computed in Go. Each
  bar runs from now to P80, with a tick at P50 and a diamond at the `due_at` target.
- **Works without JavaScript.** The swimlane SVG and an accessible `<table>` of the same data are both
  in the markup. SVG `<title>` elements give native hover details. JavaScript adds only a richer
  hover and focus popover, the timeline/table toggle and a repo filter.
- **Self-describing.** `roadmap.json` is embedded as `<script type="application/json"
  id="roadmap-data">`, so scripts never re-derive data and the JSON can be recovered from the page.
- **Themes.** CSS custom properties follow `prefers-color-scheme`, and a toggle can override them.
  The page uses the system font stack.
- **No network requests.** No CDN, web fonts or analytics. The page works from `file://`, GitHub Pages
  or a CI artifact, and under a strict CSP.
- **Tests.** Golden-file tests of the page rendered from a fixture roadmap, plus a check that the
  output references no external URL.
- **Rejected.** *React, Svelte or Vue*: a Node build inside a Go repo for a read-only page. *Chart.js,
  D3, Mermaid or frappe-gantt from a CDN*: this breaks offline and `file://` use, and inlining them
  costs 100–300 KB plus a licence review per library. *Client-side rendering from JSON*: blank without
  JavaScript, and not testable from Go.

### 4. Repository skeleton
| Path | Responsibility | Bead |
|---|---|---|
| `cmd/beadline` | `main`: passes `os.Args` and the standard streams to `internal/cli` | bl-ya5.1 |
| `internal/cli` | subcommands (`forecast`, `check`, `doctor`, `render`, `serve`, `version`; *superseded by ADR-2 §5*: the one command, `check`, `doctor`, `version`, `help`), flags, exit codes | bl-ya5.1, then each feature bead |
| `internal/config` | `beadline.toml` schema, defaults, validation | bl-ya5.2 |
| `internal/load` | `bd export` JSONL (one per repo, or read live with `bd`) → graph | bl-ya5.2, bl-ya5.11 |
| `internal/graph` | issues and edges: descendants, topological order, cycles, critical chain (clean room) | bl-ya5.2, bl-ya5.4 |
| `internal/dist` | duration distributions (§2) | bl-ya5.3 |
| `internal/estimate` | classes, backoff chain, per-bead P50/P80, optional write-back | bl-ya5.3 |
| `internal/forecast` | Monte Carlo scheduling simulation | bl-ya5.4 |
| `internal/roadmap` | `roadmap.json` schema, the contract between forecast, render and calibrate | bl-ya5.5 |
| `internal/render` | single-file HTML (embedded template, CSS, JS) | bl-ya5.5 |
| `internal/calibrate` | snapshots, forecast vs actual, coverage, backtest | bl-ya5.6 |
| `internal/bvexec` | optional `bv` integration, through `os/exec` only | bl-ya5.7 |

- Everything lives under `internal/`, so v0.1 promises no Go API. The public contracts are the CLI,
  `beadline.toml` and `roadmap.json`.
- CLI exit codes: 0 for success, 1 for failure, 2 for a usage error.
- Randomised code takes an explicit `*rand.Rand` and never uses the global source. Each parallel
  simulation run gets its own source, seeded from (seed, run index), so results do not depend on
  goroutine scheduling. Tests use fixed seeds.
- `make check` is the gate. It runs a gofmt check, `go vet`, `go test ./...` and the clean-room check.
  CI (`.github/workflows/check.yml`) runs exactly `make check` on the Go version from go.mod.

## Decisions — ADR-2 (bl-ya5.10)
Status: **Accepted (Alex, 2026-09-19)**. It supersedes parts of ADR-1; each superseded passage above is
marked where it stands. Source: the bl-ya5 comments of 2026-09-19, which hold the mayor's investigation
and Alex's decisions D1–D3. The studies ran read-only on exports of the five town repos (1,233 beads)
taken that day. Implementation: bl-ya5.12 (model), bl-ya5.11 (CLI), bl-ya5.6 (`check` and the backtest).

### 1. Model: age-conditioned lead times (D1)
- **Decision.** The remaining time of every open bead is drawn conditioned on its age. ADR-1 did this
  for in-progress beads only. The Monte Carlo over the graph stays (Model §2); only the draw per bead
  changes.
- **Age conditioning.** A bead of age `a` draws `d` from its class with `d > a` and keeps `d − a`. Age
  runs from `started_at` for a bead in progress, otherwise from `ready_at`.
- **Lindy fallback.** When history cannot cover the age, so that every conditioned try is rejected,
  the bead is expected to wait about as long again. The reference prototype draws `a × e^Z` with
  `Z ~ N(0, 1)`, so the median remaining time equals the age. This replaces ADR-1's fresh unconditioned
  draw, and it is not clamped at the tail cap. A straggler that has waited 30 days is no longer
  forecast to take hours.
- **Right-censoring in v0.1.** Kaplan–Meier weights the fit. Beads still open in the window enter it
  as censored observations at their current age. The survival mass that KM leaves beyond the data
  becomes a tail past the longest observation. ADR-1 §2 had deferred this as a known bias.
- **Priority is the class key.** Class = repo × type × priority. Description-length size buckets are
  dropped because they do not order lead time. Priority does: the median lead time is 3 h at P0 and
  81 h at P3. The backoff chain and the `n / (n + k)` pooling of ADR-1 §2 are unchanged.
- **Timing.** Queue latency runs from `ready_at`, the later of `created_at` and the close of the last
  blocker, not from `created_at`: a blocked bead cannot be picked up. A bead with `started_at` learns
  and draws a queue (`ready_at → started_at`) and a cycle (`started_at → closed_at`). Most beads have
  no `started_at`: only 288 of the 1,233 carry one. The rest use one `ready_at → closed_at` lead time
  with no separate queue draw.
- **Concurrency.** It is still measured and shown, and an explicit agent count is still honoured. The
  measured cap no longer drives the dates, because waiting for an agent is already inside a measured
  lead time. In the backtest, removing the cap left ADR-1's epic coverage unchanged (P80 held 15%
  either way).
- **Rejected.** *Keep ADR-1 and let calibration widen its intervals.* To reach nominal coverage its
  P80 horizons would need stretching ×23 for leaf beads and ×13 for epics, and no interval adjustment
  recovers from that. ADR-2 needs ×1.1 and ×1. *Epic-duration baseline* (historical high-level
  durations conditioned on the item's age): its P80 held 50%.

### 2. A status instead of a fake date
- **Decision.** An item with no plannable work shows a status instead of a date. The statuses are
  unscoped (`not_planned`: nothing filed under it) and awaiting close (`ready_to_close`: all of its
  work closed, the item still open). The others are `deferred`, `blocked_outside` (it waits on a repo
  that is not loaded) and `stalled` (some of its work can never start). These are the statuses of
  roadmap.json schema v1, and the values keep their names, so the schema version does not change.
  `blocked_outside` and `stalled` keep their lower-bound dates (Simulation rules).
- **Why.** Over the backtest origins, only 55 of 292 open high-level (item, origin) snapshots had
  forecastable work. Of the rest, 216 had no children yet and 21 were shells whose children had all
  closed. Any date for these would be invented.

### 3. Scope of a high-level item
- **Decision.** The scope is the parent-child descendants UNION the upstream blocking closure: every
  bead that the item or its open work waits on, transitively, with that bead's descendants. Blocking
  means `blocks`, plus `conditional-blocks` and `waits-for`, which the loader also treats as blocking.
  `tracks`, `related`, `relates-to` and `discovered-from` are ignored for scheduling. `graph.Scope`
  (bl-ya5.5) already implements this rule.

### 4. Evidence
A rolling-origin backtest ran with 32 origins every 3.5 days from 2026-05-25 to 2026-09-11. Each fit
used only data from before its origin. "Held" means the bead or item closed on or before that
quantile's date. ADR-2 was scored as the reference prototype (`bl_model.py`, model `lead_km`), not
the Go code. The prototype measured lead time from `created_at`; `ready_at` (§1) is a refinement that
the release gate (§6) re-measures. The archive, with scripts, tables in `results/*.out` and exports,
is at `/home/ubuntu/gc/.gc/agents/mayor/beadline-forward-test-2026-09-19/`.

**Leaf beads.** 546 forecasts of 139 beads.

| Model | P50 held | P80 held | P80 90% CI (by bead) | P95 held | Bias of P50 | CRPS |
|---|---|---|---|---|---|---|
| ADR-1 as written | 11% | 25% | 20–33% | 41% | −8.0 d | 12.3 d |
| ADR-2 | 45% | 79% | 72–86% | 90% | +1.4 d | 9.6 d |

**Epics and milestones.** 55 forecasts of 11 items; the target is the item's close.

| Model | P50 held | P80 held | P80 90% CI (by item) | Bias of P50 | CRPS |
|---|---|---|---|---|---|
| ADR-1 as written | 11% | 15% | 2–36% | −10.5 d | 11.9 d |
| ADR-2 | 55% | 81% | 63–94% | +4.1 d | 10.4 d |
| Baseline: epic duration | 16% | 50% | 36–68% | −5.7 d | 8.4 d |
| Baseline: throughput only | 11% | 18% | 10–35% | −11.8 d | 14.6 d |

- **Sample size.** Eleven items are too few to calibrate on, and the epic CI spans 31 points. That is
  why the release gate (§6) uses leaf beads.
- **Censoring.** Some forecasts could not be graded yet because the bead is still open and its P80
  date has not passed. That applies to 142 of the 546 leaf forecasts under ADR-2 and 13 of the 55 epic
  forecasts. The table grades only the known outcomes. If every ungraded forecast turns out held, or
  every one missed, ADR-2's leaf P80 lies between 58% and 84%, and its epic P80 between 62% and 85%.
- **CRPS.** Among the model variants, ADR-2 scores the lowest CRPS. The epic-duration baseline scores
  lower still, because its distribution is sharp around dates that hold only half the time.
- **Retro-forecast.** A separate study replayed 5 closed milestones. ADR-1's P80 held 2 of 10 times.
- **Ablation.** Each change is applied alone unless stated. The leaf rows start from ADR-1's 25%.

  | Change | P80 held | Effect |
  |---|---|---|
  | ADR-1 + age conditioning + Lindy fallback (leaf) | 72% | the main fix |
  | ADR-1 + age conditioning (leaf) | 38% | |
  | ADR-1 + Lindy fallback (leaf) | 26% | none alone |
  | ADR-1 + Kaplan–Meier (leaf) | 41% | |
  | ADR-1 + priority classes (leaf) | 25% | none alone |
  | ADR-1 without the concurrency cap (epics) | 15% → 15% | none |
  | ADR-2 + a sign-off lag (epics) | 81% → 81% | about 0.1 d |

### 5. CLI: one command (D2)
- **Decision.** The whole surface is:
  - `beadline [DIR|FILE ...]` forecasts, writes `roadmap.html` and `roadmap.json`, and records a
    snapshot (§6). A DIR is a repo with `.beads`, read live through `bd -C DIR --readonly export`. A
    FILE is a `bd export` JSONL file. One repo needs no config.
  - `beadline check` grades past forecasts, and `beadline check --backtest` replays history (§6).
  - `beadline doctor` validates the data. It is the loader's current `check`, renamed so that `check`
    means grading forecasts.
- **Config.** `repos` plus an `[expert]` table for everything else. This replaces the `[[repos]]`,
  `[conventions]` and `[model]` layout under Inputs.
- **Dropped.** `serve`.
- **Details.** Flags, defaults and wording are under "Command line and beadline.toml" (bl-ya5.11).
  The exit codes of ADR-1 §4 stay.
- **Why.** The usability study found that a first answer needed a config file plus three commands, and
  that the page showed about 23 concepts. The target is one command and about 7 concepts.

### 6. Accuracy: snapshots, grading and the release gate (D3)
- **Snapshot format `beadline.snapshot/v1`.** roadmap.json carries it. Every run also writes an
  immutable copy under `.beadline/snapshots/<as_of>_<contenthash>.json`, create-only and deduplicated
  per day. It records the schema, `generated_at`, `as_of` (the data horizon), the model version (`adr-2`),
  the beadline git commit, the seed, the resolved config with its hash, and the SHA-256 of each input
  (with the bd/Dolt commit when known). Per item it records the id, title and status, a quantile grid
  from p05 to p99 of completion dates, the descendant ids, the remaining ids, the scope state and the
  assumptions. A content hash covers the whole snapshot. The grid enables PIT and CRPS. The id sets
  separate model error from scope change. The format is specified in the bl-ya5.5 notes.
- **Predict now, score later.** `beadline check` grades every stored snapshot against current data.
  A forecast is resolved, open or void. Open is right-censored: of an open forecast, only "P80
  already missed" can be known. Void means descoped or vanished. Coverage of P50/P80/P95 is reported
  on known outcomes plus bounds, next to median absolute error, signed bias, and CRPS or pinball loss.
  Leaf beads come first, as the honest sample, and high-level items second.
- **Rolling-origin backtest.** `beadline check --backtest` replays history from one export. Each fit
  uses only data from before its origin, and a throughput baseline is scored alongside.
- **Release gate.** v0.1 ships only when the leaf-bead backtest on our own history holds P80 within
  70–90%. `check --backtest` prints PASS or FAIL for it. The gate is a band, not a floor: below 70% the
  dates are optimistic, and above 90% the intervals are too wide to plan with. It is judged on leaf
  beads because they give hundreds of forecasts where epics give eleven items. The model is not tuned
  on the test origins (bl-ya5.12).
- **Track record.** The roadmap page shows the track record ("80% dates held X of Y") under the dates.
- **Live forward test.** `snapshots_live/` in the archive holds 8 open items. ADR-1 and the ADR-2
  prototype forecast them at 2026-09-19T07:09Z. They are scored on 2026-10-03 and 2026-10-31.

### 7. Open question: wave dispatch and scope growth (recorded, not decided)
The two studies disagree.
- **The complex-projects study** finds large scope growth. By 25% of an item's life, the remaining work
  that eventually lands is about twice what was filed (2.0×, pooled over the 15 closed milestones and
  epics). Items decomposed at creation grow 1.00×, while items filled in progressively grow 2.46×.
  Work also arrives in waves, dispatched after idle gaps of 5–22 days.
- **The backtest** finds that scope growth is not the main miss. Under ADR-1, forecasts for epics
  whose scope did not grow held P80 8% of the time, against 22% for those that grew. Under ADR-2 the
  figures are 88% and 69%.
- **Where they agree.** Stragglers that were closed by a decision cause the largest errors.

ADR-2 adds no growth or wave model. Revisit after the live forward test is scored on 2026-10-03.
