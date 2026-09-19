package estimate

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"os/exec"
	"strconv"
	"strings"
)

// P80MetadataKey is the bead metadata key the write-back stores the P80 in.
// The P50 goes to bd's own estimated_minutes.
const P80MetadataKey = "ai_est_p80_minutes"

// Update is one write-back to one bead: estimated_minutes and the P80
// metadata, in whole minutes.
type Update struct {
	Repo, ID            string
	Minutes, P80Minutes int
}

// Args returns the bd arguments that apply the update.
func (u Update) Args() []string {
	return []string{
		"update", u.ID,
		"--estimate", strconv.Itoa(u.Minutes),
		"--set-metadata", P80MetadataKey + "=" + strconv.Itoa(u.P80Minutes),
	}
}

// PlanWriteBack returns the updates that give every open bead its cycle-time
// P50 and P80, rounded up to whole minutes. Beads that already carry those
// numbers are left out, so a repeated run writes nothing: each bd update is a
// commit in the beads database.
//
// The write-back is the only write beadline makes, and it is never on by
// default: callers plan and apply it only when the user asks with an
// explicit command-line flag.
func (m *Model) PlanWriteBack(beads []Bead) []Update {
	var out []Update
	for _, b := range beads {
		if b.closed() {
			continue
		}
		q := m.cycle.node(ClassOf(b)).q
		u := Update{Repo: b.Repo, ID: b.ID, Minutes: wholeMinutes(q.P50), P80Minutes: wholeMinutes(q.P80)}
		if u.Minutes != b.EstimatedMinutes || u.P80Minutes != b.P80Minutes {
			out = append(out, u)
		}
	}
	return out
}

func wholeMinutes(m float64) int { return max(1, int(math.Ceil(m))) }

// Runner runs bd with args against the beads database of repository repo.
type Runner func(ctx context.Context, repo string, args []string) error

// BdRunner returns a Runner that runs the bd binary (a name looked up in
// PATH, or a path) in each repository's directory, as given by dirs.
func BdRunner(bd string, dirs map[string]string) Runner {
	return func(ctx context.Context, repo string, args []string) error {
		dir, ok := dirs[repo]
		if !ok {
			return fmt.Errorf("no directory for repository %q", repo)
		}
		cmd := exec.CommandContext(ctx, bd, args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("%s %s (in %s): %w: %s", bd, strings.Join(args, " "), dir, err, bytes.TrimSpace(out))
		}
		return nil
	}
}

// WriteBack applies the updates in order. A failed update does not stop the
// others; every failure is returned. It stops early only when ctx is done.
func WriteBack(ctx context.Context, updates []Update, run Runner) error {
	var errs []error
	for _, u := range updates {
		if err := ctx.Err(); err != nil {
			return errors.Join(append(errs, err)...)
		}
		if err := run(ctx, u.Repo, u.Args()); err != nil {
			errs = append(errs, fmt.Errorf("write-back %s: %w", u.ID, err))
		}
	}
	return errors.Join(errs...)
}
