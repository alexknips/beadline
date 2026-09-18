// Package bvexec optionally runs an installed beads_viewer (bv) binary as an
// external process and reads its robot JSON output.
//
// It must only ever use os/exec. beads_viewer is licensed with a rider that
// is incompatible with beadline; never import, vendor or bundle its code.
package bvexec
