# beadline

**Roadmaps and delivery forecasts for [beads](https://github.com/gastownhall/beads) — built on the
assumption that the work is done by AI agents.**

beadline reads one or more beads databases, learns how long your agents actually take, simulates the
remaining dependency graph, and renders a roadmap of your **high-level** beads (milestones, epics,
goals) with P50/P80 completion dates — without anyone typing an estimate.

> Status: **design phase**. beadline is being built in the open by AI agents orchestrated with
> [Gas City](https://github.com/gastownhall/gastown); the plan lives in this repo's own beads.

## Why
The beads ecosystem has excellent viewers, boards and Gantt renderers. What none of them answer is
*"when will this milestone be done, given how our agents really perform?"* Existing timelines show dates
someone typed (`due_at`, `[Q1]` prefixes) or per-issue effort guesses scaled for humans. beadline
measures instead:

- **Per-bead estimates are learned, never typed.** Cycle-time distributions per class (repo × type × size
  proxy) from your closed beads. They are inputs; the roadmap never shows them.
- **Forecasts are simulations.** Monte Carlo over each milestone's remaining descendants, honouring
  dependency order, measured queue latency and your agents' real concurrency. Fat tails included.
- **Humans are modelled as gates.** Beads that wait on a person (configurable patterns) add a separate
  "waiting on a human" term — shown separately, because that is usually where the date moves.
- **It calibrates itself.** Every closed bead compares forecast to actual; the page shows how often
  P80 held.
- **High-level only.** Milestones, epics and cross-repo goals are what you see; everything below is math.

## How it will work
```
beadline doctor   --config beadline.toml        # load the exports; report cycles and dangling dependencies
beadline forecast --config beadline.toml        # -> roadmap.json (+ optional estimated_minutes write-back via bd)
beadline forecast --record                      # also record today's forecast as a snapshot
beadline check                                  # grade recorded forecasts against what closed since
beadline check    --backtest 60d                # replay history: how often did P80 hold?
beadline render   --out roadmap.html            # -> single-file static HTML, GitHub-Pages-able
beadline serve                                  # local preview
```
Inputs: `bd export` JSONL per repository (works with every beads backend), or live `bd`. Multi-repo
by config. Conventions (which types are "high level", which label links a goal to work across repos,
what marks a human gate, concurrency per repo) are configuration, not assumptions.

See [`docs/design.md`](docs/design.md) for the model and the open decisions.

## Contributing
Issues and PRs welcome. This project tracks its own work in beads (`.beads/`); `make check` is the
gate. MIT licensed.
