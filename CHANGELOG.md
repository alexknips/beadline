# Changelog

## v0.1 (unreleased)

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
own history (1,256 beads, 5 repos, rolling-origin backtest, `beadline check --backtest`):

| Span | Leaf P50 held | Leaf P80 held | Gate |
|---|---|---|---|
| 118 days (32 origins, 129 leaf beads) | 36% | 78% (312/398) | PASS |
| 60 days (16 origins, 51 leaf beads) | 34% | 90% (181/202) | PASS |

High-level items (milestones/epics, 118-day span, 12 items — too few to calibrate on, indicative only):
P80 held 85% (CI 64-97%).

**Caveats.**
- **The 50/50 date runs early**: leaf P50 held only 34-36%, not 50% — a follow-up (bead `bl-ya5.13`)
  tracks a fix. Plan around the 80% date, which is well calibrated; read 50/50 as an optimistic floor.
- **High-level coverage is indicative**, not tuned: 7-12 items is far short of the ~20 needed to
  calibrate on.
- Full evidence, including the ablation of what actually fixed the model (age conditioning plus the
  Lindy fallback, mainly) and what didn't (the concurrency cap, priority classes alone), is in
  [`docs/design.md`](docs/design.md#4-evidence).

**Not in v0.1.** Optional `bv` (beads_viewer) CLI integration (bead `bl-ya5.7`) is not built — its
license rider forbids automated-pipeline use, which building and testing it here would be.
