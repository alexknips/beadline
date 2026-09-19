# beadline

**Roadmaps and delivery forecasts for [beads](https://github.com/gastownhall/beads) — built on the
assumption that the work is done by AI agents.**

beadline reads one or more beads databases, learns how long your agents actually take, simulates the
remaining dependency graph, and renders a roadmap of your **high-level** beads (milestones, epics,
goals) with P50/P80 completion dates — without anyone typing an estimate.

> Status: **implemented, gate-passed, pre-release.** The model, the one-command CLI and the accuracy
> loop below are all on `main`, and the release gate passes on the town's own history — see
> [Accuracy](#accuracy). beadline is built in the open by AI agents orchestrated with
> [Gas City](https://github.com/gastownhall/gastown); the plan lives in this repo's own beads.

## Quickstart

```
beadline ~/code/repo
```

That's the whole thing. It reads `~/code/repo`'s `.beads` live, forecasts, and writes `roadmap.html`
(open it in a browser) and `roadmap.json` next to it — plus a line per milestone, epic and goal like
"plan for 2026-10-02 (80% chance) · 50/50: 2026-09-28 · 3 of 11 left". A repo with `.beads` in the
current directory needs no path at all. `beadline help` lists every flag.

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
- **High-level only.** Milestones, epics and cross-repo goals are what you see; everything below is
  math.

## Reading the dates
Every milestone, epic and goal gets two dates, plus how much work is left and whether anything is
waiting on a person:

- **`plan for <date> (80% chance)` is the date to plan around.** It is the date beadline expects 4
  forecasts in 5 to hold or beat — the one the [release gate](#accuracy) is measured against.
- **`50/50: <date>` is the coin-flip date** — as many similar forecasts should land after it as
  before. It's close to calibrated now (see [Accuracy](#accuracy)): 50% over 60 days, a bit early at
  42% over 118 days — treat it as a plausible date, not a guaranteed one.

Both, plus the full quantile grid, are in `roadmap.json`; `beadline --explain <id>` prints what one
item's dates rest on — its scope, the critical chain, and the work still to land.

## Accuracy
Every run of `beadline` records an immutable snapshot of its forecast. `beadline check` later grades
each one against what actually closed, and `beadline check --backtest 60d` replays history to test the
model before you trust it — both report coverage (how often each quantile held), bias, and a release
gate: **v0.1 does not ship until the leaf-bead backtest holds its 80% date 70-90% of the time.**

On the town's own history (1,263 beads across 5 repos, rolling-origin backtest, measured 2026-09-19 on
commit `b3175ea`, after the Kaplan–Meier tail fix in bead `bl-ya5.13`):

| Span | Leaf P50 held | Leaf P80 held | Gate |
|---|---|---|---|
| 118 days (32 origins, 129 leaf beads) | 42% | 75% (305/405) | PASS |
| 60 days (16 origins, 51 leaf beads) | 50% | 89% (182/204) | PASS |

High-level items (milestones/epics, 118-day span, 12 items): P80 held 87%.

**Caveats, honestly:** the 50/50 date is close to calibrated now — 50% held over 60 days, 42% over
118 days, still a bit early on the longer span. Plan around the 80% date; the 60-day P80 (89%) sits
just under the gate's 90% ceiling, so it runs slightly conservative there. High-level coverage above
is indicative only — 7-12 items is far short of the ~20 needed to calibrate on; trust the leaf-bead
numbers. The full study, including the `bl-ya5.13` fix and what it changed, is in
[`docs/design.md`](docs/design.md#4-evidence); release-to-release numbers are in
[`CHANGELOG.md`](CHANGELOG.md).

## Privacy note
`roadmap.html` is self-contained and needs no server, which means it embeds every scheduled bead's
title — in the timeline's hover details, the table, and the raw `roadmap-data` JSON it carries. If your
bead titles are sensitive, don't publish it somewhere those titles shouldn't be seen, including a public
GitHub Pages site. Use `[expert] hide` (see [`docs/model.md`](docs/model.md)) to leave specific items
off the roadmap, or serve the page somewhere access-controlled instead.

## Getting forecast-friendly beads
The model learns from how your beads actually behave, so a few habits make its forecasts sharper:

- **Decompose milestones at creation**, not as you go. Milestones filed as one shot before work
  started grew to about 1.00× their filed scope by a quarter of the way through; milestones filled in
  progressively grew to 2.46×. Scope that shows up late looks like a miss beadline couldn't have seen
  coming.
- **Close stragglers instead of letting them ride.** A handful of beads left open long past their
  natural close, then closed by a decision rather than delivery, cause the largest single errors in the
  backtest.
- **Mark non-delivering closes.** Set `gc.work_outcome` to `no-op` or `abandoned`, or use a close
  reason that says so (`duplicate`, `superseded`, `won't fix`, `not needed`, `descoped`, …). beadline
  already excludes these from both training and grading — but only the ones it can recognise; an
  unmarked non-delivery teaches the model a false short cycle time.

## Configuration
One repo with `.beads` needs nothing. Several repos, or a changed default — which types count as "high
level", what marks a human gate, agents per repo, or the model's own knobs — take a `beadline.toml`.
See [`examples/beadline.toml`](examples/beadline.toml) for a worked multi-repo example and
[`docs/model.md`](docs/model.md) for what each `[expert]` key does and how the model behind the dates
works.

## Keeping a roadmap current
A roadmap is worth most when it's fresh. [`examples/github-workflows/roadmap.yml`](examples/github-workflows/roadmap.yml)
is a copyable GitHub Actions workflow: it installs `beadline` and `bd`, forecasts, and publishes
`roadmap.html` to GitHub Pages on a schedule. The same recipe works with any scheduler — cron, a
systemd timer, … — run `beadline` against your repos and serve the resulting `roadmap.html` however
you serve static files.

## Relationship to beads_viewer
beadline takes no code from `bv` ([Dicklesworthstone/beads_viewer](https://github.com/Dicklesworthstone/beads_viewer)):
its license rider is incompatible with beadline's plain MIT license and with how beadline is built (by
AI agents). See [`docs/design.md`](docs/design.md#relationship-to-beads_viewer-bv) for the clean-room
policy this project holds to.

## Contributing
Issues and PRs welcome. This project tracks its own work in beads (`.beads/`); `make check` is the
gate. MIT licensed.
