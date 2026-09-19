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
