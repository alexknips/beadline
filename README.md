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

- **Per-bead estimates are learned, never typed.** Lead-time distributions per class (repo × type ×
  priority), from when a bead became ready to its close, learned from your delivered beads with the
  ones still open counted too. Every open bead's estimate is conditioned on how long it has already
  waited. They are inputs; the roadmap never shows them.
- **Forecasts are simulations.** Monte Carlo over each milestone's remaining descendants, honouring
  dependency order. Fat tails included.
- **Humans are modelled as gates.** Beads that wait on a person (configurable patterns) add a separate
  "waiting on a human" term — shown separately, because that is usually where the date moves.
- **It calibrates itself.** Every closed bead compares forecast to actual; the page shows how often
  P80 held.
- **High-level only.** Milestones, epics and cross-repo goals are what you see; everything below is math.

## How it works
```
beadline                                   # in a repo with .beads: roadmap.html + roadmap.json in .
beadline ../api ../web exports/hq.jsonl    # several repos, read live with bd, or bd export files
beadline --explain api-m1                  # what one milestone's dates rest on
beadline check                             # grade past forecasts against what closed since
beadline check --backtest 60d              # replay history: how often did the 80% date hold?
beadline doctor                            # check the data: cycles, dangling dependencies, duplicates
```
Every run forecasts, writes a single-file static `roadmap.html` (GitHub-Pages-able) and
`roadmap.json`, prints each milestone as "plan for <date> (80% chance) · 50/50: <date>", and records
a snapshot that `beadline check` grades later. One repo needs no config; several, or a changed
default, take a `beadline.toml` of `repos = [...]` plus an `[expert]` table. Conventions (which types
are "high level", which label links a goal to work across repos, what marks a human gate, agents per
repo) are configuration, not assumptions. `beadline help` lists every flag.

See [`docs/design.md`](docs/design.md) for the model and the open decisions.

## Contributing
Issues and PRs welcome. This project tracks its own work in beads (`.beads/`); `make check` is the
gate. MIT licensed.
