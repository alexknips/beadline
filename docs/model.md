# The model

This is what `beadline` actually computes, and every `[expert]` key that changes it. If you just want
dates, you don't need this file — `beadline ~/code/repo` needs no configuration. Read it when you want
to know what's behind a date, or a roadmap looks wrong for your data. The full design history,
including the accepted ADRs and the evidence behind them, is in [`design.md`](design.md).

## The idea in three steps

1. **Learn how long things take, per bead, without anyone typing an estimate.** Every closed bead
   teaches a lead time (from when it became unblocked, `ready_at`, to `closed_at`) for its class: its
   repo, issue type and priority. Beads still open teach too — an open bead of a given age is evidence
   that its lead time exceeds that age (right-censoring, handled with Kaplan–Meier), not left out until
   it closes. A class with little history borrows from its repo, then from every repo, then from a
   broad prior — more of its own data the more it has (`pooling_strength` below).
2. **Draw a plausible future for every open bead, all at once, thousands of times.** Each simulated run
   schedules every open bead across every configured repo — respecting dependencies, and each open
   bead's own age, so a straggler that's already waited 30 days isn't forecast to land in an hour. A
   milestone's date, in that run, is whenever its last remaining bead finishes. 2,000 runs (default)
   give a distribution of finish dates per milestone, epic and cross-repo goal — not one number.
3. **Report the finish-date distribution as two headline dates, and check itself.** "80% chance" is the
   quantile 4 runs in 5 beat; "50/50" is the median run. Every forecast is recorded as an immutable
   snapshot; `beadline check` later grades it against what actually closed, and `beadline check
   --backtest SPAN` replays history to test the model before you trust it at all. See the README's
   [Accuracy](../README.md#accuracy) section for what that grading currently shows.

Humans are modelled separately: a bead matching a gate pattern (a title like `^HUMAN:`, or a metadata
key such as `awaiting_signoff`) waits a learned "human latency" before it can start, on top of — not
instead of — its own lead time. That wait is reported apart from agent time, because it's usually where
a date actually moves.

## Configuration (`[expert]`)

One repo with `.beads` needs no file at all. Everything below is optional, in `beadline.toml`:

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

The values shown are the defaults. An unknown key is an error that names the closest valid one (`did
you mean expert.agents?`), so a typo never falls back to a default silently.

| Key | What it changes |
|---|---|
| `agents` | Agents per repo working at once. `"measure"` (default) learns it from how many beads were `in_progress` at once in the window; a fixed number sets a hard concurrency limit in the simulation. Either way the *dates* mostly don't move — a measured limit is already inside each bead's learned lead time. |
| `roadmap_types` | Which issue types are "high level" and get a row on the roadmap. Everything else is scheduled but never shown on its own. |
| `goal_label` | The regexp (with one capture group) that links a bead's label to a cross-repo goal, e.g. `goal:hc-4za` names goal `hc-4za`. A goal's forecast is the latest of its members'. |
| `human_gate.titles` / `.metadata` | How a human-waiting bead is recognised: a title pattern (`^HUMAN:`), or a metadata key set to anything but `""`, `false`, `0` or `null`. |
| `human_gate.hours_prior` | The median human-gate wait assumed before any gates have closed to learn from. |
| `hide` | Bead and goal IDs to leave off the roadmap entirely (still scheduled, since other work may depend on them). |
| `history_days` | How far back closed beads are read to learn durations. |
| `ignore_types` | Issue types that are infrastructure, not work — dropped from the graph entirely, with every edge to them. |
| `runs` | Simulated schedules per forecast. More runs sharpen the tail quantiles (P95, P99) at the cost of time; 2,000 is enough for P50/P80 to be stable. |
| `seed` | The Monte Carlo seed. The same inputs and seed always give the same roadmap — useful for diffing a config change. |
| `cycle_minutes_prior` | The median *cycle* time assumed for a class with no history at all (the root of the backoff chain). The root lead-time prior is twice this — a queue plus a cycle. |
| `pooling_strength` | How fast a class trusts its own data over its parent's: `n / (n + pooling_strength)`. At the default (10), a class with 10 observations is half its own data, one with 100 is 91%. |
| `tail_cap_factor` | Every drawn duration is capped at this factor times the longest ever observed, so one wild draw can't set a milestone's whole date. |

`--seed`, `--runs` and `--agents REPO=N` on the command line override the file for one run; `--as-of
DATE` forecasts from the data as it stood at a past moment, rewinding it the same way the backtest
does.

## Known limits, honestly

- **The 50/50 date is close to calibrated, though still a bit early over longer spans.** After the
  Kaplan–Meier tail fix (bead `bl-ya5.13`), our own backtest holds it 50% of the time over 60 days and
  42% over 118 days (measured 2026-09-19, commit `b3175ea`) — up from 34-36% before the fix. The 80%
  date remains the one to plan around; the 60-day P80 (89%) sits just under the gate's 90% ceiling, so
  it runs a little conservative there.
- **High-level (milestone/epic/goal) coverage is indicative, not tuned.** The backtest has only 7-12
  distinct items to grade against, far short of the ~20 needed to calibrate on; trust the leaf-bead
  numbers, which have hundreds.
- **Scope growth isn't modelled.** A milestone decomposed progressively, not at creation, can grow well
  past what was filed when it was forecast (see "Getting forecast-friendly beads" in the README);
  beadline reports the scope it can see, not what a milestone will eventually become.
