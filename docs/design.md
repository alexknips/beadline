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
  human_gate_hours_prior = 24
  ```

## Model
### 1. Estimator (per bead, invisible)
- Class = (repo, issue_type, size bucket). Size proxy: description length, child count, labels.
- From closed beads in `window_days`: empirical distribution of **cycle time** (`started_at → closed_at`,
  fallback `created_at → closed_at`) and **queue latency** (`created_at → started_at`).
- Unseen class → repo prior → global prior. Fat tails are kept (sample from the empirical distribution
  or a fitted log-normal with tail cap), never `median × count`.
- Optional write-back: `estimated_minutes` (P50) and `ai_est_p80_minutes` on open beads via `bd update`,
  so other tools (e.g. `bv --robot-capacity`) get agent-scale numbers.

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

## Open decisions (first design bead)
- Language/distribution: Go single binary (ecosystem norm: bd, bv) vs Python vs TypeScript.
- Distribution fitting: empirical bootstrap vs log-normal; tail handling.
- UI stack for the single-file HTML.
