// Package graph holds issues and their parent-child and blocking edges, and
// the algorithms beadline needs on them: descendants, topological order,
// cycle detection and the critical chain.
//
// Clean room: implemented from first principles. Do not copy or closely
// paraphrase beads_viewer source (see docs/design.md).
package graph
