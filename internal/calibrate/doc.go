// Package calibrate measures how forecasts hold up (docs/design.md,
// "Calibration"). It records each forecast as an immutable snapshot, grades
// snapshots against what closed since, and replays history in a
// rolling-origin backtest that forecasts each past moment from the data
// visible then. Leaf beads are the honest sample: there are many of them.
// High-level items come second, with scope changes told apart.
package calibrate
