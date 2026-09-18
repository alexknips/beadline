// Package dist represents duration distributions (cycle time, queue latency,
// human-gate latency): a smoothed bootstrap in log space with hierarchical
// backoff to a log-normal root prior and a tail cap. See docs/design.md,
// ADR-1 §2.
package dist
