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
  later: live `bd … --json` and a Dolt reader.
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
  key is an error, so a typo cannot silently fall back to a default.

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
  check` prints them. It exits 1 only for a cycle among open beads, since no schedule can satisfy
  one. Malformed JSON, by contrast, is fatal, and the error names the file and line.

## Model
### 1. Estimator (per bead, invisible)
- Class = (repo, issue_type, size bucket). Size proxy: description length, child count, labels. A
  `size:s|m|l` label decides (`xs`, `xl` count as `s`, `l`). Otherwise a bead is **L** with a
  description of 1,500+ characters or 3+ children, **M** with 400+ characters or any child, else **S**.
- From closed beads in `window_days`: empirical distribution of **cycle time** (`started_at → closed_at`,
  fallback `created_at → closed_at`) and **queue latency** (`created_at → started_at`). Only work
  beads count: the caller leaves out infra and high-level types, whose durations span other work.
  Queue latency has no prior of its own and uses `cycle_minutes_prior`.
- Unseen class → repo prior → global prior. Fat tails are kept, never `median × count`: samples come
  from a smoothed bootstrap with hierarchical backoff and a tail cap (ADR-1 §2).
- Optional write-back, off unless a command-line flag asks for it: `estimated_minutes` (P50) and
  `ai_est_p80_minutes` of the full cycle time on open beads via `bd update`, so other tools
  (e.g. `bv --robot-capacity`) get agent-scale numbers. Beads that already carry the same numbers are
  skipped, so a repeated run writes nothing.

### 2. Forecaster (per high-level bead)
- Collect remaining descendants via parent-child edges; blocking edges define order.
- Monte Carlo: for each run, agents pull ready beads (respecting `concurrency` per repo), each bead
  waits its sampled queue latency, then its sampled cycle time; children become ready when blockers
  close. Milestone finish = last descendant closed.
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
  were in progress at once in the window (`started_at` to `closed_at`), at least 1.
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

### 3. Calibration
- Store each forecast snapshot. When a bead/milestone closes, record forecast-vs-actual. Report
  coverage ("P80 held 78% of the time") and use it to widen/narrow intervals.

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
  `inputs.exports[]` gives each export's repo, path, SHA-256 and size, and `inputs.fingerprint`
  hashes those in config order. Same fingerprint, config and seed give the same roadmap.
- **`repos[]`** are the swimlanes, in config order. Each has bead counts, `concurrency` and
  `concurrency_source` (`configured` or `measured`), and `rate_per_day` once the forecaster measures
  it.
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
- **`calibration`** holds `samples`, `p50_coverage` and `p80_coverage`. It is written by calibrate
  (bl-ya5.6) and absent until then.

`roadmap.Build` fills everything the graph alone determines. The forecaster then sets the forecast
fields (and `stalled`) and calls `Assess` again.

### roadmap.html (bl-ya5.5)
`beadline render --in roadmap.json --out roadmap.html [--title ...]` writes one file, replacing the
old one atomically so a web server never serves half a page. The page follows ADR-1 §3:
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
- **Self-contained.** The page has no `src` or `href` attributes and no links. It makes no network
  requests. A `Content-Security-Policy` of `default-src 'none'` allows only the page's own inline
  style and script, by SHA-256. So the page works from `file://`, from any directory of a static
  server, and next to other pages. `roadmap.json` is embedded as `<script type="application/json"
  id="roadmap-data">`.
- **Script.** Under 10 KB and optional. It adds a timeline/table toggle, a repo filter that re-stacks
  the lanes (goal rows follow their members' repos), a theme toggle (auto, light or dark) and the
  popover. It saves preferences in `localStorage` when that is available. Without it, both views
  show and the theme follows the system.

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
these decisions.

### 1. Language and distribution: Go, one static binary
- **Decision.** Go (go.mod pins the minimum, currently 1.22), built with `CGO_ENABLED=0`. Users install
  it with `go install github.com/alexknips/beadline/cmd/beadline@latest` or download a release binary.
  Like `bd`, there is no runtime to install.
- **Why.** The standard library covers all of v0.1. `encoding/json` reads the JSONL exports.
  `html/template` with `embed` builds the single-file page and escapes untrusted bead titles by context.
  `net/http` backs `serve`, and `os/exec` runs live `bd` and the optional `bv`. `math/rand/v2` gives a
  seeded PCG, so a forecast is reproducible. Goroutines run simulations in parallel. 2,000 runs over a
  few thousand beads is a plain CPU loop that Go finishes in well under a second. For statistics we
  need sampling, quantiles and a bandwidth rule, roughly 150 lines, so scipy is not needed.
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
  10 observations is half its own, and one with 100 is 91 % its own.
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
- **Write-back numbers.** P50/P80 of 4,000 draws with the configured seed, from the same sampler the
  simulation uses, so the two always agree.
- **Rejected.** *Pure empirical bootstrap*: it can never exceed the observed maximum, its percentiles
  are stepped, and it has nothing to offer a thin class. *Fitted log-normal*: it forces one mode onto
  data that is often bimodal, the fit is sensitive to outliers, and the tail cap ends up doing all the
  work. The log-normal survives only as the root prior.
- **Known bias.** Beads still open are missing from the closed-bead window (right-censoring), which
  makes estimates optimistic. v0.1 accepts this. Calibration (bl-ya5.6) measures coverage and widens
  intervals. A Kaplan–Meier correction follows if coverage stays low.

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
| `internal/cli` | subcommands (`check`, `forecast`, `render`, `serve`, `version`), flags, exit codes | bl-ya5.1, then each feature bead |
| `internal/config` | `beadline.toml` schema, defaults, validation | bl-ya5.2 |
| `internal/load` | `bd export` JSONL (one per repo) → graph | bl-ya5.2 |
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
