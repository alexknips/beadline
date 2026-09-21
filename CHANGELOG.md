# Changelog

## Unreleased

**Idle-time mask (bead `bl-dmd`).** `expert.idle` declares known idle windows (a host migration, a
recorded pool suspension); they mask out of every learned duration and every open bead's age
exactly, no inference needed. `expert.idle_gap_hours` can additionally infer idle time from a gap
in the cross-repo activity timeline, but defaults to **0 (off)**: a real-data backtest of the five
town repos found no gap threshold, under either calendar-mapping assumption, that both held the
release gate and moved a stuck item's forecast the right way — the town's real activity is bursty
enough (0.30-0.68 measured duty cycle) that a uniform stretch back to calendar time over- or
under-corrects. `roadmap.json` always reports what was masked (`idle`), so a declared window's
effect, or the fact that nothing was masked, is visible either way. Full evidence and the escalated
decision are in [`docs/design.md`](docs/design.md#decisions--adr-3-bl-dmd) (ADR-3).

## v0.1 — 2026-09-20

The whole surface: one command, `beadline [DIR|FILE...]`, plus `check` and `doctor`. Reads one or more
beads repos live (or their `bd export` files), learns lead times per class from your closed and open
beads, and Monte-Carlos the remaining graph into a roadmap: `roadmap.html` (single file, zero network
requests) and `roadmap.json`, for your milestones, epics and cross-repo goals — P50/P80 completion
dates, never a typed estimate.

**Model.** Age-conditioned lead times with Kaplan–Meier censoring and a Lindy fallback for stragglers
(ADR-2 §1); priority as the class key; humans modelled as a separate gate wait. See
[`docs/model.md`](docs/model.md).

**Accuracy, honestly.** Every run records an immutable snapshot; `beadline check` grades it against
what closed since, and `beadline check --backtest SPAN` replays history. v0.1 does not ship until the
leaf-bead backtest holds its 80% date 70-90% of the time (the release gate) — it does, on the town's
own history (1,263 beads, 5 repos, rolling-origin backtest, `beadline check --backtest`, measured
2026-09-19 on commit `b3175ea`, after the Kaplan–Meier tail fix in bead `bl-ya5.13`):

| Span | Leaf P50 held | Leaf P80 held | Gate |
|---|---|---|---|
| 118 days (32 origins, 129 leaf beads) | 42% | 75% (305/405) | PASS |
| 60 days (16 origins, 51 leaf beads) | 50% | 89% (182/204) | PASS |

High-level items (milestones/epics, 118-day span, 12 items — too few to calibrate on, indicative only):
P80 held 87% (CI 69-100%).

**Caveats.**
- **The 50/50 date is close to calibrated**: leaf P50 held 50% over 60 days and 42% over 118 days —
  still a bit early on the longer span, but no longer the 34-36% seen before the `bl-ya5.13` fix. Plan
  around the 80% date; the 60-day P80 (89%) sits just under the gate's 90% ceiling, so it runs
  slightly conservative there.
- **High-level coverage is indicative**, not tuned: 7-12 items is far short of the ~20 needed to
  calibrate on.
- Full evidence, including what the `bl-ya5.13` fix changed and the earlier ablation of what fixed
  the model (age conditioning plus the Lindy fallback, mainly) and what didn't (the concurrency cap,
  priority classes alone), is in [`docs/design.md`](docs/design.md#4-evidence).

**Not in v0.1.** Optional `bv` (beads_viewer) CLI integration (bead `bl-ya5.7`) is not built — its
license rider forbids automated-pipeline use, which building and testing it here would be.
